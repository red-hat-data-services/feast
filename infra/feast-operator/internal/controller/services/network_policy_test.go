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

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/handler"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	npac "k8s.io/client-go/applyconfigurations/networking/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		ctx = context.Background()
		featureStore = newTestFeatureStore("np-test-fs", DefaultNs)
		Expect(k8sClient.Create(ctx, featureStore)).To(Succeed())
		feast = setupFeastServices(ctx, featureStore)
	})

	AfterEach(func() {
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

	Describe("buildDashboardIngressRules", func() {
		It("should create a rule with namespace+pod selectors for Dashboard", func() {
			ports := feast.collectDashboardPorts()
			rules := buildDashboardIngressRules(ports)
			Expect(rules).To(HaveLen(2))
			for i, podLabel := range []string{dashboardPodLabel, dashboardPodLabelCurrent} {
				Expect(rules[i].From).To(HaveLen(1))
				peer := rules[i].From[0]
				Expect(peer.NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
					dashboardNamespaceLabel, dashboardNamespaceValue,
				))
				Expect(peer.PodSelector.MatchLabels).To(HaveKeyWithValue(podLabel, "true"))
			}
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
			Expect(dashNP.Spec.Ingress).To(HaveLen(2))
			Expect(dashNP.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
				dashboardNamespaceLabel, dashboardNamespaceValue,
			))

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
			egressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixEgress, featureStore.Namespace)
			Expect(egressNP).To(BeNil())
			cronNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixCronJob, featureStore.Namespace)
			Expect(cronNP).To(BeNil())
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
			monNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixMonitoring, featureStore.Namespace)
			Expect(monNP).NotTo(BeNil())
			egressNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixEgress, featureStore.Namespace)
			Expect(egressNP).NotTo(BeNil())
			dataRegNP := getNetworkPolicy(ctx, GetFeastName(featureStore)+npSuffixDataReg, featureStore.Namespace)
			Expect(dataRegNP).NotTo(BeNil())
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
