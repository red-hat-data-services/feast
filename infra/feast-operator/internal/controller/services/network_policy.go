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
	"encoding/json"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	metaac "k8s.io/client-go/applyconfigurations/meta/v1"
	npac "k8s.io/client-go/applyconfigurations/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// NetworkPolicy name suffixes appended to the FeatureStore name.
const (
	npSuffixSameNs     = "-allow-same-ns"
	npSuffixDashboard  = "-allow-dashboard"
	npSuffixWorkbench  = "-allow-workbench"
	npSuffixIngress    = "-allow-ingress-controller"
	npSuffixRemoteReg  = "-allow-remote-registry"
	npSuffixMonitoring = "-allow-monitoring"
	npSuffixDataReg    = "-allow-data-registry"
	npSuffixEgress     = "-allow-egress"
	npSuffixCronJob    = "-cronjob"

	// Labels used in cross-namespace selectors (not owned by us).
	dashboardNamespaceLabel        = "platform.opendatahub.io/part-of"
	dashboardNamespaceValue        = "workbenches"
	dashboardPodLabel              = "app.opendatahub.io/dashboard"
	dashboardPodLabelCurrent       = "app.opendatahub.io/odh-dashboard"
	workbenchFeastIntegrationLabel = "opendatahub.io/feast-integration"
	ingressNamespaceLabel          = "kubernetes.io/metadata.name"
	ingressNamespaceValue          = "ingress-nginx"
	ingressPodNameLabel            = "app.kubernetes.io/name"
	ingressPodNameValue            = "ingress-nginx"
	ingressPodComponentLabel       = "app.kubernetes.io/component"
	ingressPodComponentValue       = "controller"
	openShiftIngressNamespaceLabel = "network.openshift.io/policy-group"
	openShiftIngressNamespaceValue = "ingress"
	monitoringNamespaceLabel       = "network.openshift.io/policy-group"
	monitoringNamespaceValue       = "monitoring"
)

