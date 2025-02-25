package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Azure/azure-workload-identity/pkg/config"
	"github.com/Azure/go-autorest/autorest/azure"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=ignore,groups="",resources=pods,verbs=create;update,versions=v1,name=mutation.azure-workload-identity.io,sideEffects=None,admissionReviewVersions=v1;v1beta1,matchPolicy=Equivalent
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch

// this is required for the webhook server certs generated and rotated as part of cert-controller rotator
// +kubebuilder:rbac:groups="",namespace=azure-workload-identity-system,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;create;update;patch;delete

// podMutator mutates pod objects to add project service account token volume
type podMutator struct {
	client client.Client
	// reader is an instance of mgr.GetAPIReader that is configured to use the API server.
	// This should be used sparingly and only when the client does not fit the use case.
	reader             client.Reader
	config             *config.Config
	isARCCluster       bool
	decoder            *admission.Decoder
	audience           string
	azureAuthorityHost string
}

// NewPodMutator returns a pod mutation handler
func NewPodMutator(client client.Client, reader client.Reader, arcCluster bool, audience string) (admission.Handler, error) {
	c, err := config.ParseConfig()
	if err != nil {
		return nil, err
	}
	if audience == "" {
		audience = DefaultAudience
	}
	// this is used to configure the AZURE_AUTHORITY_HOST env var that's
	// used by the azure sdk
	azureAuthorityHost, err := getAzureAuthorityHost(c)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get AAD endpoint")
	}

	return &podMutator{
		client:             client,
		reader:             reader,
		config:             c,
		isARCCluster:       arcCluster,
		audience:           audience,
		azureAuthorityHost: azureAuthorityHost,
	}, nil
}

// PodMutator adds projected service account volume for incoming pods if service account is annotated
func (m *podMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}

	err := m.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// for daemonset/deployment pods the namespace field is not set in objectMeta
	// explicitly set the namespace to request namespace
	pod.Namespace = req.Namespace

	logger := log.Log.WithName("handler").WithValues("pod", pod.Name, "namespace", pod.Namespace, "service-account", pod.Spec.ServiceAccountName)
	// get service account associated with the pod
	serviceAccount := &corev1.ServiceAccount{}
	if err = m.client.Get(ctx, types.NamespacedName{Name: pod.Spec.ServiceAccountName, Namespace: pod.Namespace}, serviceAccount); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to get service account")
			return admission.Errored(http.StatusBadRequest, err)
		}
		// bypass cache and get from the API server as it's not found in cache
		err = m.reader.Get(ctx, types.NamespacedName{Name: pod.Spec.ServiceAccountName, Namespace: pod.Namespace}, serviceAccount)
		if err != nil {
			logger.Error(err, "failed to get service account")
			return admission.Errored(http.StatusBadRequest, err)
		}
	}

	// check if the service account has the annotation
	if !isServiceAccountAnnotated(serviceAccount) {
		logger.Info("service account not annotated")
		return admission.Allowed("service account not annotated")
	}
	// get service account token expiration
	serviceAccountTokenExpiration, err := getServiceAccountTokenExpiration(pod, serviceAccount)
	if err != nil {
		logger.Error(err, "failed to get service account token expiration")
		return admission.Errored(http.StatusBadRequest, err)
	}
	// get the clientID
	clientID := getClientID(serviceAccount)
	// get the tenantID
	tenantID := getTenantID(serviceAccount, m.config)
	// get containers to skip
	skipContainers := getSkipContainers(pod)
	pod.Spec.InitContainers = m.mutateContainers(pod.Spec.InitContainers, clientID, tenantID, skipContainers)
	pod.Spec.Containers = m.mutateContainers(pod.Spec.Containers, clientID, tenantID, skipContainers)

	if !m.isARCCluster {
		// add the projected service account token volume to the pod if not exists
		if err = addProjectedServiceAccountTokenVolume(pod, serviceAccountTokenExpiration, m.audience); err != nil {
			logger.Error(err, "failed to add projected service account volume")
			return admission.Errored(http.StatusBadRequest, err)
		}
	} else {
		// Kubernetes secret created by the arc jwt issuer will follow the naming convention
		// localtoken-<service account name>
		secretName := fmt.Sprintf("localtoken-%s", serviceAccount.Name)
		// add the secret volume for arc scenarios
		// TODO (aramase) Do we need to validate the k8s secret exists before adding the volume?
		if err = addProjectedSecretVolume(pod, m.config, secretName); err != nil {
			logger.Error(err, "failed to add projected secret volume")
			return admission.Errored(http.StatusBadRequest, err)
		}
	}

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		logger.Error(err, "failed to marshal pod object")
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// PodMutator implements admission.DecoderInjector
// A decoder will be automatically injected

