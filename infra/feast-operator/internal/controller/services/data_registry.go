/*
Copyright 2024 Feast Community.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package services

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// isDataRegistryEnabled returns true when the FeatureStore CR carries the
// catalog annotation set to "true". Follows the isProtectedProject() pattern.
func (feast *FeastServices) isDataRegistryEnabled() bool {
	annotations := feast.Handler.FeatureStore.GetAnnotations()
	return annotations[DataRegistryAnnotation] == "true"
}

// validateDataRegistryAnnotation checks for common annotation misconfigurations
// (e.g. "True", "yes", "1") and logs a warning so operators can spot typos.
func (feast *FeastServices) validateDataRegistryAnnotation() {
	logger := log.FromContext(feast.Handler.Context)
	annotations := feast.Handler.FeatureStore.GetAnnotations()
	val, exists := annotations[DataRegistryAnnotation]
	if !exists {
		return
	}
	if val == "true" {
		return
	}
	lower := strings.ToLower(val)
	if lower == "true" || val == "yes" || val == "1" {
		logger.Info("Data registry annotation has a non-canonical value; only exact \"true\" enables it",
			"annotation", DataRegistryAnnotation, "value", val, "hint", "use \"true\" (lowercase)")
	}
}

// validateDataRegistryNamespace is a secondary guard that ensures the namespace
// hosting the data-registry CR carries the required platform label. The primary
// enforcement is the exact namespace name match performed by the controller
// (comparing against dataRegistryNamespace from the capabilities ConfigMap,
// which defaults to rhoai-data-registry). Custom namespace selection is deferred.
func (feast *FeastServices) validateDataRegistryNamespace() error {
	ns := &corev1.Namespace{}
	nsName := feast.Handler.FeatureStore.Namespace
	if err := feast.Handler.Client.Get(
		feast.Handler.Context,
		types.NamespacedName{Name: nsName},
		ns,
	); err != nil {
		return fmt.Errorf("failed to read namespace %q for data-registry label check: %w", nsName, err)
	}
	if !namespaceDesignatedForDataRegistry(ns) {
		return fmt.Errorf(
			"namespace %q is not designated for the data registry "+
				"(missing required label %s=true or %s=true); "+
				"the data-registry FeatureStore CR must be created in the namespace "+
				"labeled by the platform operator (e.g. rhoai-data-registry)",
			nsName, DataRegistryNamespaceLabel, DataRegistryPlatformNamespaceLabel,
		)
	}
	return nil
}

func namespaceDesignatedForDataRegistry(ns *corev1.Namespace) bool {
	if ns == nil || ns.Labels == nil {
		return false
	}
	if ns.Labels[DataRegistryNamespaceLabel] == "true" {
		return true
	}
	return ns.Labels[DataRegistryPlatformNamespaceLabel] == "true"
}

// IsDataRegistryCREnabled reports whether the CR carries the canonical data-registry annotation.
func IsDataRegistryCREnabled(cr *feastdevv1.FeatureStore) bool {
	if cr == nil {
		return false
	}
	return cr.GetAnnotations()[DataRegistryAnnotation] == "true"
}

// validateDataRegistrySingleton ensures only one FeatureStore CR across the
// entire cluster has the data-registry annotation enabled. Uses
// metadata.creationTimestamp as a deterministic tiebreaker: the oldest
// annotated CR wins, all others are rejected. This eliminates the race
// window that existed with the old List+Lock approach because
// creationTimestamp is set by the API server and is always unique.
func (feast *FeastServices) validateDataRegistrySingleton() error {
	self := feast.Handler.FeatureStore
	var list feastdevv1.FeatureStoreList
	if err := feast.Handler.Client.List(feast.Handler.Context, &list); err != nil {
		return fmt.Errorf("failed to list FeatureStore CRs for singleton check: %w", err)
	}

	for i := range list.Items {
		item := &list.Items[i]
		if item.Namespace == self.Namespace && item.Name == self.Name {
			continue
		}
		if item.DeletionTimestamp != nil {
			continue
		}
		if item.Annotations[DataRegistryAnnotation] != "true" {
			continue
		}
		// Another annotated CR exists — oldest wins.
		if item.CreationTimestamp.Before(&self.CreationTimestamp) ||
			(item.CreationTimestamp.Equal(&self.CreationTimestamp) && item.Name < self.Name) {
			return fmt.Errorf(
				"data registry is already enabled on FeatureStore %s/%s (created at %s); "+
					"only one data-registry instance is allowed cluster-wide",
				item.Namespace, item.Name,
				item.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
			)
		}
	}
	return nil
}

// deployDataRegistry creates (or updates) the full data-registry resource set
// when the catalog annotation is present: a two-container Deployment
// (feast-server + kube-rbac-proxy), an HTTPS Service, an auth.yaml ConfigMap,
// and aggregated ClusterRoles.
// When the annotation is removed or absent, all resources are cleaned up.
func (feast *FeastServices) deployDataRegistry() error {
	if !feast.isDataRegistryEnabled() {
		return feast.CleanupDataRegistry()
	}

	// Ensure the finalizer is present so that cluster-scoped resources are
	// cleaned up even if the operator pod restarts between the CR deletion and
	// the cleanup controller call.
	cr := feast.Handler.FeatureStore
	if !controllerutil.ContainsFinalizer(cr, DataRegistryFinalizer) {
		controllerutil.AddFinalizer(cr, DataRegistryFinalizer)
		if err := feast.Handler.Client.Update(feast.Handler.Context, cr); err != nil {
			return err
		}
	}

	if err := feast.deployDataRegistryAuthConfig(); err != nil {
		return err
	}
	if err := feast.deployDataRegistryClusterRoles(); err != nil {
		return err
	}
	if err := feast.deployDataRegistryAuthDelegatorBinding(); err != nil {
		return err
	}
	if err := feast.createDataRegistryService(); err != nil {
		return err
	}
	if err := feast.createDataRegistryDeployment(); err != nil {
		return err
	}
	if err := feast.deployDataRegistryCaBundleConfigMap(); err != nil {
		return err
	}
	if err := feast.createDataRegistryRoute(); err != nil {
		return err
	}
	if err := feast.createOrDeleteDataRegistryServiceMonitor(); err != nil {
		logger := log.FromContext(feast.Handler.Context)
		logger.Error(err, "Failed to apply Data Registry ServiceMonitor; continuing without metrics scrape")
	}
	return nil
}

// CleanupDataRegistry removes resources before releasing the FeatureStore
// finalizer. It is used by normal mode changes and capability rejection.
func (feast *FeastServices) CleanupDataRegistry() error {
	if err := feast.CleanupDataRegistryResources(); err != nil {
		return err
	}
	cr := feast.Handler.FeatureStore
	if !controllerutil.ContainsFinalizer(cr, DataRegistryFinalizer) {
		return nil
	}
	controllerutil.RemoveFinalizer(cr, DataRegistryFinalizer)
	return feast.Handler.Client.Update(feast.Handler.Context, cr)
}

// CleanupDataRegistryResources removes all data-registry owned resources.
// Exported so the controller can call it when the platform capability is disabled.
func (feast *FeastServices) CleanupDataRegistryResources() error {
	if isOpenShift {
		if err := feast.Handler.DeleteOwnedFeastObj(feast.initDataRegistryRoute()); err != nil {
			return err
		}
	}
	if err := feast.Handler.DeleteOwnedFeastObj(feast.initDataRegistryDeploy()); err != nil {
		return err
	}
	if err := feast.Handler.DeleteOwnedFeastObj(feast.initDataRegistrySvc()); err != nil {
		return err
	}
	if err := feast.Handler.DeleteOwnedFeastObj(feast.initDataRegistryAuthCM()); err != nil {
		return err
	}
	if err := feast.Handler.DeleteOwnedFeastObj(feast.initDataRegistryCaBundleCM()); err != nil {
		return err
	}
	if err := feast.CleanupDataRegistryClusterRoles(); err != nil {
		return err
	}
	if err := feast.CleanupDataRegistryAuthDelegatorBinding(); err != nil {
		return err
	}
	// Explicit cleanup for namespace-scoped monitoring resources that have
	// owner references (GC handles them too, but be explicit for faster cleanup).
	// Guard on hasServiceMonitorCRD: when the monitoring CRD is absent, the
	// API server returns NoKindMatchError on Get/Delete, not NotFound.
	if hasServiceMonitorCRD {
		if err := feast.deleteDataRegistryServiceMonitor(); err != nil {
			return err
		}
	}
	if err := feast.deleteAllNetworkPolicies(); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Deployment
// ---------------------------------------------------------------------------

func (feast *FeastServices) createDataRegistryDeployment() error {
	logger := log.FromContext(feast.Handler.Context)
	deploy := feast.initDataRegistryDeploy()
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		deploy,
		controllerutil.MutateFn(func() error {
			return feast.setDataRegistryDeployment(deploy)
		}),
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "Deployment", deploy.Name, "operation", op)
	}
	return nil
}

func (feast *FeastServices) initDataRegistryDeploy() *appsv1.Deployment {
	deploy := &appsv1.Deployment{
		ObjectMeta: feast.GetObjectMetaType(DataRegistryFeastType),
	}
	deploy.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
	return deploy
}

// setDataRegistryDeployment mutates deploy with the full two-container pod spec:
// feast-server (bound to localhost) + kube-rbac-proxy (exposed to the cluster).
func (feast *FeastServices) setDataRegistryDeployment(deploy *appsv1.Deployment) error {
	cr := feast.Handler.FeatureStore
	labels := feast.getFeastTypeLabels(DataRegistryFeastType)

	selectorLabels := map[string]string{
		NameLabelKey:        cr.Name,
		ServiceTypeLabelKey: string(DataRegistryFeastType),
	}

	var replicas int32 = 1
	deploy.Labels = labels
	deploy.Spec = appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: metav1.SetAsLabelSelector(selectorLabels),
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				Annotations: map[string]string{
					// kube-rbac-proxy reads auth.yaml only at process start.
					// Bump this when SAR config semantics change so pods roll.
					"dataregistry.opendatahub.io/auth-config-revision": "format2-path-capture-v2",
				},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: feast.initFeastSA().Name,
				Volumes: []corev1.Volume{
					{
						Name: "auth-config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: feast.dataRegistryAuthCMName(),
								},
							},
						},
					},
					{
						Name: "tls-certs",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: feast.dataRegistryTlsSecretName(),
							},
						},
					},
				},
			},
		},
	}

	feastCtr, err := feast.buildDataRegistryContainer()
	if err != nil {
		return err
	}
	proxyCtr := feast.buildKubeRBACProxyContainer()

	deploy.Spec.Template.Spec.Containers = []corev1.Container{feastCtr, proxyCtr}
	// Generated feature_store.yaml points the file registry at /feast-data/registry.db.
	feast.mountEmptyDirVolumes(&deploy.Spec.Template.Spec)

	return controllerutil.SetControllerReference(cr, deploy, feast.Handler.Scheme)
}

// buildDataRegistryContainer returns the feast-server container spec.
// feast serve_registry (feature-server 0.66.x) has no --host/-h flag and binds
// uvicorn to 0.0.0.0. Do not pass a host flag; kube-rbac-proxy remains the
// cluster-facing listener. FEAST_PROJECT is set to DataRegistryProject (Phase-1 storage model).
func (feast *FeastServices) buildDataRegistryContainer() (corev1.Container, error) {
	image := getFeatureServerImage()

	fsYamlB64, err := feast.GetServiceFeatureStoreYamlBase64()
	if err != nil {
		return corev1.Container{}, err
	}

	// HTTP probe against GET /healthz: a lightweight endpoint registered before
	// any auth middleware, so it always returns 200 regardless of SAR config.
	// /projects cannot be used because data_registry_sar.py requires a bearer token
	// when DATA_REGISTRY_SAR_API_GROUP is set, and kubelet probes carry no token.
	probeHandler := corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{
			Path:   "/healthz",
			Port:   intstr.FromInt32(DataRegistryPort),
			Scheme: corev1.URISchemeHTTP,
		},
	}

	// SAR env vars are consumed by the Feast Python server (not by kube-rbac-proxy).
	// The proxy uses auth.yaml for its initial authentication gate, but the server
	// performs its own SubjectAccessReview calls during cross-namespace search to
	// determine which namespaces a given user is authorized to browse.
	ctr := corev1.Container{
		Name:  DataRegistryContainerName,
		Image: image,
		Command: []string{
			feastCommand,
			"serve_registry",
			"--no-grpc",
			"--rest-api",
			"--rest-port", strconv.Itoa(int(DataRegistryPort)),
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          metricsPortName,
				ContainerPort: MetricsPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: []corev1.EnvVar{
			// Python create_feature_store() reads FEATURE_STORE_YAML_BASE64 and
			// materializes feature_store.yaml itself. TMP_ is only consumed by
			// the standard feast-init container, which this pod does not run.
			{Name: FeatureStoreYamlEnvVar, Value: fsYamlB64},
			{Name: "FEAST_USAGE", Value: "False"},
			{Name: DataRegistryEnabledEnvVar, Value: "true"},
			{Name: DataRegistrySARApiGroupEnvVar, Value: dataRegistryAPIGroup},
			{Name: DataRegistrySARResourcesEnvVar, Value: "namespaces,tables,volumes,generic-tables"},
			{Name: FeastProjectEnvVar, Value: DataRegistryProject},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(DefaultDataRegistryCPURequest),
				corev1.ResourceMemory: resource.MustParse(DefaultDataRegistryMemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(DefaultDataRegistryCPULimit),
				corev1.ResourceMemory: resource.MustParse(DefaultDataRegistryMemoryLimit),
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  probeHandler,
			PeriodSeconds: 10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:     probeHandler,
			PeriodSeconds:    20,
			FailureThreshold: 6,
		},
		StartupProbe: &corev1.Probe{
			ProbeHandler:     probeHandler,
			PeriodSeconds:    3,
			FailureThreshold: 40,
		},
	}

	// Inherit env/envFrom from spec.services.registry.local.server so
	// ${POSTGRES_USER} (and similar) in the SQL registry path expand.
	if serverConfigs := feast.getServerConfigs(RegistryFeastType); serverConfigs != nil {
		if serverConfigs.OptionalCtrConfigs.EnvFrom != nil {
			ctr.EnvFrom = *serverConfigs.OptionalCtrConfigs.EnvFrom
		}
		if serverConfigs.OptionalCtrConfigs.Env != nil {
			ctr.Env = envOverride(ctr.Env, *serverConfigs.OptionalCtrConfigs.Env)
		}
	}

	return ctr, nil
}

// buildKubeRBACProxyContainer returns the kube-rbac-proxy sidecar spec.
// The proxy listens on 0.0.0.0:8443 (HTTPS) and forwards authenticated
// requests to the feast-server at 127.0.0.1:6572.
//
// Auth.yaml uses Format2 endpoint rules: path capture on /v1/{project}/... and
// byQueryParameter on legacy registry REST. Requires ODH kube-rbac-proxy
// v3.6.0-ea.2+ (named path captures, opendatahub-io/kube-rbac-proxy#28).
func (feast *FeastServices) buildKubeRBACProxyContainer() corev1.Container {
	return corev1.Container{
		Name:  DataRegistryProxyContainerName,
		Image: getKubeRBACProxyImage(),
		Args: []string{
			fmt.Sprintf("--secure-listen-address=0.0.0.0:%d", DataRegistryProxyPort),
			fmt.Sprintf("--upstream=http://%s:%d/", DataRegistryLocalhostAddr, DataRegistryPort),
			"--config-file=/etc/kube-rbac-proxy/auth.yaml",
			"--tls-cert-file=/etc/tls/tls.crt",
			"--tls-private-key-file=/etc/tls/tls.key",
			// Multi-tenant list endpoints bypass proxy SAR: the server
			// performs TokenReview + per-namespace SAR (data_registry_sar.py).
			// Catalog /v1/{project}/search is Format2, not listed here.
			"--ignore-paths=/projects,/api/v1/projects,/v1/projects,/search,/api/v1/search",
			// Forward the authenticated username to the upstream server as
			// X-Remote-User so Python can populate registered_by.
			"--auth-header-fields-enabled",
			"--auth-header-user-field-name=X-Remote-User",
			"--v=3",
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "https",
				ContainerPort: DataRegistryProxyPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(DefaultKubeRBACProxyCPURequest),
				corev1.ResourceMemory: resource.MustParse(DefaultKubeRBACProxyMemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(DefaultKubeRBACProxyCPULimit),
				corev1.ResourceMemory: resource.MustParse(DefaultKubeRBACProxyMemoryLimit),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "auth-config",
				MountPath: "/etc/kube-rbac-proxy",
				ReadOnly:  true,
			},
			{
				Name:      "tls-certs",
				MountPath: "/etc/tls",
				ReadOnly:  true,
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(DataRegistryProxyPort),
				},
			},
			PeriodSeconds: 10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(DataRegistryProxyPort),
				},
			},
			PeriodSeconds:    20,
			FailureThreshold: 6,
		},
	}
}

func getKubeRBACProxyImage() string {
	if img, exists := os.LookupEnv(kubeRBACProxyImageVar); exists && img != "" {
		return img
	}
	return DefaultKubeRBACProxyImage
}

// ---------------------------------------------------------------------------
// Service — HTTPS endpoint exposed by kube-rbac-proxy
// ---------------------------------------------------------------------------

func (feast *FeastServices) createDataRegistryService() error {
	logger := log.FromContext(feast.Handler.Context)
	svc := feast.initDataRegistrySvc()
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		svc,
		controllerutil.MutateFn(func() error {
			return feast.setDataRegistryService(svc)
		}),
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "Service", svc.Name, "operation", op)
	}
	return nil
}

func (feast *FeastServices) initDataRegistrySvc() *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: feast.GetObjectMetaType(DataRegistryFeastType),
	}
	svc.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	return svc
}

func (feast *FeastServices) setDataRegistryService(svc *corev1.Service) error {
	cr := feast.Handler.FeatureStore
	svc.Labels = feast.getFeastTypeLabels(DataRegistryFeastType)

	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[openshiftServingCertSecretAnnotation] = feast.dataRegistryTlsSecretName() // pragma: allowlist secret

	svc.Spec = corev1.ServiceSpec{
		Selector: map[string]string{
			NameLabelKey:        cr.Name,
			ServiceTypeLabelKey: string(DataRegistryFeastType),
		},
		Type: corev1.ServiceTypeClusterIP,
		Ports: []corev1.ServicePort{
			{
				Name:       HttpsScheme,
				Port:       int32(HttpsPort),
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt32(DataRegistryProxyPort),
			},
			{
				Name:       metricsPortName,
				Port:       MetricsPort,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt32(MetricsPort),
			},
		},
	}

	return controllerutil.SetControllerReference(cr, svc, feast.Handler.Scheme)
}

// ---------------------------------------------------------------------------
// auth.yaml ConfigMap — kube-rbac-proxy SubjectAccessReview configuration
// ---------------------------------------------------------------------------

func (feast *FeastServices) deployDataRegistryAuthConfig() error {
	logger := log.FromContext(feast.Handler.Context)
	cm := feast.initDataRegistryAuthCM()
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		cm,
		controllerutil.MutateFn(func() error {
			return feast.setDataRegistryAuthConfig(cm)
		}),
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "ConfigMap", cm.Name, "operation", op)
	}
	return nil
}

func (feast *FeastServices) initDataRegistryAuthCM() *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      feast.dataRegistryAuthCMName(),
			Namespace: feast.Handler.FeatureStore.Namespace,
		},
	}
	cm.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	return cm
}

func (feast *FeastServices) setDataRegistryAuthConfig(cm *corev1.ConfigMap) error {
	cr := feast.Handler.FeatureStore
	cm.Labels = feast.getFeastTypeLabels(DataRegistryFeastType)

	// Per-tenant SAR via Format2 endpoints only (path capture + ?project=).
	// Multi-tenant list paths are in --ignore-paths for server-side SAR.
	// Catalog /v1/{project}/search is a Format2 path, not ignore-paths.
	authYaml := feast.buildDataRegistryAuthYaml()

	cm.Data = map[string]string{
		"auth.yaml": authYaml,
	}

	return controllerutil.SetControllerReference(cr, cm, feast.Handler.Scheme)
}

// ---------------------------------------------------------------------------
// Aggregated ClusterRoles — viewer and editor permissions for data registry
// ---------------------------------------------------------------------------

func (feast *FeastServices) deployDataRegistryClusterRoles() error {
	logger := log.FromContext(feast.Handler.Context)

	viewerCR := feast.initDataRegistryClusterRole("viewer")
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		viewerCR,
		func() error {
			viewerCR.Labels = feast.dataRegistryRBACLabels()
			for key, value := range map[string]string{
				"rbac.authorization.k8s.io/aggregate-to-view":           "true",
				"rbac.authorization.k8s.io/aggregate-to-edit":           "true",
				"rbac.authorization.k8s.io/aggregate-to-admin":          "true",
				"rbac.authorization.k8s.io/aggregate-to-cluster-reader": "true",
			} {
				viewerCR.Labels[key] = value
			}
			viewerCR.Rules = []rbacv1.PolicyRule{
				{
					APIGroups: []string{dataRegistryAPIGroup},
					Resources: dataRegistryPseudoResources,
					Verbs:     []string{"get", "list", "watch"},
				},
			}
			return nil
		},
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "ClusterRole", viewerCR.Name, "operation", op)
	}

	editorCR := feast.initDataRegistryClusterRole("editor")
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		editorCR,
		func() error {
			editorCR.Labels = feast.dataRegistryRBACLabels()
			for key, value := range map[string]string{
				"rbac.authorization.k8s.io/aggregate-to-edit":  "true",
				"rbac.authorization.k8s.io/aggregate-to-admin": "true",
			} {
				editorCR.Labels[key] = value
			}
			editorCR.Rules = []rbacv1.PolicyRule{
				{
					APIGroups: []string{dataRegistryAPIGroup},
					Resources: dataRegistryPseudoResources,
					Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
			}
			return nil
		},
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "ClusterRole", editorCR.Name, "operation", op)
	}

	adminCR := feast.initDataRegistryClusterRole("admin")
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		adminCR,
		func() error {
			adminCR.Labels = feast.dataRegistryRBACLabels()
			for key, value := range map[string]string{
				"rbac.authorization.k8s.io/aggregate-to-admin": "true",
			} {
				adminCR.Labels[key] = value
			}
			adminCR.Rules = []rbacv1.PolicyRule{
				{
					APIGroups: []string{dataRegistryAPIGroup},
					Resources: dataRegistryPseudoResources,
					Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
				{
					APIGroups: []string{dataRegistryAPIGroup},
					Resources: []string{"connections"},
					Verbs:     []string{"use"},
				},
			}
			return nil
		},
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "ClusterRole", adminCR.Name, "operation", op)
	}

	return nil
}

func (feast *FeastServices) initDataRegistryClusterRole(suffix string) *rbacv1.ClusterRole {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: feast.dataRegistryClusterRoleName(suffix),
		},
	}
	cr.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("ClusterRole"))
	return cr
}

// CleanupDataRegistryClusterRoles removes the aggregated ClusterRoles.
// ClusterRoles are cluster-scoped and cannot carry namespace-scoped owner
// references, so we delete by name + managed-by label check instead of
// using DeleteOwnedFeastObj.
// Only deletes ClusterRoles owned by this FeatureStore (name label match)
// to avoid a standard CR's reconcile deleting roles created by the
// data-registry CR.
func (feast *FeastServices) CleanupDataRegistryClusterRoles() error {
	for _, suffix := range []string{"viewer", "editor", "admin"} {
		cr := &rbacv1.ClusterRole{}
		name := feast.dataRegistryClusterRoleName(suffix)
		if err := feast.Handler.Client.Get(
			feast.Handler.Context,
			types.NamespacedName{Name: name},
			cr,
		); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if feast.ownsDataRegistryRBAC(cr.Labels) {
			if err := feast.Handler.Client.Delete(feast.Handler.Context, cr); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Route — ReEncrypt Route exposing the data-registry to external clients
// ---------------------------------------------------------------------------

func (feast *FeastServices) createDataRegistryRoute() error {
	if !isOpenShift {
		return nil
	}
	logger := log.FromContext(feast.Handler.Context)
	route := feast.initDataRegistryRoute()
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		route,
		controllerutil.MutateFn(func() error {
			return feast.setDataRegistryRoute(route)
		}),
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "Route", route.Name, "operation", op)
	}
	return nil
}

func (feast *FeastServices) initDataRegistryRoute() *routev1.Route {
	route := &routev1.Route{
		ObjectMeta: feast.GetObjectMetaType(DataRegistryFeastType),
	}
	route.SetGroupVersionKind(routev1.SchemeGroupVersion.WithKind("Route"))
	return route
}

// setDataRegistryRoute configures a ReEncrypt Route that terminates TLS at the
// OpenShift Router and re-encrypts traffic to the kube-rbac-proxy sidecar.
// The destinationCACertificate is read from the Service CA bundle ConfigMap
// so the Router trusts the internal service certificate.
func (feast *FeastServices) setDataRegistryRoute(route *routev1.Route) error {
	cr := feast.Handler.FeatureStore
	route.Labels = feast.getFeastTypeLabels(DataRegistryFeastType)

	svcName := feast.GetFeastServiceName(DataRegistryFeastType)
	route.Spec = routev1.RouteSpec{
		To: routev1.RouteTargetReference{
			Kind: "Service",
			Name: svcName,
		},
		Port: &routev1.RoutePort{
			TargetPort: intstr.FromInt32(DataRegistryProxyPort),
		},
		TLS: &routev1.TLSConfig{
			Termination:                   routev1.TLSTerminationReencrypt,
			InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
		},
	}

	caCert := feast.readServiceCACert()
	if caCert != "" {
		route.Spec.TLS.DestinationCACertificate = caCert
	}

	return controllerutil.SetControllerReference(cr, route, feast.Handler.Scheme)
}

// readServiceCACert attempts to read the Service CA certificate from the
// injected CA bundle ConfigMap. Returns empty string if not yet available
// (the ConfigMap is populated asynchronously by OpenShift's service-ca
// controller; the next reconciliation will pick it up).
func (feast *FeastServices) readServiceCACert() string {
	logger := log.FromContext(feast.Handler.Context)
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{
		Name:      feast.dataRegistryCaBundleCMName(),
		Namespace: feast.Handler.FeatureStore.Namespace,
	}
	if err := feast.Handler.Client.Get(feast.Handler.Context, key, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to read Service CA bundle ConfigMap", "name", key.Name)
		}
		return ""
	}
	return cm.Data["service-ca.crt"]
}

// ---------------------------------------------------------------------------
// CA Bundle ConfigMap — annotated for OpenShift Service CA injection
// ---------------------------------------------------------------------------

func (feast *FeastServices) deployDataRegistryCaBundleConfigMap() error {
	if !isOpenShift {
		return nil
	}
	logger := log.FromContext(feast.Handler.Context)
	cm := feast.initDataRegistryCaBundleCM()
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		cm,
		controllerutil.MutateFn(func() error {
			return feast.setDataRegistryCaBundleConfigMap(cm)
		}),
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "ConfigMap", cm.Name, "operation", op)
	}
	return nil
}

func (feast *FeastServices) initDataRegistryCaBundleCM() *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      feast.dataRegistryCaBundleCMName(),
			Namespace: feast.Handler.FeatureStore.Namespace,
		},
	}
	cm.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	return cm
}

func (feast *FeastServices) setDataRegistryCaBundleConfigMap(cm *corev1.ConfigMap) error {
	cr := feast.Handler.FeatureStore
	cm.Labels = feast.getFeastTypeLabels(DataRegistryFeastType)
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[openshiftInjectCaBundleAnnotation] = stringTrue
	return controllerutil.SetControllerReference(cr, cm, feast.Handler.Scheme)
}

// ---------------------------------------------------------------------------
// Auth Delegator ClusterRoleBinding — grants the SA SubjectAccessReview perms
// ---------------------------------------------------------------------------

// deployDataRegistryAuthDelegatorBinding creates a ClusterRoleBinding that
// binds the feast ServiceAccount to the system:auth-delegator ClusterRole.
// This allows the data-registry-server to perform SubjectAccessReview calls
// for server-side authorization of cross-namespace search requests.
func (feast *FeastServices) deployDataRegistryAuthDelegatorBinding() error {
	logger := log.FromContext(feast.Handler.Context)
	crb := feast.initDataRegistryAuthDelegatorCRB()
	if op, err := controllerutil.CreateOrUpdate(
		feast.Handler.Context,
		feast.Handler.Client,
		crb,
		func() error {
			crb.Labels = feast.dataRegistryRBACLabels()
			crb.Subjects = []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      feast.initFeastSA().Name,
					Namespace: feast.Handler.FeatureStore.Namespace,
				},
			}
			crb.RoleRef = rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     "system:auth-delegator",
			}
			return nil
		},
	); err != nil {
		return err
	} else if op == controllerutil.OperationResultCreated || op == controllerutil.OperationResultUpdated {
		logger.Info("Successfully reconciled", "ClusterRoleBinding", crb.Name, "operation", op)
	}
	if feast.Handler.FeatureStore.UID != "" {
		return feast.deleteDataRegistryAuthDelegatorBinding(feast.legacyDataRegistryAuthDelegatorCRBName())
	}
	return nil
}

func (feast *FeastServices) initDataRegistryAuthDelegatorCRB() *rbacv1.ClusterRoleBinding {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: feast.dataRegistryAuthDelegatorCRBName(),
		},
	}
	crb.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("ClusterRoleBinding"))
	return crb
}

// CleanupDataRegistryAuthDelegatorBinding removes the auth-delegator CRB.
// ClusterRoleBindings are cluster-scoped and cannot carry namespace-scoped
// owner references, so we delete by name + managed-by label check.
func (feast *FeastServices) CleanupDataRegistryAuthDelegatorBinding() error {
	currentName := feast.dataRegistryAuthDelegatorCRBName()
	legacyName := feast.legacyDataRegistryAuthDelegatorCRBName()
	if err := feast.deleteDataRegistryAuthDelegatorBinding(currentName); err != nil {
		return err
	}
	if currentName != legacyName {
		if err := feast.deleteDataRegistryAuthDelegatorBinding(legacyName); err != nil {
			return err
		}
	}
	return nil
}

func (feast *FeastServices) deleteDataRegistryAuthDelegatorBinding(name string) error {
	crb := &rbacv1.ClusterRoleBinding{}
	if err := feast.Handler.Client.Get(
		feast.Handler.Context,
		types.NamespacedName{Name: name},
		crb,
	); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if feast.ownsDataRegistryRBAC(crb.Labels) {
		if err := feast.Handler.Client.Delete(feast.Handler.Context, crb); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Naming helpers
// ---------------------------------------------------------------------------

func (feast *FeastServices) dataRegistryAuthCMName() string {
	return GetFeastName(feast.Handler.FeatureStore) + dataRegistryAuthConfigSuffix
}

func (feast *FeastServices) dataRegistryTlsSecretName() string {
	return GetFeastName(feast.Handler.FeatureStore) + dataRegistryTlsSecretSuffix
}

func (feast *FeastServices) dataRegistryClusterRoleName(suffix string) string {
	switch suffix {
	case "viewer":
		return DataRegistryViewerClusterRoleName
	case "editor":
		return DataRegistryEditorClusterRoleName
	case "admin":
		return DataRegistryAdminClusterRoleName
	default:
		return "feast-data-registry-" + suffix
	}
}

func (feast *FeastServices) dataRegistryAuthDelegatorCRBName() string {
	name := feast.legacyDataRegistryAuthDelegatorCRBName()
	if uid := feast.Handler.FeatureStore.UID; uid != "" {
		return name + "-" + string(uid)
	}
	return name
}

func (feast *FeastServices) legacyDataRegistryAuthDelegatorCRBName() string {
	return GetFeastName(feast.Handler.FeatureStore) + dataRegistryAuthDelegatorSuffix
}

func (feast *FeastServices) dataRegistryRBACLabels() map[string]string {
	labels := map[string]string{
		NameLabelKey:      feast.Handler.FeatureStore.Name,
		ManagedByLabelKey: ManagedByLabelValue,
	}
	if uid := feast.Handler.FeatureStore.UID; uid != "" {
		labels[dataRegistryOwnerUIDLabelKey] = string(uid)
	}
	return labels
}

func (feast *FeastServices) ownsDataRegistryRBAC(labels map[string]string) bool {
	if labels[ManagedByLabelKey] != ManagedByLabelValue ||
		labels[NameLabelKey] != feast.Handler.FeatureStore.Name {
		return false
	}
	ownerUID := labels[dataRegistryOwnerUIDLabelKey]
	return ownerUID == "" || (feast.Handler.FeatureStore.UID != "" && ownerUID == string(feast.Handler.FeatureStore.UID))
}

func (feast *FeastServices) dataRegistryCaBundleCMName() string {
	return GetFeastName(feast.Handler.FeatureStore) + dataRegistryCaBundleSuffix
}