// reconcileNetworkPolicies creates or updates all operand NetworkPolicies for
// a standard FeatureStore deployment (non-data-registry mode).
func (feast *FeastServices) reconcileNetworkPolicies() error {
	cr := feast.Handler.FeatureStore
	logger := log.FromContext(feast.Handler.Context)

	// Remove stale data-registry policy after a mode switch to standard.
	if err := feast.deleteNetworkPolicies(npSuffixDataReg); err != nil {
		return err
	}

	ports := feast.collectEnabledServicePorts()
	if len(ports) == 0 {
		return feast.deleteNetworkPolicies(npSuffixSameNs, npSuffixDashboard, npSuffixWorkbench, npSuffixIngress, npSuffixRemoteReg, npSuffixMonitoring)
	}

	selectorLabels := feast.getSelectorLabels()

	// Same-namespace catch-all: podSelector: {} → all Feast service ports.
	// Covers Workbenches, CronJobs, Spark, Ray, intra-Feast.
	if err := feast.applyNetworkPolicy(
		GetFeastName(cr)+npSuffixSameNs,
		selectorLabels,
		buildSameNamespaceIngressRules(ports),
	); err != nil {
		return err
	}

	// Dashboard cross-namespace → Registry REST.
	dashPorts := feast.collectDashboardPorts()
	if len(dashPorts) > 0 {
		if err := feast.applyNetworkPolicy(
			GetFeastName(cr)+npSuffixDashboard,
			selectorLabels,
			buildDashboardIngressRules(dashPorts),
		); err != nil {
			return err
		}
	} else {
		if err := feast.deleteNetworkPolicies(npSuffixDashboard); err != nil {
			return err
		}
	}

	// Workbenches opt in through the Dashboard-managed Feast integration label.
	// The peer combines an empty namespace selector with the pod label so that
	// only opted-in pods in any namespace can reach Feast API ports.
	workbenchPorts := feast.collectWorkbenchPorts()
	if len(workbenchPorts) > 0 {
		if err := feast.applyNetworkPolicy(
			GetFeastName(cr)+npSuffixWorkbench,
			selectorLabels,
			buildWorkbenchIngressRules(workbenchPorts),
		); err != nil {
			return err
		}
	} else if err := feast.deleteNetworkPolicies(npSuffixWorkbench); err != nil {
		return err
	}

	// The ingress-nginx controller forwards Registry REST requests from its
	// namespace. OpenShift's router also needs access to the UI Route backend.
	// Keep each source restricted to the ports it actually serves.
	ingressPorts := feast.collectDashboardPorts()
	openShiftRouterPorts := feast.collectOpenShiftRouterPorts()
	if len(ingressPorts) > 0 || len(openShiftRouterPorts) > 0 {
		var ingressRules []*npac.NetworkPolicyIngressRuleApplyConfiguration
		if len(ingressPorts) > 0 {
			ingressRules = append(ingressRules, buildIngressControllerRules(ingressPorts)...)
		}
		if len(openShiftRouterPorts) > 0 {
			ingressRules = append(ingressRules, buildOpenShiftRouterRules(openShiftRouterPorts)...)
		}
		if err := feast.applyNetworkPolicy(
			GetFeastName(cr)+npSuffixIngress,
			selectorLabels,
			ingressRules,
		); err != nil {
			return err
		}
	} else if err := feast.deleteNetworkPolicies(npSuffixIngress); err != nil {
		return err
	}

	// Remote FeatureStores reference the local Registry gRPC endpoint across
	// namespaces. Limit that exception to Feast-managed pods and the Registry port.
	remoteRegistryPorts := feast.collectRemoteRegistryPorts()
	if len(remoteRegistryPorts) > 0 {
		if err := feast.applyNetworkPolicy(
			GetFeastName(cr)+npSuffixRemoteReg,
			selectorLabels,
			buildRemoteRegistryIngressRules(remoteRegistryPorts),
		); err != nil {
			return err
		}
	} else if err := feast.deleteNetworkPolicies(npSuffixRemoteReg); err != nil {
		return err
	}

	// Monitoring cross-namespace → metrics port.
	metricsPorts := feast.collectMetricsPorts()
	if len(metricsPorts) > 0 {
		if err := feast.applyNetworkPolicy(
			GetFeastName(cr)+npSuffixMonitoring,
			selectorLabels,
			buildMonitoringIngressRules(metricsPorts),
		); err != nil {
			return err
		}
	} else {
		if err := feast.deleteNetworkPolicies(npSuffixMonitoring); err != nil {
			return err
		}
	}

	if err := feast.applyEgressAllowAllPolicy(
		GetFeastName(cr)+npSuffixEgress,
		selectorLabels,
	); err != nil {
		return err
	}

	// CronJob pods: deny-all ingress (no Service) + allow-all egress.
	cronJobLabels := map[string]string{
		NameLabelKey: feast.GetFeastServiceName(CronJobFeastType),
	}
	if err := feast.applyCronJobNetworkPolicy(
		GetFeastName(cr)+npSuffixCronJob,
		cronJobLabels,
	); err != nil {
		return err
	}

	logger.Info("Reconciled operand NetworkPolicies")
	return nil
}

// reconcileDataRegistryNetworkPolicies creates or updates NetworkPolicies for
// the data-registry deployment mode.
func (feast *FeastServices) reconcileDataRegistryNetworkPolicies() error {
	cr := feast.Handler.FeatureStore
	logger := log.FromContext(feast.Handler.Context)

	// Must match actual data-registry pod labels, not the generated Service name.
	selectorLabels := map[string]string{
		NameLabelKey:        cr.Name,
		ServiceTypeLabelKey: string(DataRegistryFeastType),
	}

	proxyPorts := []*npac.NetworkPolicyPortApplyConfiguration{
		npPort(DataRegistryProxyPort),
	}

	// Dashboard cross-namespace → data-registry proxy.
	if err := feast.applyNetworkPolicy(
		GetFeastName(cr)+npSuffixDataReg,
		selectorLabels,
		buildDashboardIngressRules(proxyPorts),
	); err != nil {
		return err
	}

	// Same-namespace access for pods in the platform namespace.
	if err := feast.applyNetworkPolicy(
		GetFeastName(cr)+npSuffixSameNs,
		selectorLabels,
		buildSameNamespaceIngressRules(proxyPorts),
	); err != nil {
		return err
	}
	if err := feast.applyNetworkPolicy(
		GetFeastName(cr)+npSuffixWorkbench,
		selectorLabels,
		buildWorkbenchIngressRules(proxyPorts),
	); err != nil {
		return err
	}

	// Monitoring → data-registry metrics.
	monPorts := []*npac.NetworkPolicyPortApplyConfiguration{
		npPort(MetricsPort),
	}
	if err := feast.applyNetworkPolicy(
		GetFeastName(cr)+npSuffixMonitoring,
		selectorLabels,
		buildMonitoringIngressRules(monPorts),
	); err != nil {
		return err
	}

	if err := feast.applyEgressAllowAllPolicy(
		GetFeastName(cr)+npSuffixEgress,
		selectorLabels,
	); err != nil {
		return err
	}

	// OpenShift router → data-registry proxy (Route backend).
	if isOpenShift {
		if err := feast.applyNetworkPolicy(
			GetFeastName(cr)+npSuffixIngress,
			selectorLabels,
			buildOpenShiftRouterRules(proxyPorts),
		); err != nil {
			return err
		}
	} else if err := feast.deleteNetworkPolicies(npSuffixIngress); err != nil {
		return err
	}

	// Clean up standard-mode-only policies.
	if err := feast.deleteNetworkPolicies(npSuffixDashboard, npSuffixRemoteReg); err != nil {
		return err
	}

	logger.Info("Reconciled data-registry NetworkPolicies")
	return nil
}