// InjectDecoder injects the decoder
func (m *podMutator) InjectDecoder(d *admission.Decoder) error {
	m.decoder = d
	return nil
}

// mutateContainers mutates the containers by injecting the projected
// service account token volume and environment variables
func (m *podMutator) mutateContainers(containers []corev1.Container, clientID string, tenantID string, skipContainers map[string]struct{}) []corev1.Container {
	for i := range containers {
		// container is in the skip list
		if _, ok := skipContainers[containers[i].Name]; ok {
			continue
		}
		// add environment variables to container if not exists
		containers[i] = addEnvironmentVariables(containers[i], clientID, tenantID, m.azureAuthorityHost)
		// add the volume mount if not exists
		containers[i] = addProjectedTokenVolumeMount(containers[i])
	}
	return containers
}

// isServiceAccountAnnotated checks if the service account has been annotated
// to use with workload identity
func isServiceAccountAnnotated(sa *corev1.ServiceAccount) bool {
	if len(sa.Labels) == 0 {
		return false
	}
	_, ok := sa.Labels[UsePodIdentityLabel]
	return ok
}

// getSkipContainers gets the list of containers to skip based on the annotation
func getSkipContainers(pod *corev1.Pod) map[string]struct{} {
	skipContainers := pod.Annotations[SkipContainersAnnotation]
	if len(skipContainers) == 0 {
		return nil
	}
	skipContainersList := strings.Split(skipContainers, ";")
	m := make(map[string]struct{})
	for _, skipContainer := range skipContainersList {
		m[strings.TrimSpace(skipContainer)] = struct{}{}
	}
	return m
}

// getServiceAccountTokenExpiration returns the expiration seconds for the project service account token volume
// Order of preference:
// 	1. annotation in the pod
// 	2. annotation in the service account
//	default expiration if no annotation specified
func getServiceAccountTokenExpiration(pod *corev1.Pod, sa *corev1.ServiceAccount) (int64, error) {
	serviceAccountTokenExpiration := DefaultServiceAccountTokenExpiration
	var err error
	// check if expiry defined in the pod with annotation
	if pod.Annotations != nil && pod.Annotations[ServiceAccountTokenExpiryAnnotation] != "" {
		if serviceAccountTokenExpiration, err = strconv.ParseInt(pod.Annotations[ServiceAccountTokenExpiryAnnotation], 10, 64); err != nil {
			return 0, err
		}
	} else if sa.Annotations != nil && sa.Annotations[ServiceAccountTokenExpiryAnnotation] != "" {
		if serviceAccountTokenExpiration, err = strconv.ParseInt(sa.Annotations[ServiceAccountTokenExpiryAnnotation], 10, 64); err != nil {
			return 0, err
		}
	}
	// validate expiration time
	if !validServiceAccountTokenExpiry(serviceAccountTokenExpiration) {
		return 0, errors.Errorf("token expiration %d not valid. Expected value to be between 3600 and 86400", serviceAccountTokenExpiration)
	}
	return serviceAccountTokenExpiration, nil
}

func validServiceAccountTokenExpiry(tokenExpiry int64) bool {
	return tokenExpiry <= MaxServiceAccountTokenExpiration && tokenExpiry >= MinServiceAccountTokenExpiration
}

// getClientID returns the clientID to be configured
func getClientID(sa *corev1.ServiceAccount) string {
	return sa.Annotations[ClientIDAnnotation]
}

// getTenantID returns the tenantID to be configured
func getTenantID(sa *corev1.ServiceAccount, c *config.Config) string {
	// use tenantID if provided in the annotation
	if tenantID, ok := sa.Annotations[TenantIDAnnotation]; ok {
		return tenantID
	}
	// use the cluster tenantID as default value
	return c.TenantID
}

