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
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/handler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var _ = Describe("Data Registry", func() {
	var (
		featureStore       *feastdevv1.FeatureStore
		feast              *FeastServices
		typeNamespacedName types.NamespacedName
		ctx                context.Context
	)

	setAnnotation := func(value string) {
		// Re-read the latest CR to avoid conflict with deployDataRegistry()
		// which may have updated the CR (e.g. added a finalizer).
		latest := &feastdevv1.FeatureStore{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, latest)).To(Succeed())
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		if value == "" {
			delete(latest.Annotations, DataRegistryAnnotation)
		} else {
			latest.Annotations[DataRegistryAnnotation] = value
		}
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		featureStore = latest
		feast.refreshFeatureStore(ctx, typeNamespacedName)
	}

	// labelNamespace adds or removes the data-registry label on the test namespace.
	labelNamespace := func(ctx context.Context, add bool) {
		nsObj := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: DefaultNs}, nsObj)).To(Succeed())
		if nsObj.Labels == nil {
			nsObj.Labels = map[string]string{}
		}
		if add {
			nsObj.Labels[DataRegistryNamespaceLabel] = "true"
			delete(nsObj.Labels, DataRegistryPlatformNamespaceLabel)
		} else {
			delete(nsObj.Labels, DataRegistryNamespaceLabel)
			delete(nsObj.Labels, DataRegistryPlatformNamespaceLabel)
		}
		Expect(k8sClient.Update(ctx, nsObj)).To(Succeed())
	}

	BeforeEach(func() {
		isOpenShift = false
		ctx = context.Background()
		typeNamespacedName = types.NamespacedName{
			Name:      "dr-teststore",
			Namespace: DefaultNs,
		}
		// Label the test namespace so the data-registry CR is allowed here.
		labelNamespace(ctx, true)

		featureStore = &feastdevv1.FeatureStore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      typeNamespacedName.Name,
				Namespace: typeNamespacedName.Namespace,
			},
			Spec: feastdevv1.FeatureStoreSpec{
				FeastProject: "data_registry",
				Services: &feastdevv1.FeatureStoreServices{
					Registry: &feastdevv1.Registry{
						Local: &feastdevv1.LocalRegistryConfig{
							Server: &feastdevv1.RegistryServerConfigs{
								ServerConfigs: feastdevv1.ServerConfigs{
									ContainerConfigs: feastdevv1.ContainerConfigs{
										DefaultCtrConfigs: feastdevv1.DefaultCtrConfigs{
											Image: ptr.To("test-image"),
										},
									},
								},
								GRPC:    ptr.To(true),
								RestAPI: ptr.To(false),
							},
						},
					},
				},
			},
		}

		Expect(k8sClient.Create(ctx, featureStore)).To(Succeed())
		applySpecToStatus(featureStore)

		feast = &FeastServices{
			Handler: handler.FeastHandler{
				Client:       k8sClient,
				Context:      ctx,
				Scheme:       k8sClient.Scheme(),
				FeatureStore: featureStore,
			},
		}
	})

	AfterEach(func() {
		labelNamespace(ctx, false)
		// Fetch the latest CR to remove the finalizer before deletion.
		// deployDataRegistry() adds a finalizer that prevents the CR from
		// being garbage-collected; without this, subsequent tests fail with
		// "object is being deleted" on Create.
		latest := &feastdevv1.FeatureStore{}
		if err := k8sClient.Get(ctx, typeNamespacedName, latest); err == nil {
			if controllerutil.ContainsFinalizer(latest, DataRegistryFinalizer) {
				controllerutil.RemoveFinalizer(latest, DataRegistryFinalizer)
				Expect(k8sClient.Update(ctx, latest)).To(Succeed())
			}
			Expect(k8sClient.Delete(ctx, latest)).To(Succeed())
		}
	})

	It("is disabled by default and only activates on exact 'true' annotation", func() {
		Expect(feast.isDataRegistryEnabled()).To(BeFalse(), "no annotation")

		for _, v := range []string{"false", "TRUE", "yes", "1"} {
			setAnnotation(v)
			Expect(feast.isDataRegistryEnabled()).To(BeFalse(), "annotation=%q should not enable", v)
		}

		setAnnotation("true")
		Expect(feast.isDataRegistryEnabled()).To(BeTrue())
	})

	It("produces a two-container Deployment with --ignore-paths (no /search) and FEAST_PROJECT=data_registry", func() {
		setAnnotation("true")

		deploy := feast.initDataRegistryDeploy()
		Expect(feast.setDataRegistryDeployment(deploy)).To(Succeed())

		expectedName := GetFeastName(featureStore) + "-" + string(DataRegistryFeastType)
		Expect(deploy.Name).To(Equal(expectedName))
		Expect(deploy.Namespace).To(Equal(typeNamespacedName.Namespace))

		// Labels & selector
		Expect(deploy.Labels).To(HaveKeyWithValue(NameLabelKey, featureStore.Name))
		Expect(deploy.Labels).To(HaveKeyWithValue(ServiceTypeLabelKey, string(DataRegistryFeastType)))
		selector := deploy.Spec.Selector.MatchLabels
		Expect(selector).To(HaveLen(2))
		Expect(selector).To(HaveKeyWithValue(NameLabelKey, featureStore.Name))
		Expect(selector).To(HaveKeyWithValue(ServiceTypeLabelKey, string(DataRegistryFeastType)))

		Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))
		Expect(deploy.Spec.Template.Spec.ServiceAccountName).To(Equal(feast.initFeastSA().Name))
		Expect(deploy.Spec.Template.Annotations).To(HaveKeyWithValue(
			"dataregistry.opendatahub.io/auth-config-revision", "format2-path-capture-v2"))

		// Owner reference
		Expect(deploy.OwnerReferences).To(HaveLen(1))
		Expect(deploy.OwnerReferences[0].Name).To(Equal(featureStore.Name))
		Expect(*deploy.OwnerReferences[0].Controller).To(BeTrue())

		// Must have exactly 2 containers: feast-server + kube-rbac-proxy
		Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(2))

		// --- feast-server container ---
		feastCtr := deploy.Spec.Template.Spec.Containers[0]
		Expect(feastCtr.Name).To(Equal(DataRegistryContainerName))

		// REST-only: --grpc defaults to true and --port is the gRPC port. Passing
		// --rest-api --port 6572 starts both servers on 6572 (EADDRINUSE).
		Expect(feastCtr.Command).To(ContainElements("feast", "serve_registry", "--no-grpc", "--rest-api", "--rest-port"))
		Expect(feastCtr.Command).NotTo(ContainElement("-h"))
		Expect(feastCtr.Command).NotTo(ContainElement("--host"))
		Expect(feastCtr.Command).NotTo(ContainElement("--port"))

		// Metrics port exposed directly (bypasses kube-rbac-proxy)
		Expect(feastCtr.Ports).To(HaveLen(1))
		Expect(feastCtr.Ports[0].Name).To(Equal(metricsPortName))
		Expect(feastCtr.Ports[0].ContainerPort).To(Equal(MetricsPort))
		Expect(feastCtr.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))

		// Env vars
		envMap := map[string]string{}
		for _, e := range feastCtr.Env {
			envMap[e.Name] = e.Value
		}
		Expect(envMap).To(HaveKey(FeatureStoreYamlEnvVar))
		Expect(envMap[FeatureStoreYamlEnvVar]).NotTo(BeEmpty())
		Expect(envMap).NotTo(HaveKey(TmpFeatureStoreYamlEnvVar))
		// Proxy is the auth gate; Feast kubernetes auth 401s valid SA tokens.
		fsYaml, err := base64.StdEncoding.DecodeString(envMap[FeatureStoreYamlEnvVar])
		Expect(err).NotTo(HaveOccurred())
		Expect(string(fsYaml)).To(ContainSubstring("type: no_auth"))
		Expect(envMap).To(HaveKeyWithValue("FEAST_USAGE", "False"))
		Expect(envMap).To(HaveKeyWithValue(DataRegistryEnabledEnvVar, "true"))
		Expect(envMap).To(HaveKeyWithValue(DataRegistrySARApiGroupEnvVar, "dataregistry.opendatahub.io")) // matches dataRegistryAPIGroup constant
		Expect(envMap).To(HaveKeyWithValue(DataRegistrySARResourcesEnvVar, "namespaces,tables,volumes,generic-tables"))
		// FEAST_PROJECT must be "data_registry" (Phase-1 storage model)
		Expect(envMap).To(HaveKeyWithValue(FeastProjectEnvVar, DataRegistryProject))

		// HTTP GET /healthz probe: bypasses SAR middleware so kubelet can probe
		// without a bearer token. /projects cannot be used because data_registry_sar.py
		// returns 401 when DATA_REGISTRY_SAR_API_GROUP is set.
		for _, p := range []*corev1.Probe{feastCtr.ReadinessProbe, feastCtr.LivenessProbe, feastCtr.StartupProbe} {
			Expect(p).NotTo(BeNil())
			Expect(p.HTTPGet).NotTo(BeNil(), "data-registry probe should use HTTPGet, not TCPSocket")
			Expect(p.HTTPGet.Path).To(Equal("/healthz"))
			Expect(p.HTTPGet.Port).To(Equal(intstr.FromInt32(DataRegistryPort)))
			Expect(p.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTP))
			Expect(p.TCPSocket).To(BeNil(), "TCPSocket probe should not be set (HTTP probe is preferred)")
		}

		// --- kube-rbac-proxy container ---
		proxyCtr := deploy.Spec.Template.Spec.Containers[1]
		Expect(proxyCtr.Name).To(Equal(DataRegistryProxyContainerName))
		Expect(proxyCtr.Image).To(Equal(DefaultKubeRBACProxyImage))
		Expect(proxyCtr.Args).To(ContainElements(
			fmt.Sprintf("--secure-listen-address=0.0.0.0:%d", DataRegistryProxyPort),
			fmt.Sprintf("--upstream=http://%s:%d/", DataRegistryLocalhostAddr, DataRegistryPort),
			"--config-file=/etc/kube-rbac-proxy/auth.yaml",
			"--tls-cert-file=/etc/tls/tls.crt",
			"--tls-private-key-file=/etc/tls/tls.key",
			// /projects and /search bypass proxy for server-side SAR (bearer token passthrough)
			"--ignore-paths=/projects,/api/v1/projects,/v1/projects,/search,/api/v1/search",
			"--auth-header-fields-enabled",
			"--auth-header-user-field-name=X-Remote-User",
		))
		// /search and /api/v1/search must NOT appear in ignore-paths separately
		// because /search is already included; verify it's present.
		for _, arg := range proxyCtr.Args {
			if strings.Contains(arg, "ignore-paths") {
				Expect(arg).To(ContainSubstring("/search"), "--ignore-paths must contain /search for server-side SAR")
			}
		}
		Expect(proxyCtr.Ports).To(ConsistOf(corev1.ContainerPort{
			Name: "https", ContainerPort: DataRegistryProxyPort, Protocol: corev1.ProtocolTCP,
		}))

		// Proxy volume mounts
		Expect(proxyCtr.VolumeMounts).To(ContainElements(
			corev1.VolumeMount{Name: "auth-config", MountPath: "/etc/kube-rbac-proxy", ReadOnly: true},
			corev1.VolumeMount{Name: "tls-certs", MountPath: "/etc/tls", ReadOnly: true},
		))

		// Proxy probes
		Expect(proxyCtr.ReadinessProbe).NotTo(BeNil())
		Expect(proxyCtr.ReadinessProbe.TCPSocket.Port).To(Equal(intstr.FromInt32(DataRegistryProxyPort)))
		Expect(proxyCtr.LivenessProbe).NotTo(BeNil())
		Expect(proxyCtr.LivenessProbe.TCPSocket.Port).To(Equal(intstr.FromInt32(DataRegistryProxyPort)))

		// Pod volumes: auth, tls, and emptyDir for the file registry path.
		volumes := deploy.Spec.Template.Spec.Volumes
		Expect(volumes).To(HaveLen(3))

		var authVol, tlsVol, dataVol *corev1.Volume
		for i := range volumes {
			switch volumes[i].Name {
			case "auth-config":
				authVol = &volumes[i]
			case "tls-certs":
				tlsVol = &volumes[i]
			case strings.TrimPrefix(EphemeralPath, "/"):
				dataVol = &volumes[i]
			}
		}
		Expect(authVol).NotTo(BeNil())
		Expect(authVol.ConfigMap.Name).To(Equal(feast.dataRegistryAuthCMName()))
		Expect(tlsVol).NotTo(BeNil())
		Expect(tlsVol.Secret.SecretName).To(Equal(feast.dataRegistryTlsSecretName()))
		Expect(dataVol).NotTo(BeNil())
		Expect(dataVol.EmptyDir).NotTo(BeNil())
		Expect(feastCtr.VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name:      strings.TrimPrefix(EphemeralPath, "/"),
			MountPath: EphemeralPath,
		}))
	})

	It("creates an HTTPS Service targeting the proxy port", func() {
		setAnnotation("true")

		svc := feast.initDataRegistrySvc()
		Expect(feast.setDataRegistryService(svc)).To(Succeed())

		Expect(svc.Labels).To(HaveKeyWithValue(ServiceTypeLabelKey, string(DataRegistryFeastType)))

		// OpenShift serving cert annotation
		Expect(svc.Annotations).To(HaveKey(openshiftServingCertSecretAnnotation))
		Expect(svc.Annotations[openshiftServingCertSecretAnnotation]).To(Equal(feast.dataRegistryTlsSecretName()))

		// Service spec
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		Expect(svc.Spec.Selector).To(HaveKeyWithValue(NameLabelKey, featureStore.Name))
		Expect(svc.Spec.Selector).To(HaveKeyWithValue(ServiceTypeLabelKey, string(DataRegistryFeastType)))
		Expect(svc.Spec.Ports).To(HaveLen(2))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(HttpsPort)))
		Expect(svc.Spec.Ports[0].TargetPort).To(Equal(intstr.FromInt32(DataRegistryProxyPort)))
		Expect(svc.Spec.Ports[0].Name).To(Equal(HttpsScheme))
		Expect(svc.Spec.Ports[1].Name).To(Equal(metricsPortName))
		Expect(svc.Spec.Ports[1].Port).To(Equal(MetricsPort))
		Expect(svc.Spec.Ports[1].TargetPort).To(Equal(intstr.FromInt32(MetricsPort)))

		// Owner reference
		Expect(svc.OwnerReferences).To(HaveLen(1))
		Expect(svc.OwnerReferences[0].Name).To(Equal(featureStore.Name))
	})

	It("creates an auth.yaml ConfigMap with per-tenant Format2 endpoint rules", func() {
		setAnnotation("true")

		cm := feast.initDataRegistryAuthCM()
		Expect(feast.setDataRegistryAuthConfig(cm)).To(Succeed())

		Expect(cm.Labels).To(HaveKeyWithValue(ServiceTypeLabelKey, string(DataRegistryFeastType)))
		Expect(cm.Data).To(HaveKey("auth.yaml"))

		authContent := cm.Data["auth.yaml"]

		Expect(authContent).To(ContainSubstring("dataregistry.opendatahub.io"))
		Expect(authContent).To(ContainSubstring("resource: registries"))
		Expect(authContent).To(ContainSubstring("endpoints:"))

		// No Format1 fallback on the data-registry install namespace.
		Expect(authContent).NotTo(ContainSubstring("namespace: " + featureStore.Namespace))

		// Catalog: path capture for Feast project (= K8s namespace).
		Expect(authContent).To(ContainSubstring("path: /v1/{project}/namespaces"))
		Expect(authContent).To(ContainSubstring(`namespace: '{{ index .PathParams "project" }}'`))

		// Legacy registry REST: ?project= and /api/v1 prefix.
		for _, path := range []string{"/entities", "/feature_views", "/data_sources",
			"/feature_services", "/saved_datasets", "/permissions", "/features",
			"/labels", "/label_views"} {
			Expect(authContent).To(ContainSubstring("path: " + path))
			Expect(authContent).To(ContainSubstring("path: /api/v1" + path))
		}
		Expect(authContent).To(ContainSubstring("path: /entities/*"))
		Expect(authContent).To(ContainSubstring("path: /api/v1/entities/*"))
		Expect(authContent).To(ContainSubstring("path: /features/*/*"))
		Expect(authContent).To(ContainSubstring("path: /saved_datasets/data/*"))

		Expect(authContent).To(ContainSubstring("byQueryParameter:"))
		Expect(authContent).To(ContainSubstring("name: project"))
		Expect(authContent).To(ContainSubstring(`namespace: "{{ .Value }}"`))

		Expect(authContent).To(ContainSubstring("path: /v1/config"))
		Expect(authContent).To(ContainSubstring("name: warehouse"))

		// Catalog search is tenant-scoped; proxy SAR uses {project}.
		Expect(authContent).To(ContainSubstring("path: /v1/{project}/search"))
		// Iceberg HEAD is existence check; Kubernetes has no HEAD verb.
		Expect(authContent).To(ContainSubstring("methods: [head]"))
		Expect(authContent).To(ContainSubstring("verb: get"))

		Expect(authContent).NotTo(ContainSubstring("path: /search"))

		Expect(cm.OwnerReferences).To(HaveLen(1))
		Expect(cm.OwnerReferences[0].Name).To(Equal(featureStore.Name))
	})

	It("produces a ReEncrypt Route spec targeting the proxy Service", func() {
		setAnnotation("true")

		route := feast.initDataRegistryRoute()
		Expect(feast.setDataRegistryRoute(route)).To(Succeed())

		expectedSvcName := feast.GetFeastServiceName(DataRegistryFeastType)
		Expect(route.Labels).To(HaveKeyWithValue(ServiceTypeLabelKey, string(DataRegistryFeastType)))
		Expect(route.Spec.To.Kind).To(Equal("Service"))
		Expect(route.Spec.To.Name).To(Equal(expectedSvcName))
		Expect(route.Spec.Port.TargetPort).To(Equal(intstr.FromInt32(DataRegistryProxyPort)))

		Expect(route.Spec.TLS).NotTo(BeNil())
		Expect(route.Spec.TLS.Termination).To(Equal(routev1.TLSTerminationReencrypt))
		Expect(route.Spec.TLS.InsecureEdgeTerminationPolicy).To(Equal(routev1.InsecureEdgeTerminationPolicyRedirect))

		// Without a populated CA bundle CM, DestinationCACertificate is empty
		Expect(route.Spec.TLS.DestinationCACertificate).To(BeEmpty())

		// Owner reference
		Expect(route.OwnerReferences).To(HaveLen(1))
		Expect(route.OwnerReferences[0].Name).To(Equal(featureStore.Name))
	})

	It("populates DestinationCACertificate when CA bundle ConfigMap exists", func() {
		setAnnotation("true")

		// Simulate OpenShift injecting the service CA into the bundle CM
		bundleCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      feast.dataRegistryCaBundleCMName(),
				Namespace: typeNamespacedName.Namespace,
			},
			Data: map[string]string{
				"service-ca.crt": "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n",
			},
		}
		Expect(k8sClient.Create(ctx, bundleCM)).To(Succeed())

		route := feast.initDataRegistryRoute()
		Expect(feast.setDataRegistryRoute(route)).To(Succeed())
		Expect(route.Spec.TLS.DestinationCACertificate).To(ContainSubstring("FAKECA"))

		Expect(k8sClient.Delete(ctx, bundleCM)).To(Succeed())
	})

	It("produces a CA bundle ConfigMap with inject-cabundle annotation", func() {
		setAnnotation("true")

		cm := feast.initDataRegistryCaBundleCM()
		Expect(feast.setDataRegistryCaBundleConfigMap(cm)).To(Succeed())

		Expect(cm.Labels).To(HaveKeyWithValue(ServiceTypeLabelKey, string(DataRegistryFeastType)))
		Expect(cm.Annotations).To(HaveKeyWithValue(openshiftInjectCaBundleAnnotation, stringTrue))

		// Owner reference
		Expect(cm.OwnerReferences).To(HaveLen(1))
		Expect(cm.OwnerReferences[0].Name).To(Equal(featureStore.Name))
	})

	It("produces an auth-delegator ClusterRoleBinding for server-side SAR", func() {
		setAnnotation("true")

		Expect(feast.deployDataRegistryAuthDelegatorBinding()).To(Succeed())

		crb := &rbacv1.ClusterRoleBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryAuthDelegatorCRBName()}, crb)).To(Succeed())

		Expect(crb.RoleRef.APIGroup).To(Equal(rbacv1.GroupName))
		Expect(crb.RoleRef.Kind).To(Equal("ClusterRole"))
		Expect(crb.RoleRef.Name).To(Equal("system:auth-delegator"))

		Expect(crb.Subjects).To(HaveLen(1))
		Expect(crb.Subjects[0].Kind).To(Equal("ServiceAccount"))
		Expect(crb.Subjects[0].Name).To(Equal(feast.initFeastSA().Name))
		Expect(crb.Subjects[0].Namespace).To(Equal(typeNamespacedName.Namespace))

		Expect(crb.Labels).To(HaveKeyWithValue(ManagedByLabelKey, ManagedByLabelValue))

		// Cleanup
		Expect(feast.CleanupDataRegistryAuthDelegatorBinding()).To(Succeed())
		err := k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryAuthDelegatorCRBName()}, &rbacv1.ClusterRoleBinding{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("creates three ClusterRoles: viewer, editor, and admin with all pseudo-resources", func() {
		setAnnotation("true")

		Expect(feast.deployDataRegistryClusterRoles()).To(Succeed())

		expectedResources := ConsistOf("registries", "namespaces", "tables", "volumes", "generic-tables")

		// Viewer ClusterRole
		viewerCR := &rbacv1.ClusterRole{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName("viewer")}, viewerCR)).To(Succeed())
		Expect(viewerCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-view", "true"))
		Expect(viewerCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-edit", "true"))
		Expect(viewerCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-admin", "true"))
		Expect(viewerCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-cluster-reader", "true"))
		Expect(viewerCR.Rules).To(HaveLen(1))
		Expect(viewerCR.Rules[0].APIGroups).To(ConsistOf("dataregistry.opendatahub.io"))
		Expect(viewerCR.Rules[0].Resources).To(expectedResources)
		Expect(viewerCR.Rules[0].Verbs).To(ConsistOf("get", "list", "watch"))

		// Editor ClusterRole
		editorCR := &rbacv1.ClusterRole{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName("editor")}, editorCR)).To(Succeed())
		Expect(editorCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-edit", "true"))
		Expect(editorCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-admin", "true"))
		Expect(editorCR.Labels).NotTo(HaveKey("rbac.authorization.k8s.io/aggregate-to-view"))
		Expect(editorCR.Rules).To(HaveLen(1))
		Expect(editorCR.Rules[0].Resources).To(expectedResources)
		Expect(editorCR.Rules[0].Verbs).To(ConsistOf("get", "list", "watch", "create", "update", "patch", "delete"))

		// Admin ClusterRole
		adminCR := &rbacv1.ClusterRole{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName("admin")}, adminCR)).To(Succeed())
		Expect(adminCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-admin", "true"))
		Expect(adminCR.Labels).NotTo(HaveKey("rbac.authorization.k8s.io/aggregate-to-view"))
		Expect(adminCR.Labels).NotTo(HaveKey("rbac.authorization.k8s.io/aggregate-to-edit"))
		Expect(adminCR.Rules).To(HaveLen(2))
		Expect(adminCR.Rules[0].Resources).To(expectedResources)
		Expect(adminCR.Rules[0].Verbs).To(ConsistOf("get", "list", "watch", "create", "update", "patch", "delete"))
		Expect(adminCR.Rules[1].Resources).To(ConsistOf("connections"))
		Expect(adminCR.Rules[1].Verbs).To(ConsistOf("use"))

		// Cleanup
		Expect(feast.CleanupDataRegistryClusterRoles()).To(Succeed())
		for _, suffix := range []string{"viewer", "editor", "admin"} {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName(suffix)}, &rbacv1.ClusterRole{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})

	It("uses DataRegistryPort constant for --rest-port, not a hardcoded string", func() {
		setAnnotation("true")

		deploy := feast.initDataRegistryDeploy()
		Expect(feast.setDataRegistryDeployment(deploy)).To(Succeed())

		feastCtr := deploy.Spec.Template.Spec.Containers[0]
		expectedPort := strconv.Itoa(int(DataRegistryPort))
		Expect(feastCtr.Command).To(ContainElement(expectedPort), "--rest-port flag should use DataRegistryPort constant")

		proxyCtr := deploy.Spec.Template.Spec.Containers[1]
		expectedUpstream := fmt.Sprintf("--upstream=http://%s:%d/", DataRegistryLocalhostAddr, DataRegistryPort)
		expectedListen := fmt.Sprintf("--secure-listen-address=0.0.0.0:%d", DataRegistryProxyPort)
		Expect(proxyCtr.Args).To(ContainElement(expectedUpstream))
		Expect(proxyCtr.Args).To(ContainElement(expectedListen))
	})

	It("sets resource requests and limits on the kube-rbac-proxy container", func() {
		setAnnotation("true")

		deploy := feast.initDataRegistryDeploy()
		Expect(feast.setDataRegistryDeployment(deploy)).To(Succeed())

		proxyCtr := deploy.Spec.Template.Spec.Containers[1]
		Expect(proxyCtr.Name).To(Equal(DataRegistryProxyContainerName))

		Expect(proxyCtr.Resources.Requests).NotTo(BeNil(), "proxy should have resource requests")
		Expect(proxyCtr.Resources.Limits).NotTo(BeNil(), "proxy should have resource limits")
		Expect(proxyCtr.Resources.Requests.Cpu().String()).To(Equal(DefaultKubeRBACProxyCPURequest))
		Expect(proxyCtr.Resources.Requests.Memory().String()).To(Equal(DefaultKubeRBACProxyMemoryRequest))
		Expect(proxyCtr.Resources.Limits.Cpu().String()).To(Equal(DefaultKubeRBACProxyCPULimit))
		Expect(proxyCtr.Resources.Limits.Memory().String()).To(Equal(DefaultKubeRBACProxyMemoryLimit))
	})

	It("rejects a newer data-registry-enabled FeatureStore CR (singleton enforcement via creationTimestamp)", func() {
		setAnnotation("true")

		// Name sorts after "dr-teststore" so featureStore wins the tiebreaker
		// when creationTimestamps are identical (same second in envtest).
		otherFS := &feastdevv1.FeatureStore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dr-zzz-otherstore",
				Namespace: DefaultNs,
				Annotations: map[string]string{
					DataRegistryAnnotation: "true",
				},
			},
			Spec: feastdevv1.FeatureStoreSpec{
				FeastProject: "other_registry",
				Services: &feastdevv1.FeatureStoreServices{
					Registry: &feastdevv1.Registry{
						Local: &feastdevv1.LocalRegistryConfig{
							Server: &feastdevv1.RegistryServerConfigs{
								ServerConfigs: feastdevv1.ServerConfigs{
									ContainerConfigs: feastdevv1.ContainerConfigs{
										DefaultCtrConfigs: feastdevv1.DefaultCtrConfigs{
											Image: ptr.To("test-image"),
										},
									},
								},
								GRPC:    ptr.To(true),
								RestAPI: ptr.To(false),
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, otherFS)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, otherFS) }()

		// featureStore wins (name sorts first), so validation passes for it.
		Expect(feast.validateDataRegistrySingleton()).To(Succeed())

		// otherFS loses, so validation from its perspective must fail.
		otherFeast := &FeastServices{
			Handler: handler.FeastHandler{
				Client:       k8sClient,
				Context:      ctx,
				Scheme:       k8sClient.Scheme(),
				FeatureStore: otherFS,
			},
		}
		err := otherFeast.validateDataRegistrySingleton()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("already enabled"))
		Expect(err.Error()).To(ContainSubstring("cluster-wide"))
	})

	It("allows the oldest annotated CR to win when multiple exist (creationTimestamp tiebreaker)", func() {
		setAnnotation("true")

		// Name sorts after "dr-teststore" → featureStore wins the tiebreaker.
		newerFS := &feastdevv1.FeatureStore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dr-zzz-newer",
				Namespace: DefaultNs,
				Annotations: map[string]string{
					DataRegistryAnnotation: "true",
				},
			},
			Spec: feastdevv1.FeatureStoreSpec{
				FeastProject: "newer_registry",
				Services: &feastdevv1.FeatureStoreServices{
					Registry: &feastdevv1.Registry{
						Local: &feastdevv1.LocalRegistryConfig{
							Server: &feastdevv1.RegistryServerConfigs{
								ServerConfigs: feastdevv1.ServerConfigs{
									ContainerConfigs: feastdevv1.ContainerConfigs{
										DefaultCtrConfigs: feastdevv1.DefaultCtrConfigs{
											Image: ptr.To("test-image"),
										},
									},
								},
								GRPC:    ptr.To(true),
								RestAPI: ptr.To(false),
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, newerFS)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, newerFS) }()

		// The original featureStore wins → passes.
		Expect(feast.validateDataRegistrySingleton()).To(Succeed())

		// The newer CR is rejected and error references the winning CR.
		newerFeast := &FeastServices{
			Handler: handler.FeastHandler{
				Client:       k8sClient,
				Context:      ctx,
				Scheme:       k8sClient.Scheme(),
				FeatureStore: newerFS,
			},
		}
		err := newerFeast.validateDataRegistrySingleton()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("already enabled"))
		Expect(err.Error()).To(ContainSubstring("cluster-wide"))
		Expect(err.Error()).To(ContainSubstring(featureStore.Name))
	})

	It("validates the auth.yaml apiGroup matches the dataRegistryAPIGroup constant", func() {
		setAnnotation("true")

		cm := feast.initDataRegistryAuthCM()
		Expect(feast.setDataRegistryAuthConfig(cm)).To(Succeed())

		Expect(cm.Data["auth.yaml"]).To(ContainSubstring(dataRegistryAPIGroup))
	})

	It("creates and cleans up the full resource set via deployDataRegistry", func() {
		isOpenShift = false
		drKey := types.NamespacedName{
			Name:      GetFeastName(featureStore) + "-" + string(DataRegistryFeastType),
			Namespace: typeNamespacedName.Namespace,
		}
		cmKey := types.NamespacedName{
			Name:      feast.dataRegistryAuthCMName(),
			Namespace: typeNamespacedName.Namespace,
		}

		// Disabled → no resources
		Expect(feast.deployDataRegistry()).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, drKey, &appsv1.Deployment{}))).To(BeTrue())

		// Enable → all resources created
		setAnnotation("true")
		Expect(feast.deployDataRegistry()).To(Succeed())

		// Deployment exists with 2 containers
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, drKey, deploy)).To(Succeed())
		Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(2))
		Expect(deploy.Spec.Template.Spec.Containers[0].Name).To(Equal(DataRegistryContainerName))
		Expect(deploy.Spec.Template.Spec.Containers[1].Name).To(Equal(DataRegistryProxyContainerName))

		// Service exists
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, drKey, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].TargetPort).To(Equal(intstr.FromInt32(DataRegistryProxyPort)))

		// Auth ConfigMap exists with per-tenant Format2 endpoints
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, cmKey, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKey("auth.yaml"))
		Expect(cm.Data["auth.yaml"]).To(ContainSubstring("resource: registries"))
		Expect(cm.Data["auth.yaml"]).To(ContainSubstring("endpoints:"))
		Expect(cm.Data["auth.yaml"]).To(ContainSubstring("byQueryParameter:"))

		// ClusterRoles: all three (viewer, editor, admin) exist with all pseudo-resources
		expectedResources := ConsistOf("registries", "namespaces", "tables", "volumes", "generic-tables")

		viewerCR := &rbacv1.ClusterRole{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName("viewer")}, viewerCR)).To(Succeed())
		Expect(viewerCR.Rules).To(HaveLen(1))
		Expect(viewerCR.Rules[0].APIGroups).To(ConsistOf("dataregistry.opendatahub.io"))
		Expect(viewerCR.Rules[0].Resources).To(expectedResources)
		Expect(viewerCR.Rules[0].Verbs).To(ConsistOf("get", "list", "watch"))
		Expect(viewerCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-view", "true"))

		editorCR := &rbacv1.ClusterRole{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName("editor")}, editorCR)).To(Succeed())
		Expect(editorCR.Rules[0].Resources).To(expectedResources)
		Expect(editorCR.Rules[0].Verbs).To(ContainElements("create", "update", "patch", "delete"))
		Expect(editorCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-edit", "true"))

		adminCR := &rbacv1.ClusterRole{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName("admin")}, adminCR)).To(Succeed())
		Expect(adminCR.Labels).To(HaveKeyWithValue("rbac.authorization.k8s.io/aggregate-to-admin", "true"))
		Expect(adminCR.Labels).NotTo(HaveKey("rbac.authorization.k8s.io/aggregate-to-view"))
		Expect(adminCR.Rules).To(HaveLen(2))
		Expect(adminCR.Rules[0].Resources).To(expectedResources)
		Expect(adminCR.Rules[1].Resources).To(ConsistOf("connections"))
		Expect(adminCR.Rules[1].Verbs).To(ConsistOf("use"))

		// Auth-delegator ClusterRoleBinding exists
		crbKey := types.NamespacedName{Name: feast.dataRegistryAuthDelegatorCRBName()}
		crb := &rbacv1.ClusterRoleBinding{}
		Expect(k8sClient.Get(ctx, crbKey, crb)).To(Succeed())
		Expect(crb.RoleRef.Name).To(Equal("system:auth-delegator"))
		Expect(crb.Subjects).To(HaveLen(1))
		Expect(crb.Subjects[0].Kind).To(Equal("ServiceAccount"))
		Expect(crb.Subjects[0].Name).To(Equal(feast.initFeastSA().Name))
		Expect(crb.Subjects[0].Namespace).To(Equal(typeNamespacedName.Namespace))

		// Idempotent
		Expect(feast.deployDataRegistry()).To(Succeed())

		// Remove annotation → all resources cleaned up
		setAnnotation("")
		Expect(feast.deployDataRegistry()).To(Succeed())

		err := k8sClient.Get(ctx, drKey, &appsv1.Deployment{})
		deleted := apierrors.IsNotFound(err)
		if !deleted {
			d := &appsv1.Deployment{}
			_ = k8sClient.Get(ctx, drKey, d)
			deleted = d.DeletionTimestamp != nil
		}
		Expect(deleted).To(BeTrue())

		err = k8sClient.Get(ctx, drKey, &corev1.Service{})
		Expect(apierrors.IsNotFound(err) || err == nil).To(BeTrue())

		err = k8sClient.Get(ctx, cmKey, &corev1.ConfigMap{})
		Expect(apierrors.IsNotFound(err) || err == nil).To(BeTrue())

		// Admin ClusterRole cleaned up
		err = k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName("admin")}, &rbacv1.ClusterRole{})
		Expect(apierrors.IsNotFound(err) || err == nil).To(BeTrue())

		// Auth-delegator CRB cleaned up
		err = k8sClient.Get(ctx, crbKey, &rbacv1.ClusterRoleBinding{})
		Expect(apierrors.IsNotFound(err) || err == nil).To(BeTrue())
	})

	It("finalizer cleanup removes cluster-scoped resources for the matching CR UID", func() {
		setAnnotation("true")

		// Create cluster-scoped resources
		Expect(feast.deployDataRegistryClusterRoles()).To(Succeed())
		Expect(feast.deployDataRegistryAuthDelegatorBinding()).To(Succeed())

		// Verify they exist
		for _, suffix := range []string{"viewer", "editor", "admin"} {
			cr := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName(suffix)}, cr)).To(Succeed())
		}
		crb := &rbacv1.ClusterRoleBinding{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryAuthDelegatorCRBName()}, crb)).To(Succeed())

		// Simulate finalizer cleanup with the UID of the CR that owns the RBAC.
		stubFeast := &FeastServices{
			Handler: handler.FeastHandler{
				Client:  k8sClient,
				Context: ctx,
				Scheme:  k8sClient.Scheme(),
				FeatureStore: &feastdevv1.FeatureStore{
					ObjectMeta: metav1.ObjectMeta{
						Name:      featureStore.Name,
						Namespace: featureStore.Namespace,
						UID:       featureStore.UID,
					},
				},
			},
		}

		Expect(stubFeast.CleanupDataRegistryClusterRoles()).To(Succeed())
		Expect(stubFeast.CleanupDataRegistryAuthDelegatorBinding()).To(Succeed())

		// All cluster-scoped resources must be gone
		for _, suffix := range []string{"viewer", "editor", "admin"} {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName(suffix)}, &rbacv1.ClusterRole{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ClusterRole %s should be deleted", suffix)
		}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryAuthDelegatorCRBName()}, &rbacv1.ClusterRoleBinding{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "auth-delegator CRB should be deleted")
	})

	It("uses fixed ClusterRole names independent of the CR name (singleton RBAC)", func() {
		setAnnotation("true")

		Expect(feast.deployDataRegistryClusterRoles()).To(Succeed())

		Expect(feast.dataRegistryClusterRoleName("viewer")).To(Equal(DataRegistryViewerClusterRoleName))
		Expect(feast.dataRegistryClusterRoleName("editor")).To(Equal(DataRegistryEditorClusterRoleName))
		Expect(feast.dataRegistryClusterRoleName("admin")).To(Equal(DataRegistryAdminClusterRoleName))

		// Verify the ClusterRoles are created under the fixed names (not feast-<crName>-data-registry-*)
		for _, suffix := range []string{"viewer", "editor", "admin"} {
			cr := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feast.dataRegistryClusterRoleName(suffix)}, cr)).To(Succeed())
			Expect(cr.Name).NotTo(ContainSubstring(featureStore.Name),
				"ClusterRole name must be fixed and not contain the CR name")
		}

		Expect(feast.CleanupDataRegistryClusterRoles()).To(Succeed())
	})

	It("refuses data-registry mode when the CR already owns PVCs", func() {
		isOpenShift = false

		// Create a PVC that simulates a pre-existing registry PVC owned by this CR.
		pvc := feast.initPVC(RegistryFeastType)
		pvc.Spec = corev1.PersistentVolumeClaimSpec{
			AccessModes: DefaultPVCAccessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(DefaultRegistryStorageRequest),
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pvc) }()

		// Enabling data-registry mode must be refused with a clear message.
		setAnnotation("true")
		err := feast.deployDataRegistryMode()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot enable data-registry mode"))
		Expect(err.Error()).To(ContainSubstring("existing PVC"))
	})

	It("mode transition: standard resources are cleaned up when entering data-registry mode", func() {
		isOpenShift = false

		// Simulate standard-mode resources by creating a Deployment that would
		// normally exist in standard mode. Set the owner reference so that
		// DeleteOwnedFeastObj (which only deletes owned objects) can clean it up.
		feastDeploy := feast.initFeastDeploy()
		feastDeploy.Spec = appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "feast"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "feast"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "feast", Image: "test"}}},
			},
		}
		Expect(controllerutil.SetControllerReference(featureStore, feastDeploy, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, feastDeploy)).To(Succeed())

		standardDeployKey := types.NamespacedName{
			Name:      feastDeploy.Name,
			Namespace: feastDeploy.Namespace,
		}

		// Verify standard Deployment exists
		Expect(k8sClient.Get(ctx, standardDeployKey, &appsv1.Deployment{})).To(Succeed())

		// Enable data-registry mode
		setAnnotation("true")

		// Create a SA so deployDataRegistryMode doesn't fail on SA creation
		sa := feast.initFeastSA()
		sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))
		_ = k8sClient.Create(ctx, sa)

		// Run deployDataRegistryMode
		Expect(feast.deployDataRegistryMode()).To(Succeed())

		// Standard Deployment should be deleted or marked for deletion
		err := k8sClient.Get(ctx, standardDeployKey, &appsv1.Deployment{})
		deleted := apierrors.IsNotFound(err)
		if !deleted {
			d := &appsv1.Deployment{}
			_ = k8sClient.Get(ctx, standardDeployKey, d)
			deleted = d.DeletionTimestamp != nil
		}
		Expect(deleted).To(BeTrue(), "standard Deployment should be removed in data-registry mode")

		// Data-registry Deployment should exist
		drKey := types.NamespacedName{
			Name:      GetFeastName(featureStore) + "-" + string(DataRegistryFeastType),
			Namespace: typeNamespacedName.Namespace,
		}
		drDeploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, drKey, drDeploy)).To(Succeed())
		Expect(drDeploy.Spec.Template.Spec.Containers).To(HaveLen(2))
	})

	It("rejects data-registry CR when namespace lacks the label", func() {
		setAnnotation("true")

		// Remove the label so validation should fail.
		labelNamespace(ctx, false)

		err := feast.validateDataRegistryNamespace()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not designated for the data registry"))
		Expect(err.Error()).To(ContainSubstring(DataRegistryNamespaceLabel))
	})

	It("allows data-registry CR when namespace has the platform module label only", func() {
		setAnnotation("true")
		labelNamespace(ctx, false)
		nsObj := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: DefaultNs}, nsObj)).To(Succeed())
		if nsObj.Labels == nil {
			nsObj.Labels = map[string]string{}
		}
		nsObj.Labels[DataRegistryPlatformNamespaceLabel] = "true"
		Expect(k8sClient.Update(ctx, nsObj)).To(Succeed())
		Expect(feast.validateDataRegistryNamespace()).To(Succeed())
	})

	It("allows data-registry CR when namespace has the label", func() {
		setAnnotation("true")

		// Label is already set by BeforeEach.
		Expect(feast.validateDataRegistryNamespace()).To(Succeed())
	})

	It("sets DataRegistryReady=True condition on successful deployDataRegistryMode", func() {
		isOpenShift = false
		setAnnotation("true")

		sa := feast.initFeastSA()
		sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))
		_ = k8sClient.Create(ctx, sa)

		Expect(feast.deployDataRegistryMode()).To(Succeed())

		// Use feast.Handler.FeatureStore (not the test's featureStore var)
		// because refreshFeatureStore replaces the pointer.
		cond := apimeta.FindStatusCondition(feast.Handler.FeatureStore.Status.Conditions, feastdevv1.DataRegistryReadyType)
		Expect(cond).NotTo(BeNil(), "DataRegistryReady condition should be set")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(feastdevv1.ReadyReason))
		Expect(cond.Message).To(Equal(feastdevv1.DataRegistryReadyMessage))
	})

	It("sets DataRegistryReady=False condition when namespace validation fails", func() {
		setAnnotation("true")

		// Remove the label so namespace validation fails.
		labelNamespace(ctx, false)

		err := feast.deployDataRegistryMode()
		Expect(err).To(HaveOccurred())

		cond := apimeta.FindStatusCondition(feast.Handler.FeatureStore.Status.Conditions, feastdevv1.DataRegistryReadyType)
		Expect(cond).NotTo(BeNil(), "DataRegistryReady condition should be set on namespace failure")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(feastdevv1.DataRegistryFailedReason))
		Expect(cond.Message).To(ContainSubstring("not designated for the data registry"))
	})

	It("sets DataRegistryReady=False condition when singleton validation fails", func() {
		setAnnotation("true")

		// featureStore was created in BeforeEach and is the oldest annotated CR.
		// Create a second annotated CR; it's newer. Point feast at the *newer*
		// CR so that singleton validation rejects it (older CR wins).
		// Name sorts after "dr-teststore" so featureStore wins the tiebreaker.
		newerFS := &feastdevv1.FeatureStore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dr-zzz-singleton-cond",
				Namespace: DefaultNs,
				Annotations: map[string]string{
					DataRegistryAnnotation: "true",
				},
			},
			Spec: feastdevv1.FeatureStoreSpec{
				FeastProject: "other_project",
				Services: &feastdevv1.FeatureStoreServices{
					Registry: &feastdevv1.Registry{
						Local: &feastdevv1.LocalRegistryConfig{
							Server: &feastdevv1.RegistryServerConfigs{
								ServerConfigs: feastdevv1.ServerConfigs{
									ContainerConfigs: feastdevv1.ContainerConfigs{
										DefaultCtrConfigs: feastdevv1.DefaultCtrConfigs{
											Image: ptr.To("test-image"),
										},
									},
								},
								GRPC:    ptr.To(true),
								RestAPI: ptr.To(false),
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, newerFS)).To(Succeed())
		applySpecToStatus(newerFS)
		defer func() { _ = k8sClient.Delete(ctx, newerFS) }()

		newerFeast := &FeastServices{
			Handler: handler.FeastHandler{
				Client:       k8sClient,
				Context:      ctx,
				Scheme:       k8sClient.Scheme(),
				FeatureStore: newerFS,
			},
		}

		err := newerFeast.deployDataRegistryMode()
		Expect(err).To(HaveOccurred())

		cond := apimeta.FindStatusCondition(newerFeast.Handler.FeatureStore.Status.Conditions, feastdevv1.DataRegistryReadyType)
		Expect(cond).NotTo(BeNil(), "DataRegistryReady condition should be set on singleton failure")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(feastdevv1.DataRegistryFailedReason))
		Expect(cond.Message).To(ContainSubstring("already enabled"))
		Expect(cond.Message).To(ContainSubstring("cluster-wide"))
	})

	It("sets DataRegistryReady=False condition when PVC safety guard fails", func() {
		isOpenShift = false

		// Use OnlineFeastType to avoid colliding with the "refuses data-registry
		// mode when the CR already owns PVCs" test which uses RegistryFeastType.
		pvc := feast.initPVC(OnlineFeastType)
		pvc.Spec = corev1.PersistentVolumeClaimSpec{
			AccessModes: DefaultPVCAccessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(DefaultOnlineStorageRequest),
				},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pvc) }()

		setAnnotation("true")
		err := feast.deployDataRegistryMode()
		Expect(err).To(HaveOccurred())

		cond := apimeta.FindStatusCondition(feast.Handler.FeatureStore.Status.Conditions, feastdevv1.DataRegistryReadyType)
		Expect(cond).NotTo(BeNil(), "DataRegistryReady condition should be set on PVC failure")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(feastdevv1.DataRegistryFailedReason))
		Expect(cond.Message).To(ContainSubstring("cannot enable data-registry mode"))
	})

	// ---------------------------------------------------------------------------
	// RHAI-407: Finalizer lifecycle tests
	// ---------------------------------------------------------------------------

	It("adds DataRegistryFinalizer when catalog annotation is enabled", func() {
		isOpenShift = false

		setAnnotation("true")

		sa := feast.initFeastSA()
		sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))
		_ = k8sClient.Create(ctx, sa)

		Expect(feast.deployDataRegistry()).To(Succeed())

		// Re-fetch the CR from the fake client to see the updated finalizers.
		updated := &feastdevv1.FeatureStore{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(updated, DataRegistryFinalizer)).To(BeTrue(),
			"DataRegistryFinalizer should be added when annotation is enabled")
	})

	It("removes DataRegistryFinalizer when catalog annotation is removed", func() {
		isOpenShift = false

		setAnnotation("true")

		sa := feast.initFeastSA()
		sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))
		_ = k8sClient.Create(ctx, sa)

		// First enable — adds finalizer and deploys resources.
		Expect(feast.deployDataRegistry()).To(Succeed())
		feast.refreshFeatureStore(ctx, typeNamespacedName)
		Expect(controllerutil.ContainsFinalizer(feast.Handler.FeatureStore, DataRegistryFinalizer)).To(BeTrue())

		// Remove annotation — triggers cleanup and finalizer removal.
		setAnnotation("")
		Expect(feast.deployDataRegistry()).To(Succeed())

		updated := &feastdevv1.FeatureStore{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(updated, DataRegistryFinalizer)).To(BeFalse(),
			"DataRegistryFinalizer should be removed after annotation is cleared")
	})

	It("cleans up cluster-scoped resources when finalizer is handled on CR deletion", func() {
		isOpenShift = false

		setAnnotation("true")

		sa := feast.initFeastSA()
		sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))
		_ = k8sClient.Create(ctx, sa)

		// Deploy so ClusterRoles exist.
		Expect(feast.deployDataRegistry()).To(Succeed())

		// Verify ClusterRoles were created.
		for _, suffix := range []string{"viewer", "editor", "admin"} {
			cr := feast.initDataRegistryClusterRole(suffix)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name}, cr)).To(Succeed())
		}

		// Simulate the finalizer handling path (what Reconcile does on deletion).
		feast.refreshFeatureStore(ctx, typeNamespacedName)
		Expect(controllerutil.ContainsFinalizer(feast.Handler.FeatureStore, DataRegistryFinalizer)).To(BeTrue())

		Expect(feast.CleanupDataRegistryClusterRoles()).To(Succeed())
		Expect(feast.CleanupDataRegistryAuthDelegatorBinding()).To(Succeed())
		controllerutil.RemoveFinalizer(feast.Handler.FeatureStore, DataRegistryFinalizer)
		Expect(k8sClient.Update(ctx, feast.Handler.FeatureStore)).To(Succeed())

		// Verify ClusterRoles are gone.
		for _, suffix := range []string{"viewer", "editor", "admin"} {
			cr := feast.initDataRegistryClusterRole(suffix)
			err := k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name}, cr)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"ClusterRole %s should have been deleted", cr.Name)
		}

		// Verify finalizer was removed.
		updated := &feastdevv1.FeatureStore{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(updated, DataRegistryFinalizer)).To(BeFalse())
	})

	It("exposes metrics port on data-registry container and Service", func() {
		isOpenShift = false
		setAnnotation("true")

		// Verify container has metrics port.
		ctr, err := feast.buildDataRegistryContainer()
		Expect(err).NotTo(HaveOccurred())
		var metricsPort *corev1.ContainerPort
		for i := range ctr.Ports {
			if ctr.Ports[i].Name == metricsPortName {
				metricsPort = &ctr.Ports[i]
				break
			}
		}
		Expect(metricsPort).NotTo(BeNil(), "feast-server container should expose 'metrics' port")
		Expect(metricsPort.ContainerPort).To(Equal(MetricsPort))
		Expect(metricsPort.Protocol).To(Equal(corev1.ProtocolTCP))

		// Verify container has resource requests/limits set.
		Expect(ctr.Resources.Requests).To(HaveKey(corev1.ResourceCPU))
		Expect(ctr.Resources.Requests).To(HaveKey(corev1.ResourceMemory))
		Expect(ctr.Resources.Limits).To(HaveKey(corev1.ResourceCPU))
		Expect(ctr.Resources.Limits).To(HaveKey(corev1.ResourceMemory))

		// Verify Service has metrics port.
		svc := feast.initDataRegistrySvc()
		Expect(feast.setDataRegistryService(svc)).To(Succeed())
		var svcMetricsPort *corev1.ServicePort
		for i := range svc.Spec.Ports {
			if svc.Spec.Ports[i].Name == metricsPortName {
				svcMetricsPort = &svc.Spec.Ports[i]
				break
			}
		}
		Expect(svcMetricsPort).NotTo(BeNil(), "data-registry Service should expose 'metrics' port")
		Expect(svcMetricsPort.Port).To(Equal(MetricsPort))
		Expect(svcMetricsPort.TargetPort).To(Equal(intstr.FromInt32(MetricsPort)))
	})
})