// deleteAllNetworkPolicies removes all operand NetworkPolicies owned by
// this FeatureStore.
func (feast *FeastServices) deleteAllNetworkPolicies() error {
	return feast.deleteNetworkPolicies(npSuffixSameNs, npSuffixDashboard, npSuffixWorkbench, npSuffixIngress, npSuffixRemoteReg, npSuffixMonitoring, npSuffixDataReg, npSuffixEgress, npSuffixCronJob)
}

// --- Port collection helpers ---

func npPort(port int32) *npac.NetworkPolicyPortApplyConfiguration {
	p := intstr.FromInt32(port)
	return npac.NetworkPolicyPort().WithProtocol(corev1.ProtocolTCP).WithPort(p)
}

func (feast *FeastServices) collectEnabledServicePorts() []*npac.NetworkPolicyPortApplyConfiguration {
	var ports []*npac.NetworkPolicyPortApplyConfiguration
	applied := feast.Handler.FeatureStore.Status.Applied

	if applied.Services == nil {
		return ports
	}

	if feast.isOnlineServer() {
		ports = append(ports, npPort(getTargetPort(OnlineFeastType, applied.Services.OnlineStore.Server.TLS)))
	}
	if feast.isOfflineServer() {
		ports = append(ports, npPort(getTargetPort(OfflineFeastType, applied.Services.OfflineStore.Server.TLS)))
	}
	if feast.isRegistryServer() {
		tls := feast.getRegistryServerTLS()
		if feast.isRegistryGrpcEnabled() {
			ports = append(ports, npPort(getTargetPort(RegistryFeastType, tls)))
		}
		if feast.isRegistryRestEnabled() {
			ports = append(ports, npPort(getTargetRestPort(RegistryFeastType, tls)))
		}
	}
	if feast.isLineageServer() {
		var tls *feastdevv1.TlsConfigs
		if svr := applied.OpenLineage.Consumer.LineageServer.Server; svr != nil {
			tls = svr.TLS
		}
		ports = append(ports, npPort(getTargetPort(LineageFeastType, tls)))
	}
	if feast.isUiServer() {
		ports = append(ports, npPort(getTargetPort(UIFeastType, applied.Services.UI.TLS)))
	}
	if feast.isMcpServer() {
		ports = append(ports, npPort(getTargetPort(McpServerFeastType, applied.Services.McpServer.TLS)))
	}

	return ports
}

func (feast *FeastServices) collectDashboardPorts() []*npac.NetworkPolicyPortApplyConfiguration {
	var ports []*npac.NetworkPolicyPortApplyConfiguration
	if feast.isRegistryServer() && feast.isRegistryRestEnabled() {
		ports = append(ports, npPort(getTargetRestPort(RegistryFeastType, feast.getRegistryServerTLS())))
	}
	return ports
}

