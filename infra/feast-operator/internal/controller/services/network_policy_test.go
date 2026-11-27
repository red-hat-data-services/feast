/*
Copyright 2026 Feast Community.

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
	"os"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/handler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	npac "k8s.io/client-go/applyconfigurations/networking/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func newTestFeatureStore(name, namespace string) *feastdevv1.FeatureStore {
	return &feastdevv1.FeatureStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: feastdevv1.FeatureStoreSpec{
			FeastProject: "np_test_project",
			Services: &feastdevv1.FeatureStoreServices{
				OnlineStore: &feastdevv1.OnlineStore{
					Server: &feastdevv1.ServerConfigs{
						ContainerConfigs: feastdevv1.ContainerConfigs{
							DefaultCtrConfigs: feastdevv1.DefaultCtrConfigs{
								Image: ptr.To("test-image"),
							},
						},
					},
				},
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
							RestAPI: ptr.To(true),
						},
					},
				},
			},
		},
	}
}

func setupFeastServices(ctx context.Context, featureStore *feastdevv1.FeatureStore) *FeastServices {
	feast := &FeastServices{
		Handler: handler.FeastHandler{
			Client:       k8sClient,
			Context:      ctx,
			Scheme:       k8sClient.Scheme(),
			FeatureStore: featureStore,
		},
	}
	Expect(feast.ApplyDefaults()).To(Succeed())
	applySpecToStatus(featureStore)
	key := types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace}
	feast.refreshFeatureStore(ctx, key)
	return feast
}

func expectDashboardPodLabels(ports []*npac.NetworkPolicyPortApplyConfiguration, podLabels []string) {
	rules := buildDashboardIngressRules(ports)
	Expect(rules).To(HaveLen(len(podLabels)))
	for i, podLabel := range podLabels {
		Expect(rules[i].From).To(HaveLen(2))
		Expect(rules[i].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			dashboardNamespaceLabel, dashboardNamespaceValue,
		))
		Expect(rules[i].From[1].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			customAppNamespaceLabel, customAppNamespaceValue,
		))
		for _, peer := range rules[i].From {
			Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(podLabel, "true"))
		}
		Expect(rules[i].Ports).To(HaveLen(len(ports)))
		Expect(rules[i].Ports[0].Port.IntVal).To(Equal(ports[0].Port.IntVal))
	}
}

func withPlatformTypeEnv(value string) {
	original, existed := os.LookupEnv(platformTypeEnv)
	if value == "" {
		Expect(os.Unsetenv(platformTypeEnv)).To(Succeed())
	} else {
		Expect(os.Setenv(platformTypeEnv, value)).To(Succeed())
	}
	DeferCleanup(func() {
		if existed {
			Expect(os.Setenv(platformTypeEnv, original)).To(Succeed())
		} else {
			Expect(os.Unsetenv(platformTypeEnv)).To(Succeed())
		}
	})
}

func getNetworkPolicy(ctx context.Context, name, namespace string) *networkingv1.NetworkPolicy {
	np := &networkingv1.NetworkPolicy{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, np)
	if err != nil {
		return nil
	}
	return np
}

var _ = Describe("NetworkPolicy", func() {
	var (
		featureStore *feastdevv1.FeatureStore
		feast        *FeastServices
		ctx          context.Context
	)

	BeforeEach(func() {
		originalNamespace, hadNamespace := os.LookupEnv("POD_NAMESPACE")
		Expect(os.Setenv("POD_NAMESPACE", "feast-operator-system")).To(Succeed())
		DeferCleanup(func() {
			if hadNamespace {
				Expect(os.Setenv("POD_NAMESPACE", originalNamespace)).To(Succeed())
			} else {
				Expect(os.Unsetenv("POD_NAMESPACE")).To(Succeed())
			}
		})
		ctx = context.Background()
		featureStore = newTestFeatureStore("np-test-fs", DefaultNs)
		Expect(k8sClient.Create(ctx, featureStore)).To(Succeed())
		feast = setupFeastServices(ctx, featureStore)
	})

	AfterEach(func() {
		resetInstallPlatformForTest()
		Expect(feast.deleteAllNetworkPolicies()).To(Succeed())
		Expect(k8sClient.Delete(ctx, featureStore)).To(Succeed())
	})

	Describe("collectEnabledServicePorts", func() {
		It("should collect ports for online server and registry gRPC+REST", func() {
			ports := feast.collectEnabledServicePorts()
			Expect(ports).To(HaveLen(3))
		})

		It("should use HTTPS ports when TLS is configured", func() {
			featureStore.Spec.Services.OnlineStore.Server.TLS = &feastdevv1.TlsConfigs{
				SecretRef: &corev1.LocalObjectReference{Name: "tls-secret"},
			}
			Expect(feast.ApplyDefaults()).To(Succeed())
			applySpecToStatus(featureStore)
			key := types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace}
			feast.refreshFeatureStore(ctx, key)

			ports := feast.collectEnabledServicePorts()
			var onlinePort int32
			for _, p := range ports {
				if p.Port != nil && p.Port.IntVal == FeastServiceConstants[OnlineFeastType].TargetHttpsPort {
					onlinePort = p.Port.IntVal
				}
			}
			Expect(onlinePort).To(Equal(FeastServiceConstants[OnlineFeastType].TargetHttpsPort))
		})
	})

	Describe("collectDashboardPorts", func() {
		It("should return registry REST port when REST API is enabled", func() {
			ports := feast.collectDashboardPorts()
			Expect(ports).To(HaveLen(1))
			Expect(ports[0].Port.IntVal).To(Equal(
				FeastServiceConstants[RegistryFeastType].TargetRestHttpPort,
			))
		})

		It("should return empty when REST API is disabled", func() {
			featureStore.Spec.Services.Registry.Local.Server.RestAPI = ptr.To(false)
			Expect(feast.ApplyDefaults()).To(Succeed())
			applySpecToStatus(featureStore)
			key := types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace}
			feast.refreshFeatureStore(ctx, key)

			ports := feast.collectDashboardPorts()
			Expect(ports).To(BeEmpty())
		})
	})

	Describe("collectRemoteRegistryPorts", func() {
		It("should return the registry gRPC port when the local registry is enabled", func() {
			ports := feast.collectRemoteRegistryPorts()
			Expect(ports).To(HaveLen(1))
			Expect(ports[0].Port.IntVal).To(Equal(
				FeastServiceConstants[RegistryFeastType].TargetHttpPort,
			))
		})

		It("should return no ports when the registry gRPC server is disabled", func() {
			featureStore.Spec.Services.Registry.Local.Server.GRPC = ptr.To(false)
			Expect(feast.ApplyDefaults()).To(Succeed())
			applySpecToStatus(featureStore)
			key := types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace}
			feast.refreshFeatureStore(ctx, key)

			Expect(feast.collectRemoteRegistryPorts()).To(BeEmpty())
		})
	})

	Describe("buildSameNamespaceIngressRules", func() {
		It("should create a rule with empty podSelector (all pods in namespace)", func() {
			ports := feast.collectEnabledServicePorts()
			rules := buildSameNamespaceIngressRules(ports)
			Expect(rules).To(HaveLen(1))
			Expect(rules[0].From).To(HaveLen(1))
			// Empty LabelSelector means all pods in namespace
			Expect(rules[0].From[0].PodSelector.MatchLabels).To(BeEmpty())
			Expect(rules[0].Ports).To(HaveLen(len(ports)))
		})
	})

	Describe("install platform detection", func() {
		It("should select the RHOAI dashboard when rhods-operator is installed", func() {
			withPlatformTypeEnv("")
			condition := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "operators.coreos.com/v2",
				"kind":       "OperatorCondition",
				"metadata": map[string]any{
					"name": "rhods-operator.3.6.0",
				},
			}}
			SetInstallPlatformReader(fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(condition).Build())

			feast.ensureInstallPlatform()

			Expect(currentInstallPlatform()).To(Equal(installPlatformRHOAI))
			expectDashboardPodLabels(feast.collectDashboardPorts(), []string{dashboardPodLabelRHOAI})
		})

		It("should select ODH dashboards when no RHOAI operator is present", func() {
			withPlatformTypeEnv("")
			SetInstallPlatformReader(fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build())

			feast.ensureInstallPlatform()

			Expect(currentInstallPlatform()).To(Equal(installPlatformODH))
			expectDashboardPodLabels(feast.collectDashboardPorts(), []string{dashboardPodLabelLegacy, dashboardPodLabelODH})
		})

		It("should honor an explicit ODH_PLATFORM_TYPE", func() {
			withPlatformTypeEnv("ManagedRHOAI")
			SetInstallPlatformReader(fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build())

			feast.ensureInstallPlatform()

			Expect(currentInstallPlatform()).To(Equal(installPlatformRHOAI))
		})
	})

	Describe("buildDashboardIngressRules", func() {
		It("should allow only the RHOAI dashboard on a RHOAI install", func() {
			setInstallPlatformForTest(installPlatformRHOAI)
			expectDashboardPodLabels(feast.collectDashboardPorts(), []string{dashboardPodLabelRHOAI})
		})

		It("should allow legacy and current ODH dashboards on an ODH install", func() {
			setInstallPlatformForTest(installPlatformODH)
			expectDashboardPodLabels(feast.collectDashboardPorts(), []string{dashboardPodLabelLegacy, dashboardPodLabelODH})
		})

		It("should allow every dashboard label when the install type is unknown", func() {
			expectDashboardPodLabels(feast.collectDashboardPorts(), []string{
				dashboardPodLabelLegacy, dashboardPodLabelODH, dashboardPodLabelRHOAI,
			})
		})
	})

	Describe("buildDataRegistryUIIngressRules", func() {
		It("should allow the Data Registry UI from the applications namespace", func() {
			ports := []*npac.NetworkPolicyPortApplyConfiguration{npPort(DataRegistryProxyPort)}
			rules := buildDataRegistryUIIngressRules(ports)
			Expect(rules).To(HaveLen(1))
			Expect(rules[0].From).To(HaveLen(2))
			Expect(rules[0].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
				dashboardNamespaceLabel, dashboardNamespaceValue,
			))
			Expect(rules[0].From[1].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
				customAppNamespaceLabel, customAppNamespaceValue,
			))
			for _, peer := range rules[0].From {
				Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
					dataRegistryUIPodLabel, dataRegistryUIPodValue,
				))
			}
			Expect(rules[0].Ports).To(HaveLen(1))
			Expect(rules[0].Ports[0].Port.IntVal).To(Equal(DataRegistryProxyPort))
		})
	})

	Describe("buildWorkbenchIngressRules", func() {
		It("should allow Feast-integrated Workbench pods from any namespace", func() {
			ports := feast.collectWorkbenchPorts()
			rules := buildWorkbenchIngressRules(ports)
			Expect(rules).To(HaveLen(1))
			Expect(rules[0].From).To(HaveLen(1))
			peer := rules[0].From[0]
			Expect(peer.NamespaceSelector.MatchLabels).To(BeEmpty())
			Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
				workbenchFeastIntegrationLabel, "true",
			))
			Expect(rules[0].Ports).To(HaveLen(len(ports)))
		})
	})

	Describe("buildIngressControllerRules", func() {
		It("should allow only the ingress-nginx controller in its namespace", func() {
			ports := feast.collectDashboardPorts()
			rules := buildIngressControllerRules(ports)
			Expect(rules).To(HaveLen(1))
			Expect(rules[0].From).To(HaveLen(1))
			peer := rules[0].From[0]
			Expect(peer.NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
				ingressNamespaceLabel, ingressNamespaceValue,
			))
			Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
				ingressPodNameLabel, ingressPodNameValue,
			))
			Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
				ingressPodComponentLabel, ingressPodComponentValue,
			))
			Expect(rules[0].Ports).To(HaveLen(1))
			Expect(rules[0].Ports[0].Port.IntVal).To(Equal(
				FeastServiceConstants[RegistryFeastType].TargetRestHttpPort,
			))
		})
	})

	Describe("buildRemoteRegistryIngressRules", func() {
		It("should allow Feast-managed pods in other namespaces on registry gRPC only", func() {
			ports := feast.collectRemoteRegistryPorts()
			rules := buildRemoteRegistryIngressRules(ports)
			Expect(rules).To(HaveLen(1))
			Expect(rules[0].From).To(HaveLen(1))
			peer := rules[0].From[0]
			Expect(peer.NamespaceSelector.MatchLabels).To(BeEmpty())
			Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(
				ManagedByLabelKey, ManagedByLabelValue,
			))
			Expect(rules[0].Ports).To(HaveLen(1))
			Expect(rules[0].Ports[0].Port.IntVal).To(Equal(
				FeastServiceConstants[RegistryFeastType].TargetHttpPort,
			))
		})
	})

	Describe("buildMonitoringIngressRules", func() {
		It("should create a rule with monitoring namespace selector", func() {
			ports := []*npac.NetworkPolicyPortApplyConfiguration{npPort(MetricsPort)}
			rules := buildMonitoringIngressRules(ports)
			Expect(rules).To(HaveLen(1))
			Expect(rules[0].From).To(HaveLen(1))
			Expect(rules[0].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
				monitoringNamespaceLabel, monitoringNamespaceValue,
			))
			Expect(rules[0].From[0].PodSelector).To(BeNil())
		})
	})

	Describe("reconcileNetworkPolicies", func() {
		It("should create scoped ingress policies for standard deployment", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			sameNsNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())
			Expect(sameNsNP.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
			Expect(sameNsNP.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(
				NameLabelKey, featureStore.Name,
			))
			// Same-ns rule allows all pods in namespace
			Expect(sameNsNP.Spec.Ingress).To(HaveLen(1))
			Expect(sameNsNP.Spec.Ingress[0].From).To(HaveLen(1))

			dashNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDashboard, featureStore.Namespace)
			Expect(dashNP).NotTo(BeNil())
			// Install type is unknown in this test, so every dashboard peer is kept.
			Expect(dashNP.Spec.Ingress).To(HaveLen(3))
			expectedDashLabels := []string{dashboardPodLabelLegacy, dashboardPodLabelODH, dashboardPodLabelRHOAI}
			for i, podLabel := range expectedDashLabels {
				Expect(dashNP.Spec.Ingress[i].From).To(HaveLen(2))
				Expect(dashNP.Spec.Ingress[i].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
					dashboardNamespaceLabel, dashboardNamespaceValue,
				))
				Expect(dashNP.Spec.Ingress[i].From[1].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
					customAppNamespaceLabel, customAppNamespaceValue,
				))
				for _, peer := range dashNP.Spec.Ingress[i].From {
					Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(podLabel, "true"))
				}
			}

			workbenchNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixWorkbench, featureStore.Namespace)
			Expect(workbenchNP).NotTo(BeNil())
			Expect(workbenchNP.Spec.Ingress).To(HaveLen(1))
			workbenchPeer := workbenchNP.Spec.Ingress[0].From[0]
			Expect(workbenchPeer.NamespaceSelector.MatchLabels).To(BeEmpty())
			Expect(workbenchPeer.PodSelector.MatchLabels).To(HaveKeyWithValue(
				workbenchFeastIntegrationLabel, "true",
			))
			Expect(workbenchNP.Spec.Ingress[0].Ports).To(HaveLen(3))

			ingressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixIngress, featureStore.Namespace)
			Expect(ingressNP).NotTo(BeNil())
			Expect(ingressNP.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
				ingressNamespaceLabel, ingressNamespaceValue,
			))
			Expect(ingressNP.Spec.Ingress[0].Ports).To(HaveLen(1))
			Expect(ingressNP.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(
				FeastServiceConstants[RegistryFeastType].TargetRestHttpPort,
			))

			remoteRegistryNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixRemoteReg, featureStore.Namespace)
			Expect(remoteRegistryNP).NotTo(BeNil())
			Expect(remoteRegistryNP.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(HaveKeyWithValue(
				ManagedByLabelKey, ManagedByLabelValue,
			))
			Expect(remoteRegistryNP.Spec.Ingress[0].Ports).To(HaveLen(1))
			Expect(remoteRegistryNP.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(
				FeastServiceConstants[RegistryFeastType].TargetHttpPort,
			))

			operatorNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixOperatorReg, featureStore.Namespace)
			Expect(operatorNP).NotTo(BeNil())
			Expect(operatorNP.Spec.Ingress).To(HaveLen(1))
			Expect(operatorNP.Spec.Ingress[0].From).To(HaveLen(1))
			operatorPeer := operatorNP.Spec.Ingress[0].From[0]
			Expect(operatorPeer.NamespaceSelector.MatchLabels).To(Equal(map[string]string{
				operatorNamespaceLabel: "feast-operator-system",
			}))
			Expect(operatorPeer.PodSelector.MatchLabels).To(Equal(map[string]string{
				operatorPodNameLabel:         operatorPodNameValue,
				operatorPodControlPlaneLabel: operatorPodControlPlaneValue,
			}))
			Expect(operatorNP.Spec.Ingress[0].Ports).To(HaveLen(1))
			Expect(operatorNP.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(
				FeastServiceConstants[RegistryFeastType].TargetRestHttpPort,
			))
		})

		It("should use the registry REST TLS target port for the operator", func() {
			featureStore.Spec.Services.Registry.Local.Server.TLS = &feastdevv1.TlsConfigs{
				SecretRef: &corev1.LocalObjectReference{Name: "tls-secret"},
			}
			Expect(feast.ApplyDefaults()).To(Succeed())
			applySpecToStatus(featureStore)
			feast.refreshFeatureStore(ctx, types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace})

			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			operatorNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixOperatorReg, featureStore.Namespace)
			Expect(operatorNP).NotTo(BeNil())
			Expect(operatorNP.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(
				FeastServiceConstants[RegistryFeastType].TargetRestHttpsPort,
			))
		})

		It("should remove the operator allowance when registry REST is disabled", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			featureStore.Spec.Services.Registry.Local.Server.RestAPI = ptr.To(false)
			Expect(feast.ApplyDefaults()).To(Succeed())
			applySpecToStatus(featureStore)
			feast.refreshFeatureStore(ctx, types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace})

			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixOperatorReg, featureStore.Namespace)).To(BeNil())
		})

		It("should reject an empty operator namespace rather than write a broad selector", func() {
			Expect(os.Unsetenv("POD_NAMESPACE")).To(Succeed())
			Expect(feast.reconcileNetworkPolicies()).To(MatchError(ContainSubstring("POD_NAMESPACE is required")))
			Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixOperatorReg, featureStore.Namespace)).To(BeNil())
		})

		It("should not create monitoring policy when metrics is disabled", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			monNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixMonitoring, featureStore.Namespace)
			Expect(monNP).To(BeNil())
		})

		It("should create monitoring policy when metrics is enabled", func() {
			featureStore.Spec.Services.OnlineStore.Server.Metrics = ptr.To(true)
			Expect(feast.ApplyDefaults()).To(Succeed())
			applySpecToStatus(featureStore)
			key := types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace}
			feast.refreshFeatureStore(ctx, key)

			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			monNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixMonitoring, featureStore.Namespace)
			Expect(monNP).NotTo(BeNil())
			Expect(monNP.Spec.Ingress[0].Ports).To(HaveLen(1))
			Expect(monNP.Spec.Ingress[0].Ports[0].Port).To(Equal(ptr.To(intstr.FromInt32(MetricsPort))))
		})

		It("should use correct target ports matching service configuration", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			sameNsNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())

			portValues := make([]int32, 0, len(sameNsNP.Spec.Ingress[0].Ports))
			for _, p := range sameNsNP.Spec.Ingress[0].Ports {
				portValues = append(portValues, p.Port.IntVal)
			}
			Expect(portValues).To(ContainElement(FeastServiceConstants[OnlineFeastType].TargetHttpPort))
			Expect(portValues).To(ContainElement(FeastServiceConstants[RegistryFeastType].TargetHttpPort))
			Expect(portValues).To(ContainElement(FeastServiceConstants[RegistryFeastType].TargetRestHttpPort))
		})

		It("should create egress allow-all policy", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			egressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixEgress, featureStore.Namespace)
			Expect(egressNP).NotTo(BeNil())
			Expect(egressNP.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
			Expect(egressNP.Spec.PolicyTypes).NotTo(ContainElement(networkingv1.PolicyTypeIngress))
			// Single empty egress rule = allow all egress
			Expect(egressNP.Spec.Egress).To(HaveLen(1))
			Expect(egressNP.Spec.Egress[0].Ports).To(BeEmpty())
			Expect(egressNP.Spec.Egress[0].To).To(BeEmpty())
		})

		It("should create CronJob policy with deny-all ingress and allow-all egress", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			cronNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixCronJob, featureStore.Namespace)
			Expect(cronNP).NotTo(BeNil())
			Expect(cronNP.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
			Expect(cronNP.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
			// Deny-all ingress: no ingress rules
			Expect(cronNP.Spec.Ingress).To(BeEmpty())
			// Allow-all egress: single empty rule
			Expect(cronNP.Spec.Egress).To(HaveLen(1))
			Expect(cronNP.Spec.Egress[0].Ports).To(BeEmpty())
			Expect(cronNP.Spec.Egress[0].To).To(BeEmpty())
			// Targets CronJob pods specifically
			Expect(cronNP.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(
				NameLabelKey, feast.GetFeastServiceName(CronJobFeastType),
			))
		})

		It("should set owner reference to the FeatureStore CR", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			sameNsNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())
			Expect(sameNsNP.OwnerReferences).To(HaveLen(1))
			Expect(sameNsNP.OwnerReferences[0].Name).To(Equal(featureStore.Name))
			Expect(sameNsNP.OwnerReferences[0].Kind).To(Equal("FeatureStore"))
			Expect(*sameNsNP.OwnerReferences[0].Controller).To(BeTrue())
		})

		It("should replace ODH and RHOAI peers with the RHOAI dashboard peer", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			dashNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDashboard, featureStore.Namespace)
			Expect(dashNP).NotTo(BeNil())
			Expect(dashNP.Spec.Ingress).To(HaveLen(3))

			setInstallPlatformForTest(installPlatformRHOAI)
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			dashNP = getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDashboard, featureStore.Namespace)
			Expect(dashNP.Spec.Ingress).To(HaveLen(1))
			Expect(dashNP.Spec.Ingress[0].From).To(HaveLen(2))
			for _, peer := range dashNP.Spec.Ingress[0].From {
				Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(dashboardPodLabelRHOAI, "true"))
				Expect(peer.PodSelector.MatchLabels).NotTo(HaveKey(dashboardPodLabelODH))
				Expect(peer.PodSelector.MatchLabels).NotTo(HaveKey(dashboardPodLabelLegacy))
			}
		})

		It("should remove dashboard policy when REST API is disabled", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			dashNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDashboard, featureStore.Namespace)
			Expect(dashNP).NotTo(BeNil())

			featureStore.Spec.Services.Registry.Local.Server.RestAPI = ptr.To(false)
			Expect(feast.ApplyDefaults()).To(Succeed())
			applySpecToStatus(featureStore)
			key := types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace}
			feast.refreshFeatureStore(ctx, key)

			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			dashNP = getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDashboard, featureStore.Namespace)
			Expect(dashNP).To(BeNil())
			ingressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixIngress, featureStore.Namespace)
			Expect(ingressNP).To(BeNil())
		})
	})

	Describe("deleteAllNetworkPolicies", func() {
		It("should remove all policies created by reconcileNetworkPolicies", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			sameNsNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())

			Expect(feast.deleteAllNetworkPolicies()).To(Succeed())

			sameNsNP = getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).To(BeNil())
			dashNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDashboard, featureStore.Namespace)
			Expect(dashNP).To(BeNil())
			ingressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixIngress, featureStore.Namespace)
			Expect(ingressNP).To(BeNil())
			remoteRegistryNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixRemoteReg, featureStore.Namespace)
			Expect(remoteRegistryNP).To(BeNil())
			operatorNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixOperatorReg, featureStore.Namespace)
			Expect(operatorNP).To(BeNil())
			egressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixEgress, featureStore.Namespace)
			Expect(egressNP).To(BeNil())
			cronNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixCronJob, featureStore.Namespace)
			Expect(cronNP).To(BeNil())
		})
	})

	Describe("data-registry cleanup on a standard FeatureStore", func() {
		standardPolicySuffixes := []string{
			npSuffixSameNs,
			npSuffixDashboard,
			npSuffixWorkbench,
			npSuffixIngress,
			npSuffixRemoteReg,
			npSuffixEgress,
			npSuffixCronJob,
		}

		It("keeps standard NetworkPolicies in place", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			uids := map[string]types.UID{}
			for _, suffix := range standardPolicySuffixes {
				np := getNetworkPolicy(ctx, GetFeastName(featureStore)+suffix, featureStore.Namespace)
				Expect(np).NotTo(BeNil(), "policy %s should exist before cleanup", suffix)
				uids[suffix] = np.UID
			}
			Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDataReg, featureStore.Namespace)).To(BeNil())

			savedOpenShift := isOpenShift
			isOpenShift = false
			defer func() { isOpenShift = savedOpenShift }()

			// Deploy() calls this for every FeatureStore that is not a data registry.
			Expect(feast.deployDataRegistry()).To(Succeed())

			for _, suffix := range standardPolicySuffixes {
				np := getNetworkPolicy(ctx, GetFeastName(featureStore)+suffix, featureStore.Namespace)
				Expect(np).NotTo(BeNil(), "policy %s should survive data-registry cleanup", suffix)
				Expect(np.UID).To(Equal(uids[suffix]), "policy %s should keep its identity", suffix)
			}
		})

		It("still removes NetworkPolicies when the standard workload is torn down", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)).NotTo(BeNil())

			savedOpenShift := isOpenShift
			isOpenShift = false
			defer func() { isOpenShift = savedOpenShift }()

			Expect(feast.CleanupStandardResources()).To(Succeed())

			for _, suffix := range standardPolicySuffixes {
				Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+suffix, featureStore.Namespace)).To(BeNil(),
					"policy %s should be removed with the standard workload", suffix)
			}
		})
	})

	Describe("reconcileDataRegistryNetworkPolicies", func() {
		// Other test suites may set isOpenShift = false; restore the original value.
		var savedIsOpenShift bool
		BeforeEach(func() {
			savedIsOpenShift = isOpenShift
			isOpenShift = true
		})
		AfterEach(func() {
			isOpenShift = savedIsOpenShift
		})

		It("should create OpenShift router ingress policy for the data-registry Route", func() {
			Expect(feast.reconcileDataRegistryNetworkPolicies()).To(Succeed())

			ingressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixIngress, featureStore.Namespace)
			Expect(ingressNP).NotTo(BeNil(), "data-registry mode should create an ingress NP for the OpenShift router")
			Expect(ingressNP.Spec.Ingress).To(HaveLen(1))
			Expect(ingressNP.Spec.Ingress[0].From).To(HaveLen(1))
			nsSelector := ingressNP.Spec.Ingress[0].From[0].NamespaceSelector
			Expect(nsSelector).NotTo(BeNil())
			Expect(nsSelector.MatchLabels).To(HaveKeyWithValue(
				openShiftIngressNamespaceLabel, openShiftIngressNamespaceValue,
			))
			Expect(ingressNP.Spec.Ingress[0].Ports).To(HaveLen(1))
			Expect(ingressNP.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(DataRegistryProxyPort))
		})

		It("should create same-namespace, workbench, monitoring, and egress policies", func() {
			Expect(feast.reconcileDataRegistryNetworkPolicies()).To(Succeed())

			sameNsNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())
			workbenchNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixWorkbench, featureStore.Namespace)
			Expect(workbenchNP).NotTo(BeNil())
			Expect(workbenchNP.Spec.Ingress).To(HaveLen(1))
			Expect(workbenchNP.Spec.Ingress[0].Ports).To(HaveLen(1))
			Expect(workbenchNP.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(DataRegistryProxyPort))
			Expect(workbenchNP.Spec.Ingress[0].From).To(HaveLen(2))
			Expect(workbenchNP.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(Equal(map[string]string{
				workbenchFeastIntegrationLabel: "true",
			}))
			Expect(workbenchNP.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).To(BeEmpty())
			Expect(workbenchNP.Spec.Ingress[0].From[1].PodSelector.MatchLabels).To(Equal(map[string]string{
				workbenchPodLabel: "true",
			}))
			monNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixMonitoring, featureStore.Namespace)
			Expect(monNP).NotTo(BeNil())
			egressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixEgress, featureStore.Namespace)
			Expect(egressNP).NotTo(BeNil())
			dataRegNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDataReg, featureStore.Namespace)
			Expect(dataRegNP).NotTo(BeNil())
			Expect(dataRegNP.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(
				NameLabelKey, featureStore.Name,
			))
			Expect(dataRegNP.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(
				ServiceTypeLabelKey, string(DataRegistryFeastType),
			))
			Expect(dataRegNP.Spec.Ingress).To(HaveLen(1))
			Expect(dataRegNP.Spec.Ingress[0].Ports).To(HaveLen(1))
			Expect(dataRegNP.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(DataRegistryProxyPort))
			Expect(dataRegNP.Spec.Ingress[0].From).To(HaveLen(2))
			Expect(dataRegNP.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(HaveKeyWithValue(
				dataRegistryUIPodLabel, dataRegistryUIPodValue,
			))
			Expect(dataRegNP.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
				dashboardNamespaceLabel, dashboardNamespaceValue,
			))
			Expect(dataRegNP.Spec.Ingress[0].From[1].PodSelector.MatchLabels).To(HaveKeyWithValue(
				dataRegistryUIPodLabel, dataRegistryUIPodValue,
			))
			Expect(dataRegNP.Spec.Ingress[0].From[1].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
				customAppNamespaceLabel, customAppNamespaceValue,
			))
		})

		It("should clean up standard-mode-only policies", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			dashNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDashboard, featureStore.Namespace)
			Expect(dashNP).NotTo(BeNil())

			Expect(feast.reconcileDataRegistryNetworkPolicies()).To(Succeed())
			dashNP = getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDashboard, featureStore.Namespace)
			Expect(dashNP).To(BeNil(), "dashboard NP should be deleted in data-registry mode")
			remoteRegNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixRemoteReg, featureStore.Namespace)
			Expect(remoteRegNP).To(BeNil(), "remote-registry NP should be deleted in data-registry mode")
			operatorNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixOperatorReg, featureStore.Namespace)
			Expect(operatorNP).To(BeNil(), "operator REST NP should be deleted in data-registry mode")
		})

		It("should restore the operator REST policy when returning to standard mode", func() {
			Expect(feast.reconcileDataRegistryNetworkPolicies()).To(Succeed())
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixOperatorReg, featureStore.Namespace)).NotTo(BeNil())
			Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDataReg, featureStore.Namespace)).To(BeNil())
		})

		It("removes data-registry NetworkPolicies before releasing the finalizer", func() {
			Expect(feast.reconcileDataRegistryNetworkPolicies()).To(Succeed())

			rejectedSuffixes := []string{
				npSuffixDataReg,
				npSuffixSameNs,
				npSuffixWorkbench,
				npSuffixMonitoring,
				npSuffixEgress,
				npSuffixIngress,
			}
			key := types.NamespacedName{Name: featureStore.Name, Namespace: featureStore.Namespace}
			feast.refreshFeatureStore(ctx, key)
			controllerutil.AddFinalizer(feast.Handler.FeatureStore, DataRegistryFinalizer)
			Expect(k8sClient.Update(ctx, feast.Handler.FeatureStore)).To(Succeed())
			feast.refreshFeatureStore(ctx, key)

			// envtest has no Route API. Skip the OpenShift route delete; the
			// policies under test are already created.
			savedOpenShift := isOpenShift
			isOpenShift = false
			defer func() {
				isOpenShift = savedOpenShift
				latest := &feastdevv1.FeatureStore{}
				if err := k8sClient.Get(ctx, key, latest); err != nil {
					return
				}
				if controllerutil.ContainsFinalizer(latest, DataRegistryFinalizer) {
					controllerutil.RemoveFinalizer(latest, DataRegistryFinalizer)
					Expect(k8sClient.Update(ctx, latest)).To(Succeed())
				}
			}()

			Expect(feast.CleanupDataRegistryResources()).To(Succeed())
			feast.refreshFeatureStore(ctx, key)
			Expect(controllerutil.ContainsFinalizer(feast.Handler.FeatureStore, DataRegistryFinalizer)).To(BeTrue())
			for _, suffix := range rejectedSuffixes {
				Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+suffix, featureStore.Namespace)).NotTo(BeNil(),
					"object cleanup should leave policy %s in place", suffix)
			}

			Expect(feast.CleanupRejectedDataRegistry()).To(Succeed())
			feast.refreshFeatureStore(ctx, key)
			Expect(controllerutil.ContainsFinalizer(feast.Handler.FeatureStore, DataRegistryFinalizer)).To(BeFalse())
			for _, suffix := range rejectedSuffixes {
				Expect(getNetworkPolicy(ctx, GetFeastName(featureStore)+suffix, featureStore.Namespace)).To(BeNil(),
					"rejection should remove policy %s", suffix)
			}
		})
	})

	Describe("SSA drift reconciliation", func() {
		It("should restore policy to desired state after manual modification", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			sameNsNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())
			originalPortCount := len(sameNsNP.Spec.Ingress[0].Ports)

			// Manually add an extra port to simulate drift
			sameNsNP.Spec.Ingress[0].Ports = append(sameNsNP.Spec.Ingress[0].Ports,
				networkingv1.NetworkPolicyPort{
					Protocol: ptr.To(corev1.ProtocolTCP),
					Port:     ptr.To(intstr.FromInt32(9999)),
				})
			Expect(k8sClient.Update(ctx, sameNsNP)).To(Succeed())

			// Re-reconcile should restore the original state
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			sameNsNP = getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())
			Expect(sameNsNP.Spec.Ingress[0].Ports).To(HaveLen(originalPortCount))
		})
	})

	Describe("policy deletion recovery", func() {
		It("should recreate a policy if it is manually deleted", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())

			sameNsNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())

			Expect(k8sClient.Delete(ctx, sameNsNP)).To(Succeed())
			sameNsNP = getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).To(BeNil())

			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			sameNsNP = getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			Expect(sameNsNP).NotTo(BeNil())
		})
	})

	Describe("npPort helper", func() {
		It("should create a TCP port configuration", func() {
			p := npPort(8080)
			Expect(*p.Protocol).To(Equal(corev1.ProtocolTCP))
			Expect(p.Port.IntVal).To(Equal(int32(8080)))
		})
	})

	Describe("multiple FeatureStore instances", func() {
		var (
			featureStore2 *feastdevv1.FeatureStore
			feast2        *FeastServices
		)

		BeforeEach(func() {
			featureStore2 = newTestFeatureStore("np-test-fs2", DefaultNs)
			Expect(k8sClient.Create(ctx, featureStore2)).To(Succeed())
			feast2 = setupFeastServices(ctx, featureStore2)
		})

		AfterEach(func() {
			Expect(feast2.deleteAllNetworkPolicies()).To(Succeed())
			Expect(k8sClient.Delete(ctx, featureStore2)).To(Succeed())
		})

		It("should create independent policies for each FeatureStore", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			Expect(feast2.reconcileNetworkPolicies()).To(Succeed())

			np1 := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			np2 := getNetworkPolicy(ctx, GetFeastName(featureStore2)+npSuffixSameNs, featureStore2.Namespace)
			Expect(np1).NotTo(BeNil())
			Expect(np2).NotTo(BeNil())
			Expect(np1.Name).NotTo(Equal(np2.Name))

			// Verify pod selectors are distinct
			Expect(np1.Spec.PodSelector.MatchLabels[NameLabelKey]).To(Equal(featureStore.Name))
			Expect(np2.Spec.PodSelector.MatchLabels[NameLabelKey]).To(Equal(featureStore2.Name))
		})

		It("should not affect other FeatureStore policies on deletion", func() {
			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			Expect(feast2.reconcileNetworkPolicies()).To(Succeed())

			Expect(feast.deleteAllNetworkPolicies()).To(Succeed())

			np1 := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixSameNs, featureStore.Namespace)
			np2 := getNetworkPolicy(ctx, GetFeastName(featureStore2)+npSuffixSameNs, featureStore2.Namespace)
			Expect(np1).To(BeNil())
			Expect(np2).NotTo(BeNil())
		})
	})

	Describe("unrelated NetworkPolicy preservation", func() {
		It("should not modify NetworkPolicies not owned by this FeatureStore", func() {
			unrelatedNP := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unrelated-policy",
					Namespace: featureStore.Namespace,
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				},
			}
			Expect(k8sClient.Create(ctx, unrelatedNP)).To(Succeed())

			Expect(feast.reconcileNetworkPolicies()).To(Succeed())
			Expect(feast.deleteAllNetworkPolicies()).To(Succeed())

			existing := &networkingv1.NetworkPolicy{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(unrelatedNP), existing)
			Expect(err).NotTo(HaveOccurred())
			Expect(existing.Name).To(Equal("unrelated-policy"))

			Expect(k8sClient.Delete(ctx, unrelatedNP)).To(Succeed())
		})
	})
})