// addEnvironmentVariables adds the clientID, tenantID and token file path environment variables needed for SDK
func addEnvironmentVariables(container corev1.Container, clientID, tenantID, azureAuthorityHost string) corev1.Container {
	m := make(map[string]string)
	for _, env := range container.Env {
		m[env.Name] = env.Value
	}
	// add the clientID env var
	if _, ok := m[AzureClientIDEnvVar]; !ok {
		container.Env = append(container.Env, corev1.EnvVar{Name: AzureClientIDEnvVar, Value: clientID})
	}
	// add the tenantID env var
	if _, ok := m[AzureTenantIDEnvVar]; !ok {
		container.Env = append(container.Env, corev1.EnvVar{Name: AzureTenantIDEnvVar, Value: tenantID})
	}
	// add the token file env var
	if _, ok := m[AzureFederatedTokenFileEnvVar]; !ok {
		container.Env = append(container.Env, corev1.EnvVar{Name: AzureFederatedTokenFileEnvVar, Value: filepath.Join(TokenFileMountPath, TokenFilePathName)})
	}
	// add the azure authority host env var
	if _, ok := m[AzureAuthorityHostEnvVar]; !ok {
		container.Env = append(container.Env, corev1.EnvVar{Name: AzureAuthorityHostEnvVar, Value: azureAuthorityHost})
	}

	return container
}

// addProjectedTokenVolumeMount adds the projected token volume mount for the container
func addProjectedTokenVolumeMount(container corev1.Container) corev1.Container {
	for _, volume := range container.VolumeMounts {
		if volume.Name == TokenFilePathName {
			return container
		}
	}
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{
			Name:      TokenFilePathName,
			MountPath: TokenFileMountPath,
			ReadOnly:  true,
		})

	return container
}

func addProjectedServiceAccountTokenVolume(pod *corev1.Pod, serviceAccountTokenExpiration int64, audience string) error {
	// add the projected service account token volume to the pod if not exists
	for _, volume := range pod.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, pvs := range volume.Projected.Sources {
			if pvs.ServiceAccountToken == nil {
				continue
			}
			if pvs.ServiceAccountToken.Path == TokenFilePathName {
				return nil
			}
		}
	}

	// add the projected service account token volume
	// the path for this volume will always be set to "azure-identity-token"
	pod.Spec.Volumes = append(
		pod.Spec.Volumes,
		corev1.Volume{
			Name: TokenFilePathName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
								Path:              TokenFilePathName,
								ExpirationSeconds: &serviceAccountTokenExpiration,
								Audience:          audience,
							},
						},
					},
				},
			},
		})

	return nil
}

// addProjectedSecretVolume adds a projected volume with kubernetes secret as source for
// arc clusters. The controller will generate the JWT token and create kubernetes secret for
// the service account.
func addProjectedSecretVolume(pod *corev1.Pod, config *config.Config, secretName string) error {
	// add the projected secret volume to the pod if not exists
	for _, volume := range pod.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, pvs := range volume.Projected.Sources {
			if pvs.Secret == nil || pvs.Secret.Name != secretName {
				continue
			}
			for _, item := range pvs.Secret.Items {
				if item.Path == TokenFilePathName {
					return nil
				}
			}
		}
	}

	// add the projected secret volume
	// the path for this volume will always be set to "azure-identity-token"
	pod.Spec.Volumes = append(
		pod.Spec.Volumes,
		corev1.Volume{
			Name: TokenFilePathName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							Secret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: secretName,
								},
								Items: []corev1.KeyToPath{
									{
										Key:  "token",
										Path: TokenFilePathName,
									},
								},
							},
						},
					},
				},
			},
		})

	return nil
}

// getAzureAuthorityHost returns the active directory endpoint to use for requesting
// tokens based on the azure environment the webhook is configured with.
func getAzureAuthorityHost(c *config.Config) (string, error) {
	var env azure.Environment
	var err error
	if c.Cloud == "" {
		env = azure.PublicCloud
	} else {
		env, err = azure.EnvironmentFromName(c.Cloud)
	}
	return env.ActiveDirectoryEndpoint, err
}