func (feast *FeastServices) collectWorkbenchPorts() []*npac.NetworkPolicyPortApplyConfiguration {
	var ports []*npac.NetworkPolicyPortApplyConfiguration
	applied := feast.Handler.FeatureStore.Status.Applied

	if applied.Services == nil {
		return ports
	}
	if feast.isOnlineServer() {
		ports = append(ports, npPort(getTargetPort(OnlineFeastType, applied.Services.OnlineStore.Server.TLS)))
	}
	if feast.isOfflineServer() {
		ports = append(ports, npPort(getTargetPort(OfflineFeastType, applied.Services.OfflineStore.Server.TLS)))
	}
	if feast.isRegistryServer() {
		tls := feast.getRegistryServerTLS()
		if feast.isRegistryGrpcEnabled() {
			ports = append(ports, npPort(getTargetPort(RegistryFeastType, tls)))
		}
		if feast.isRegistryRestEnabled() {
			ports = append(ports, npPort(getTargetRestPort(RegistryFeastType, tls)))
		}
	}
	if feast.isLineageServer() {
		var tls *feastdevv1.TlsConfigs
		if server := applied.OpenLineage.Consumer.LineageServer.Server; server != nil {
			tls = server.TLS
		}
		ports = append(ports, npPort(getTargetPort(LineageFeastType, tls)))
	}

	return ports
}

func (feast *FeastServices) collectOpenShiftRouterPorts() []*npac.NetworkPolicyPortApplyConfiguration {
	if feast.isUiServer() {
		return []*npac.NetworkPolicyPortApplyConfiguration{
			npPort(getTargetPort(UIFeastType, feast.Handler.FeatureStore.Status.Applied.Services.UI.TLS)),
		}
	}
	return nil
}

func (feast *FeastServices) collectRemoteRegistryPorts() []*npac.NetworkPolicyPortApplyConfiguration {
	if feast.isRegistryServer() && feast.isRegistryGrpcEnabled() {
		return []*npac.NetworkPolicyPortApplyConfiguration{
			npPort(getTargetPort(RegistryFeastType, feast.getRegistryServerTLS())),
		}
	}
	return nil
}

func (feast *FeastServices) collectMetricsPorts() []*npac.NetworkPolicyPortApplyConfiguration {
	var ports []*npac.NetworkPolicyPortApplyConfiguration
	if feast.isMetricsEnabled(OnlineFeastType) {
		ports = append(ports, npPort(MetricsPort))
	}
	return ports
}

func (feast *FeastServices) getRegistryServerTLS() *feastdevv1.TlsConfigs {
	reg := feast.Handler.FeatureStore.Status.Applied.Services.Registry
	if reg != nil && reg.Local != nil && reg.Local.Server != nil {
		return reg.Local.Server.TLS
	}
	return nil
}

// --- Ingress rule builders ---

func buildSameNamespaceIngressRules(ports []*npac.NetworkPolicyPortApplyConfiguration) []*npac.NetworkPolicyIngressRuleApplyConfiguration {
	return []*npac.NetworkPolicyIngressRuleApplyConfiguration{
		npac.NetworkPolicyIngressRule().
			WithFrom(npac.NetworkPolicyPeer().
				WithPodSelector(metaac.LabelSelector())).
			WithPorts(ports...),
	}
}

func buildDashboardIngressRules(ports []*npac.NetworkPolicyPortApplyConfiguration) []*npac.NetworkPolicyIngressRuleApplyConfiguration {
	return []*npac.NetworkPolicyIngressRuleApplyConfiguration{
		dashboardIngressRule(dashboardPodLabel, ports),
		// Dashboard deployments in the current ODH release use the more specific
		// label below. Keep the older label for releases that still emit it.
		dashboardIngressRule(dashboardPodLabelCurrent, ports),
	}
}

func buildWorkbenchIngressRules(ports []*npac.NetworkPolicyPortApplyConfiguration) []*npac.NetworkPolicyIngressRuleApplyConfiguration {
	return []*npac.NetworkPolicyIngressRuleApplyConfiguration{
		npac.NetworkPolicyIngressRule().
			WithFrom(npac.NetworkPolicyPeer().
				WithNamespaceSelector(metaac.LabelSelector()).
				WithPodSelector(metaac.LabelSelector().
					WithMatchLabels(map[string]string{
						workbenchFeastIntegrationLabel: "true",
					}))).
			WithPorts(ports...),
	}
}

func dashboardIngressRule(podLabel string, ports []*npac.NetworkPolicyPortApplyConfiguration) *npac.NetworkPolicyIngressRuleApplyConfiguration {
	return npac.NetworkPolicyIngressRule().
		WithFrom(npac.NetworkPolicyPeer().
			WithNamespaceSelector(metaac.LabelSelector().
				WithMatchLabels(map[string]string{
					dashboardNamespaceLabel: dashboardNamespaceValue,
				})).
			WithPodSelector(metaac.LabelSelector().
				WithMatchLabels(map[string]string{
					podLabel: "true",
				}))).
		WithPorts(ports...)
}

func buildIngressControllerRules(ports []*npac.NetworkPolicyPortApplyConfiguration) []*npac.NetworkPolicyIngressRuleApplyConfiguration {
	return []*npac.NetworkPolicyIngressRuleApplyConfiguration{
		npac.NetworkPolicyIngressRule().
			WithFrom(npac.NetworkPolicyPeer().
				WithNamespaceSelector(metaac.LabelSelector().
					WithMatchLabels(map[string]string{
						ingressNamespaceLabel: ingressNamespaceValue,
					})).
				WithPodSelector(metaac.LabelSelector().
					WithMatchLabels(map[string]string{
						ingressPodNameLabel:      ingressPodNameValue,
						ingressPodComponentLabel: ingressPodComponentValue,
					}))).
			WithPorts(ports...),
	}
}

func buildOpenShiftRouterRules(ports []*npac.NetworkPolicyPortApplyConfiguration) []*npac.NetworkPolicyIngressRuleApplyConfiguration {
	return []*npac.NetworkPolicyIngressRuleApplyConfiguration{
		npac.NetworkPolicyIngressRule().
			WithFrom(npac.NetworkPolicyPeer().
				WithNamespaceSelector(metaac.LabelSelector().
					WithMatchLabels(map[string]string{
						openShiftIngressNamespaceLabel: openShiftIngressNamespaceValue,
					}))).
			WithPorts(ports...),
	}
}

func buildRemoteRegistryIngressRules(ports []*npac.NetworkPolicyPortApplyConfiguration) []*npac.NetworkPolicyIngressRuleApplyConfiguration {
	return []*npac.NetworkPolicyIngressRuleApplyConfiguration{
		npac.NetworkPolicyIngressRule().
			WithFrom(npac.NetworkPolicyPeer().
				WithNamespaceSelector(metaac.LabelSelector()).
				WithPodSelector(metaac.LabelSelector().
					WithMatchLabels(map[string]string{
						ManagedByLabelKey: ManagedByLabelValue,
					}))).
			WithPorts(ports...),
	}
}

func buildMonitoringIngressRules(ports []*npac.NetworkPolicyPortApplyConfiguration) []*npac.NetworkPolicyIngressRuleApplyConfiguration {
	return []*npac.NetworkPolicyIngressRuleApplyConfiguration{
		npac.NetworkPolicyIngressRule().
			WithFrom(npac.NetworkPolicyPeer().
				WithNamespaceSelector(metaac.LabelSelector().
					WithMatchLabels(map[string]string{
						monitoringNamespaceLabel: monitoringNamespaceValue,
					}))).
			WithPorts(ports...),
	}
}

// --- SSA apply / delete helpers ---

func (feast *FeastServices) applyNetworkPolicy(
	name string,
	podSelectorLabels map[string]string,
	ingressRules []*npac.NetworkPolicyIngressRuleApplyConfiguration,
) error {
	cr := feast.Handler.FeatureStore
	policyType := networkingv1.PolicyTypeIngress

	ac := npac.NetworkPolicy(name, cr.Namespace).
		WithLabels(feast.getLabels()).
		WithOwnerReferences(
			metaac.OwnerReference().
				WithAPIVersion(feastdevv1.GroupVersion.String()).
				WithKind("FeatureStore").
				WithName(cr.Name).
				WithUID(cr.UID).
				WithController(true).
				WithBlockOwnerDeletion(true),
		).
		WithSpec(npac.NetworkPolicySpec().
			WithPodSelector(metaac.LabelSelector().
				WithMatchLabels(podSelectorLabels)).
			WithPolicyTypes(policyType).
			WithIngress(ingressRules...),
		)

	data, err := json.Marshal(ac)
	if err != nil {
		return err
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace},
	}
	np.SetGroupVersionKind(networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"))

	if err := feast.Handler.Client.Patch(feast.Handler.Context, np,
		client.RawPatch(types.ApplyPatchType, data),
		client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return err
	}

	log.FromContext(feast.Handler.Context).Info("Successfully applied", "NetworkPolicy", name)
	return nil
}

// applyCronJobNetworkPolicy creates a combined policy for materialization
// CronJob pods: deny-all ingress (pods expose no Service) and allow-all
// egress (pods use kubectl exec to the Feast deployment via the API server,
// and Spark/Ray jobs connect to user-configured data stores).
func (feast *FeastServices) applyCronJobNetworkPolicy(
	name string,
	podSelectorLabels map[string]string,
) error {
	cr := feast.Handler.FeatureStore

	ac := npac.NetworkPolicy(name, cr.Namespace).
		WithLabels(feast.getLabels()).
		WithOwnerReferences(
			metaac.OwnerReference().
				WithAPIVersion(feastdevv1.GroupVersion.String()).
				WithKind("FeatureStore").
				WithName(cr.Name).
				WithUID(cr.UID).
				WithController(true).
				WithBlockOwnerDeletion(true),
		).
		WithSpec(npac.NetworkPolicySpec().
			WithPodSelector(metaac.LabelSelector().
				WithMatchLabels(podSelectorLabels)).
			WithPolicyTypes(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress).
			WithEgress(npac.NetworkPolicyEgressRule()),
		)

	data, err := json.Marshal(ac)
	if err != nil {
		return err
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace},
	}
	np.SetGroupVersionKind(networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"))

	if err := feast.Handler.Client.Patch(feast.Handler.Context, np,
		client.RawPatch(types.ApplyPatchType, data),
		client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return err
	}

	log.FromContext(feast.Handler.Context).Info("Successfully applied", "NetworkPolicy", name)
	return nil
}

// applyEgressAllowAllPolicy creates a policy that permits unrestricted
// outbound traffic from the selected Feast operand pods (ADR-0016 §Explicit
// egress behavior).
//
// Narrower egress rules are impractical because Feast pods connect to
// user-configured offline stores (S3, BigQuery, Redshift, Snowflake,
// Postgres), online stores (Redis, DynamoDB, Bigtable, Cassandra), cloud
// IAM/metadata endpoints, and SQL-backed registries whose addresses are
// resolved at runtime from feature_store.yaml and cannot be enumerated at
// policy-creation time.
//
// External controls relied on: TLS for store connections when configured,
// cloud IAM authentication for managed services, Kubernetes RBAC for
// API-server access, and OpenShift OAuth/OIDC for user-facing endpoints.
func (feast *FeastServices) applyEgressAllowAllPolicy(
	name string,
	podSelectorLabels map[string]string,
) error {
	cr := feast.Handler.FeatureStore

	ac := npac.NetworkPolicy(name, cr.Namespace).
		WithLabels(feast.getLabels()).
		WithOwnerReferences(
			metaac.OwnerReference().
				WithAPIVersion(feastdevv1.GroupVersion.String()).
				WithKind("FeatureStore").
				WithName(cr.Name).
				WithUID(cr.UID).
				WithController(true).
				WithBlockOwnerDeletion(true),
		).
		WithSpec(npac.NetworkPolicySpec().
			WithPodSelector(metaac.LabelSelector().
				WithMatchLabels(podSelectorLabels)).
			WithPolicyTypes(networkingv1.PolicyTypeEgress).
			WithEgress(npac.NetworkPolicyEgressRule()),
		)

	data, err := json.Marshal(ac)
	if err != nil {
		return err
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace},
	}
	np.SetGroupVersionKind(networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"))

	if err := feast.Handler.Client.Patch(feast.Handler.Context, np,
		client.RawPatch(types.ApplyPatchType, data),
		client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return err
	}

	log.FromContext(feast.Handler.Context).Info("Successfully applied", "NetworkPolicy", name)
	return nil
}

func (feast *FeastServices) deleteNetworkPolicies(suffixes ...string) error {
	cr := feast.Handler.FeatureStore
	for _, suffix := range suffixes {
		np := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GetFeastName(cr) + suffix,
				Namespace: cr.Namespace,
			},
		}
		np.SetGroupVersionKind(networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"))
		if err := feast.Handler.DeleteOwnedFeastObj(np); err != nil {
			return err
		}
	}
	return nil
}
