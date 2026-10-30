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

package controller

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	handler "sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/access"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/authz"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/capabilities"
	feasthandler "github.com/feast-dev/feast/infra/feast-operator/internal/controller/handler"
	feastmetrics "github.com/feast-dev/feast/infra/feast-operator/internal/controller/metrics"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/registry"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/services"
	routev1 "github.com/openshift/api/route/v1"
)

// Constants for requeue
const (
	RequeueDelayError       = 5 * time.Second
	RequeuePeriodicInterval = 30 * time.Second
)

// feastProjectPattern is the CRD pattern on spec.feastProject and status.applied.feastProject.
var feastProjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ensureAppliedFeastProject keeps status.applied.feastProject valid so a status
// update that reports a rejection is accepted by the API server.
func ensureAppliedFeastProject(cr *feastdevv1.FeatureStore) {
	if feastProjectPattern.MatchString(cr.Status.Applied.FeastProject) {
		return
	}
	if feastProjectPattern.MatchString(cr.Spec.FeastProject) {
		cr.Status.Applied.FeastProject = cr.Spec.FeastProject
		return
	}
	cr.Status.Applied.FeastProject = services.DataRegistryProject
}

// FeatureStoreReconciler reconciles a FeatureStore object
type FeatureStoreReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Metrics *feastmetrics.FeatureStoreMetrics
}

// +kubebuilder:rbac:groups=feast.dev,resources=featurestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=feast.dev,resources=featurestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=feast.dev,resources=featurestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;create;update;watch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;persistentvolumeclaims,verbs=get;list;create;update;watch;delete;deletecollection
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;create;update;watch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;create;update;watch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;create;watch;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=create;get;list
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=feast-data-registry-admin;feast-data-registry-editor;feast-data-registry-viewer;feast-discover-namespaces;feast-oidc-token-review;feast-token-review-cluster-role,verbs=update;delete
// bind on ClusterRole system:auth-delegator is listed in config/rbac/role.yaml (controller-gen
// markers cannot express resourceNames containing ':').
// +kubebuilder:rbac:groups=dataregistry.opendatahub.io,resources=registries;namespaces;tables;volumes;generic-tables,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dataregistry.opendatahub.io,resources=connections,verbs=use
// namespaces update is required by access.EnsureNamespaceLabel and
// RemoveNamespaceLabelIfLast, which write the opendatahub.io/feast
// discovery label. Not present upstream; do not drop when syncing.
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;delete;deletecollection
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=sparkoperator.k8s.io,resources=sparkapplications,verbs=create;get;delete
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;create;update;watch;delete
// destinationCACertificate on reencrypt routes requires routes/custom-host.
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create;update
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=mlflow.opendatahub.io,resources=mlflows,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *FeatureStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, recErr error) {
	logger := log.FromContext(ctx)

	// Get the FeatureStore using v1 (storage version)
	cr := &feastdevv1.FeatureStore{}
	err := r.Get(ctx, req.NamespacedName, cr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("FeatureStore CR not found, has been deleted")
			if r.Metrics != nil {
				r.Metrics.DeleteFeatureStore(req.NamespacedName.Namespace, req.NamespacedName.Name)
			}
			r.cleanupOnDeletion(ctx, req.NamespacedName.Namespace, req.NamespacedName.Name)
			// A deleted CR no longer triggers reconciles — the capabilities
			// ConfigMap watch enqueues only existing CRs — so this not-found
			// pass is the last chance to release the ConfigMap finalizer for
			// the module operator uninstall handshake.
			if relErr := r.reconcileCapabilitiesConfigOnCRRemoval(ctx, types.NamespacedName{}); relErr != nil {
				return ctrl.Result{}, relErr
			}
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to get FeatureStore CR")
		return ctrl.Result{}, err
	}
	currentStatus := cr.Status.DeepCopy()

	if cr.DeletionTimestamp != nil {
		if r.Metrics != nil {
			r.Metrics.DeleteFeatureStore(cr.Namespace, cr.Name)
		}
		return ctrl.Result{}, r.finalizeFeatureStore(ctx, cr)
	}

	result, recErr = r.deployFeast(ctx, cr)
	if recErr == nil && r.Metrics != nil {
		r.Metrics.RecordFeatureStore(cr)
	}
	// status.applied is a FeatureStoreSpec, so feastProject must match the CRD
	// pattern. Early returns (wrong data-registry name, missing annotation)
	// never run ApplyDefaults, and dropping the finalizer clears status.
	// An empty project makes the status update invalid, so the rejection
	// condition never persists and the CR looks ignored.
	ensureAppliedFeastProject(cr)
	if cr.DeletionTimestamp == nil && !reflect.DeepEqual(currentStatus, cr.Status) {
		if err = r.Client.Status().Update(ctx, cr); err != nil {
			if apierrors.IsConflict(err) {
				logger.Info("FeatureStore object modified, retry syncing status")
				// Re-queue and preserve existing recErr
				result = ctrl.Result{Requeue: true, RequeueAfter: RequeueDelayError}
			}
			logger.Error(err, "Error updating the FeatureStore status")
			if recErr == nil {
				// There is no existing recErr. Set it to the status update error
				recErr = err
			}
		}
	}

	if recErr == nil && cr.DeletionTimestamp == nil && apimeta.IsStatusConditionTrue(cr.Status.Conditions, feastdevv1.ReadyType) {
		if err := access.EnsureNamespaceLabel(ctx, r.Client, cr.Namespace); err != nil {
			logger.Error(err, "Failed to add Feast label to namespace")
		}
		r.addToNamespaceRegistry(ctx, cr)
		r.addToOpenLineageDiscovery(ctx, cr)
		policies, err := r.fetchPermissionsFromRegistry(ctx, cr)
		if err != nil {
			logger.Error(err, "Failed to fetch permissions from registry")
		}
		if err != nil || len(policies) == 0 || cr.Status.ClientConfigMap == "" {
			logger.V(1).Info("Auto-access prerequisites missing or registry unreachable; cleaning up stale auto-access RBAC",
				"policies", len(policies),
				"clientConfigMapSet", cr.Status.ClientConfigMap != "",
				"fetchError", err != nil,
			)
			if err := access.CleanupAutoAccessRBAC(ctx, r.Client, cr.Namespace, cr.Name); err != nil {
				logger.Error(err, "Failed to cleanup stale auto-access RBAC")
			}
		} else {
			if err := access.ReconcileAutoAccessRBAC(ctx, r.Client, r.Scheme, cr, cr.Namespace, cr.Name, cr.Status.ClientConfigMap, policies); err != nil {
				logger.Error(err, "Failed to reconcile auto-access RBAC")
			}
		}
	}

	if recErr == nil && result.RequeueAfter == 0 {
		result.RequeueAfter = RequeuePeriodicInterval
	}
	return result, recErr
}

// finalizeFeatureStore completes the removal of a deleting FeatureStore CR.
// The data-registry finalizer is dropped first: cluster-scoped ClusterRoles
// and CRBs have no owner references and would be orphaned if this pod
// restarted. The CR's own registry and discovery cleanup runs next, and only
// then is the capabilities ConfigMap finalizer released — the module operator
// uninstalls this operator once that finalizer is gone, so cleanup has to
// finish first. The CR delete event still enqueues one not-found reconcile
// that releases again if this attempt did not.
func (r *FeatureStoreReconciler) finalizeFeatureStore(ctx context.Context, cr *feastdevv1.FeatureStore) error {
	logger := log.FromContext(ctx)
	// cleaned is set only after the data-registry finalizer is removed, so
	// the hold check can ignore the informer's stale copy of this object.
	var cleaned types.NamespacedName
	if controllerutil.ContainsFinalizer(cr, services.DataRegistryFinalizer) {
		feast := services.FeastServices{
			Handler: feasthandler.FeastHandler{
				Client:       r.Client,
				Context:      ctx,
				FeatureStore: cr,
				Scheme:       r.Scheme,
			},
		}
		if err := feast.CleanupDataRegistryClusterRoles(); err != nil {
			logger.Error(err, "Failed to cleanup data registry ClusterRoles during finalizer handling")
			return err
		}
		if err := feast.CleanupDataRegistryAuthDelegatorBinding(); err != nil {
			logger.Error(err, "Failed to cleanup data registry auth-delegator binding during finalizer handling")
			return err
		}
		controllerutil.RemoveFinalizer(cr, services.DataRegistryFinalizer)
		if err := r.Update(ctx, cr); err != nil {
			return err
		}
		// The Update is in the API, but the List below reads the informer
		// cache, which still has the finalizer until this write is
		// observed. Pass the key so that stale copy cannot keep the
		// ConfigMap finalizer held if this object is then deleted and
		// no later reconcile runs.
		cleaned = client.ObjectKeyFromObject(cr)
	}
	r.cleanupOnDeletion(ctx, cr.Namespace, cr.Name)
	return r.reconcileCapabilitiesConfigOnCRRemoval(ctx, cleaned)
}

func (r *FeatureStoreReconciler) fetchPermissionsFromRegistry(ctx context.Context, cr *feastdevv1.FeatureStore) ([]registry.PermissionPolicy, error) {
	logger := log.FromContext(ctx)
	registryRest := cr.Status.ServiceHostnames.RegistryRest
	if registryRest == "" {
		logger.V(1).Info("Skipping permission fetch: registry REST API hostname is not set (ensure RestAPI is enabled)")
		return nil, nil
	}
	project := cr.Status.Applied.FeastProject
	if project == "" {
		project = cr.Spec.FeastProject
	}
	if project == "" {
		logger.Info("Skipping permission fetch: feast project name is not set")
		return nil, nil
	}
	intraCommToken, err := r.readIntraCommunicationToken(ctx, cr)
	if err != nil {
		return nil, err
	}
	useTLS := cr.Status.Applied.Services.Registry != nil &&
		cr.Status.Applied.Services.Registry.Local != nil &&
		cr.Status.Applied.Services.Registry.Local.Server != nil &&
		cr.Status.Applied.Services.Registry.Local.Server.TLS.IsTLS()
	return registry.ListPermissions(ctx, registryRest, project, intraCommToken, useTLS)
}

func (r *FeatureStoreReconciler) readIntraCommunicationToken(ctx context.Context, cr *feastdevv1.FeatureStore) (string, error) {
	cmName := services.GetIntraCommunicationConfigMapName(cr.Name)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: cr.Namespace}, cm); err != nil {
		return "", err
	}
	return cm.Data["token"], nil
}

func (r *FeatureStoreReconciler) addToNamespaceRegistry(ctx context.Context, cr *feastdevv1.FeatureStore) {
	logger := log.FromContext(ctx)
	feast := services.FeastServices{
		Handler: feasthandler.FeastHandler{
			Client:       r.Client,
			Context:      ctx,
			FeatureStore: cr,
			Scheme:       r.Scheme,
		},
	}
	if services.IsDataRegistryCREnabled(cr) {
		if err := feast.RemoveFromNamespaceRegistry(); err != nil {
			logger.Error(err, "Failed to remove data registry from namespace registry")
		}
		return
	}
	if err := feast.AddToNamespaceRegistry(); err != nil {
		logger.Error(err, "Failed to add feature store to namespace registry")
	}
}

func (r *FeatureStoreReconciler) addToOpenLineageDiscovery(ctx context.Context, cr *feastdevv1.FeatureStore) {
	logger := log.FromContext(ctx)
	feast := services.FeastServices{
		Handler: feasthandler.FeastHandler{
			Client:       r.Client,
			Context:      ctx,
			FeatureStore: cr,
			Scheme:       r.Scheme,
		},
	}
	if err := feast.AddToOpenLineageDiscovery(); err != nil {
		logger.Error(err, "Failed to add feature store to OpenLineage discovery")
	}
}

func (r *FeatureStoreReconciler) cleanupOnDeletion(ctx context.Context, namespace, name string) {
	logger := log.FromContext(ctx)
	otherCount := r.countOtherFeatureStoresInNamespace(ctx, namespace, name)
	if err := access.RemoveNamespaceLabelIfLast(ctx, r.Client, namespace, otherCount); err != nil {
		logger.Error(err, "Failed to remove Feast label from namespace")
	}
	if err := access.CleanupAutoAccessRBAC(ctx, r.Client, namespace, name); err != nil {
		logger.Error(err, "Failed to cleanup auto-access RBAC")
	}
	clusterCount := r.countOtherFeatureStoresInCluster(ctx, namespace, name)
	if err := access.CleanupDiscoverClusterRoleIfLast(ctx, r.Client, clusterCount); err != nil {
		logger.Error(err, "Failed to cleanup discover ClusterRole")
	}
	deletedCR := &feastdevv1.FeatureStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	r.cleanupNamespaceRegistry(ctx, deletedCR)
	if err := r.cleanupOpenLineageDiscovery(ctx, deletedCR); err != nil {
		logger.Error(err, "Failed to clean up OpenLineage discovery entry")
	}

	// The finalizer cleans UID-labeled cluster-scoped RBAC before the CR is
	// removed. This fallback only removes legacy RBAC without a UID label;
	// it must not delete resources belonging to a same-name replacement CR.
	feast := services.FeastServices{
		Handler: feasthandler.FeastHandler{
			Client:       r.Client,
			Context:      ctx,
			FeatureStore: deletedCR,
			Scheme:       r.Scheme,
		},
	}
	if err := feast.CleanupDataRegistryClusterRoles(); err != nil {
		logger.Error(err, "Failed to cleanup data registry ClusterRoles")
	}
	if err := feast.CleanupDataRegistryAuthDelegatorBinding(); err != nil {
		logger.Error(err, "Failed to cleanup data registry auth-delegator ClusterRoleBinding")
	}
}

func (r *FeatureStoreReconciler) cleanupNamespaceRegistry(ctx context.Context, cr *feastdevv1.FeatureStore) {
	logger := log.FromContext(ctx)
	feast := services.FeastServices{
		Handler: feasthandler.FeastHandler{
			Client:       r.Client,
			Context:      ctx,
			FeatureStore: cr,
			Scheme:       r.Scheme,
		},
	}
	if err := feast.RemoveFromNamespaceRegistry(); err != nil {
		logger.Error(err, "Failed to remove feature store from namespace registry")
	}
}

func (r *FeatureStoreReconciler) countOtherFeatureStoresInNamespace(ctx context.Context, namespace, excludeName string) int {
	var list feastdevv1.FeatureStoreList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return -1
	}
	count := 0
	for i := range list.Items {
		if list.Items[i].Name != excludeName && list.Items[i].DeletionTimestamp == nil {
			count++
		}
	}
	return count
}

func (r *FeatureStoreReconciler) countOtherFeatureStoresInCluster(ctx context.Context, namespace, excludeName string) int {
	var list feastdevv1.FeatureStoreList
	if err := r.List(ctx, &list); err != nil {
		return -1
	}
	count := 0
	for i := range list.Items {
		item := &list.Items[i]
		if (item.Name != excludeName || item.Namespace != namespace) && item.DeletionTimestamp == nil {
			count++
		}
	}
	return count
}

// removeRejectedDataRegistryWorkload deletes the data-registry workload and
// drops this FeatureStore from feature-store discovery. Discovery is removed
// only after the workload is gone, so a failed delete is retried while the
// entry is still present. Callers write status after this returns: removing
// the finalizer updates the CR, and that response replaces .status.
func removeRejectedDataRegistryWorkload(feast services.FeastServices) (cleanupErr, registryErr error) {
	cleanupErr = feast.CleanupDataRegistry()
	if cleanupErr != nil {
		return cleanupErr, nil
	}
	// Older operators registered this CR for feature-store discovery. The
	// Service is gone; the discovery entry has to go with it or workbenches
	// keep a dead endpoint.
	return nil, feast.RemoveFromNamespaceRegistry()
}

// reconcileCapabilitiesConfigFinalizer manages this operator's finalizer on
// the platform capabilities ConfigMap, implementing the deletion handshake
// with feast-module-operator:
//   - healthy ConfigMap: the finalizer is ensured (idempotent) so a later
//     deletion cannot complete without this operator observing it;
//   - Terminating ConfigMap with the capabilities still enabled (manual
//     deletion, recreated by the module operator): the finalizer is released
//     without any teardown so the recreation can proceed;
//   - Terminating ConfigMap with the capabilities disabled (module operator
//     uninstall intent): the finalizer is held until no data-registry
//     FeatureStore CR still carries the DataRegistryFinalizer, i.e. until the
//     Data Registry cleanup triggered by the capability gates has finished.
//
// cleaned is the FeatureStore whose cleanup finalizer this reconcile already
// removed. An empty name excludes nothing. The informer cache still lists
// that finalizer until the update is observed, and trusting it would hold
// the ConfigMap finalizer on an object that is about to disappear.
func (r *FeatureStoreReconciler) reconcileCapabilitiesConfigFinalizer(ctx context.Context, caps capabilities.Config, cleaned types.NamespacedName) error {
	if !caps.PlatformConfigPresent {
		return nil
	}

	if !caps.PlatformConfigTerminating {
		return r.ensureCapabilitiesConfigFinalizer(ctx)
	}

	if caps.DataRegistryEnabled {
		// Deletion without teardown intent: let the module operator's
		// ConfigMap recreation proceed.
		return r.removeCapabilitiesConfigFinalizer(ctx)
	}

	// Teardown intent: hold the finalizer until every data-registry
	// FeatureStore CR has dropped its cleanup finalizer, i.e. until the
	// Data Registry workload and cluster-scoped RBAC are gone.
	//
	// Returning without an error does not end the handshake. While a
	// FeatureStore still exists, the capabilities ConfigMap watch and that
	// CR's 30s periodic requeue both run this check again, and removing the
	// cleanup finalizer is itself a FeatureStore update. A CR that is
	// deleted is released from the deletion and not-found paths.
	var list feastdevv1.FeatureStoreList
	if err := r.List(ctx, &list); err != nil {
		return fmt.Errorf("failed to list FeatureStores for capabilities teardown check: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if cleaned.Name != "" && item.Name == cleaned.Name && item.Namespace == cleaned.Namespace {
			continue
		}
		if !services.IsDataRegistryCREnabled(item) {
			continue
		}
		if controllerutil.ContainsFinalizer(item, services.DataRegistryFinalizer) {
			// Data Registry cleanup still in flight; keep holding.
			return nil
		}
	}

	return r.removeCapabilitiesConfigFinalizer(ctx)
}

// reconcileCapabilitiesConfigOnCRRemoval manages the capabilities ConfigMap
// finalizer while a FeatureStore CR is being removed. The ConfigMap watch
// mapper enqueues only existing CRs, so the deletion and not-found paths are
// what release this operator's finalizer after the CR is gone. cleaned is
// forwarded to the hold check; see reconcileCapabilitiesConfigFinalizer.
// A missing or unreadable ConfigMap is skipped: it must never block CR deletion.
func (r *FeatureStoreReconciler) reconcileCapabilitiesConfigOnCRRemoval(ctx context.Context, cleaned types.NamespacedName) error {
	logger := log.FromContext(ctx)
	caps, err := capabilities.Load(ctx, r.Client)
	if err != nil {
		logger.V(1).Info("Skipping capabilities ConfigMap finalizer handling during CR removal",
			"reason", err.Error())
		return nil
	}
	return r.reconcileCapabilitiesConfigFinalizer(ctx, caps, cleaned)
}

// ensureCapabilitiesConfigFinalizer adds the capabilities ConfigMap finalizer
// when absent. The ConfigMap must not be terminating: the API server rejects
// adding finalizers to objects being deleted, and a recreated ConfigMap is
// re-adopted by a later reconcile.
func (r *FeatureStoreReconciler) ensureCapabilitiesConfigFinalizer(ctx context.Context) error {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      capabilities.ConfigMapName,
		Namespace: capabilities.OperatorNamespace(),
	}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to read %s ConfigMap: %w", capabilities.ConfigMapName, err)
	}
	if cm.DeletionTimestamp != nil || controllerutil.ContainsFinalizer(cm, capabilities.CapabilitiesConfigFinalizer) {
		return nil
	}
	controllerutil.AddFinalizer(cm, capabilities.CapabilitiesConfigFinalizer)
	if err := r.Update(ctx, cm); err != nil {
		return fmt.Errorf("failed to add finalizer to %s ConfigMap: %w", capabilities.ConfigMapName, err)
	}
	return nil
}

// removeCapabilitiesConfigFinalizer removes the capabilities ConfigMap
// finalizer from a Terminating ConfigMap. A ConfigMap that is no longer
// terminating (recreated) is left untouched so a later reconcile re-adopts it.
func (r *FeatureStoreReconciler) removeCapabilitiesConfigFinalizer(ctx context.Context) error {
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      capabilities.ConfigMapName,
		Namespace: capabilities.OperatorNamespace(),
	}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to read %s ConfigMap: %w", capabilities.ConfigMapName, err)
	}
	if cm.DeletionTimestamp == nil || !controllerutil.ContainsFinalizer(cm, capabilities.CapabilitiesConfigFinalizer) {
		return nil
	}
	controllerutil.RemoveFinalizer(cm, capabilities.CapabilitiesConfigFinalizer)
	if err := r.Update(ctx, cm); err != nil {
		return fmt.Errorf("failed to remove finalizer from %s ConfigMap: %w", capabilities.ConfigMapName, err)
	}
	return nil
}

// guardDataRegistry applies data-registry admission. stop is true when
// deployFeast must return without deploying.
func (r *FeatureStoreReconciler) guardDataRegistry(ctx context.Context, cr *feastdevv1.FeatureStore, caps capabilities.Config) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	feast := services.FeastServices{
		Handler: feasthandler.FeastHandler{
			Client:       r.Client,
			Context:      ctx,
			FeatureStore: cr,
			Scheme:       r.Scheme,
		},
	}

	if !caps.DataRegistryEnabled {
		// Delete the workload before writing status. Dropping the
		// data-registry finalizer updates the CR, and that update
		// response replaces .status with the persisted status.
		cleanupErr, registryErr := removeRejectedDataRegistryWorkload(feast)

		drCond := services.FeastServiceConditions[services.DataRegistryFeastType][metav1.ConditionFalse]
		drCond.Reason = feastdevv1.DataRegistryDisabledReason
		drCond.Message = services.ErrorMessagePrefix + feastdevv1.DataRegistryDisabledMessage
		apimeta.SetStatusCondition(&cr.Status.Conditions, drCond)
		condition := metav1.Condition{
			Type:    feastdevv1.ReadyType,
			Status:  metav1.ConditionFalse,
			Reason:  feastdevv1.DataRegistryDisabledReason,
			Message: feastdevv1.DataRegistryDisabledMessage,
		}
		apimeta.SetStatusCondition(&cr.Status.Conditions, condition)
		cr.Status.Phase = feastdevv1.FailedPhase
		logger.Info(condition.Message)

		if cleanupErr != nil {
			logger.Error(cleanupErr, "Failed to cleanup data registry resources after capability disabled")
			return ctrl.Result{RequeueAfter: RequeueDelayError}, true, cleanupErr
		}
		if registryErr != nil {
			logger.Error(registryErr, "Failed to remove data registry from namespace registry after capability disabled")
			return ctrl.Result{RequeueAfter: RequeueDelayError}, true, registryErr
		}
		return ctrl.Result{}, true, nil
	}

	// Data-registry mode deploys only the FeatureStore named data-registry.
	// Reject any other name before the namespace check.
	if cr.Name != services.DataRegistryCRName {
		cleanupErr, registryErr := removeRejectedDataRegistryWorkload(feast)

		rejMsg := fmt.Sprintf(feastdevv1.DataRegistryNameRejectedMessage,
			services.DataRegistryCRName, cr.Name)
		drCond := services.FeastServiceConditions[services.DataRegistryFeastType][metav1.ConditionFalse]
		drCond.Reason = feastdevv1.DataRegistryNameRejectedReason
		drCond.Message = services.ErrorMessagePrefix + rejMsg
		apimeta.SetStatusCondition(&cr.Status.Conditions, drCond)
		condition := metav1.Condition{
			Type:    feastdevv1.ReadyType,
			Status:  metav1.ConditionFalse,
			Reason:  feastdevv1.DataRegistryNameRejectedReason,
			Message: rejMsg,
		}
		apimeta.SetStatusCondition(&cr.Status.Conditions, condition)
		cr.Status.Phase = feastdevv1.FailedPhase
		logger.Info("Data Registry name rejected",
			"cr", cr.Name, "requiredName", services.DataRegistryCRName)

		if cleanupErr != nil {
			logger.Error(cleanupErr, "Failed to cleanup data registry resources after name rejection")
			return ctrl.Result{RequeueAfter: RequeueDelayError}, true, cleanupErr
		}
		if registryErr != nil {
			logger.Error(registryErr, "Failed to remove data registry from namespace registry after name rejection")
			return ctrl.Result{RequeueAfter: RequeueDelayError}, true, registryErr
		}
		return ctrl.Result{}, true, nil
	}

	// Enforce exact namespace match. The data-registry namespace is always
	// rhoai-data-registry (custom namespace selection is deferred). The
	// check runs regardless of whether the ConfigMap is present because
	// caps.DataRegistryNamespace defaults to rhoai-data-registry.
	if cr.Namespace != caps.DataRegistryNamespace {
		// The expected namespace can change, and an older operator may already
		// have deployed this CR. Leave neither the workload nor its discovery
		// entry behind.
		cleanupErr, registryErr := removeRejectedDataRegistryWorkload(feast)

		rejMsg := fmt.Sprintf(feastdevv1.DataRegistryNamespaceRejectedMessage,
			caps.DataRegistryNamespace, cr.Namespace)
		drCond := services.FeastServiceConditions[services.DataRegistryFeastType][metav1.ConditionFalse]
		drCond.Reason = feastdevv1.DataRegistryNamespaceRejectedReason
		drCond.Message = services.ErrorMessagePrefix + rejMsg
		apimeta.SetStatusCondition(&cr.Status.Conditions, drCond)
		condition := metav1.Condition{
			Type:    feastdevv1.ReadyType,
			Status:  metav1.ConditionFalse,
			Reason:  feastdevv1.DataRegistryNamespaceRejectedReason,
			Message: rejMsg,
		}
		apimeta.SetStatusCondition(&cr.Status.Conditions, condition)
		cr.Status.Phase = feastdevv1.FailedPhase
		logger.Info("Data Registry namespace rejected",
			"cr", cr.Name, "crNamespace", cr.Namespace,
			"expectedNamespace", caps.DataRegistryNamespace)

		if cleanupErr != nil {
			logger.Error(cleanupErr, "Failed to cleanup data registry resources after namespace rejection")
			return ctrl.Result{RequeueAfter: RequeueDelayError}, true, cleanupErr
		}
		if registryErr != nil {
			logger.Error(registryErr, "Failed to remove data registry from namespace registry after namespace rejection")
			return ctrl.Result{RequeueAfter: RequeueDelayError}, true, registryErr
		}
		return ctrl.Result{}, true, nil
	}
	return ctrl.Result{}, false, nil
}

func (r *FeatureStoreReconciler) deployFeast(ctx context.Context, cr *feastdevv1.FeatureStore) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	condition := metav1.Condition{
		Type:    feastdevv1.ReadyType,
		Status:  metav1.ConditionTrue,
		Reason:  feastdevv1.ReadyReason,
		Message: feastdevv1.ReadyMessage,
	}

	caps, capErr := capabilities.Load(ctx, r.Client)
	if capErr != nil {
		// Distinguish PlatformConfigMissing from generic errors.
		reason := feastdevv1.FailedReason
		msg := "Error: " + capErr.Error()
		if _, ok := capErr.(*capabilities.PlatformConfigMissingError); ok {
			reason = feastdevv1.PlatformConfigMissingReason
			msg = capErr.Error()
		}
		logger.Error(capErr, "Failed to load feast-capabilities-config")
		condition = metav1.Condition{
			Type:    feastdevv1.ReadyType,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: msg,
		}
		apimeta.SetStatusCondition(&cr.Status.Conditions, condition)
		cr.Status.Phase = feastdevv1.FailedPhase
		return ctrl.Result{Requeue: true, RequeueAfter: RequeueDelayError}, capErr
	}

	// Manage this operator's finalizer on the capabilities ConfigMap before
	// any capability gating: on a healthy ConfigMap the finalizer is ensured
	// so a later deletion cannot bypass this operator; on a Terminating
	// ConfigMap it is released once the Data Registry cleanup signaled by the
	// disabled capability flags has completed, or immediately when the
	// deletion carries no teardown intent (manual deletion, recreated by the
	// module operator).
	if err := r.reconcileCapabilitiesConfigFinalizer(ctx, caps, types.NamespacedName{}); err != nil {
		condition = metav1.Condition{
			Type:    feastdevv1.ReadyType,
			Status:  metav1.ConditionFalse,
			Reason:  feastdevv1.FailedReason,
			Message: "Error: " + err.Error(),
		}
		apimeta.SetStatusCondition(&cr.Status.Conditions, condition)
		cr.Status.Phase = feastdevv1.FailedPhase
		return ctrl.Result{RequeueAfter: RequeueDelayError}, err
	}

	if cr.Namespace == caps.DataRegistryNamespace && !services.IsDataRegistryCREnabled(cr) {
		feast := services.FeastServices{Handler: feasthandler.FeastHandler{
			Client: r.Client, Context: ctx, FeatureStore: cr, Scheme: r.Scheme,
		}}
		// Cleanup may update the CR to drop its finalizer. That update
		// response replaces .status, so the rejection is recorded after it.
		drCleanupErr := feast.CleanupDataRegistry()
		var stdCleanupErr error
		var registryErr error
		if drCleanupErr == nil {
			stdCleanupErr = feast.CleanupStandardResources()
		}
		if drCleanupErr == nil && stdCleanupErr == nil {
			registryErr = feast.RemoveFromNamespaceRegistry()
		}

		drCond := services.FeastServiceConditions[services.DataRegistryFeastType][metav1.ConditionFalse]
		drCond.Reason = feastdevv1.DataRegistryAnnotationRequiredReason
		drCond.Message = services.ErrorMessagePrefix + feastdevv1.DataRegistryAnnotationRequiredMessage
		apimeta.SetStatusCondition(&cr.Status.Conditions, drCond)
		condition = metav1.Condition{
			Type:    feastdevv1.ReadyType,
			Status:  metav1.ConditionFalse,
			Reason:  feastdevv1.DataRegistryAnnotationRequiredReason,
			Message: feastdevv1.DataRegistryAnnotationRequiredMessage,
		}
		apimeta.SetStatusCondition(&cr.Status.Conditions, condition)
		cr.Status.Phase = feastdevv1.FailedPhase

		if drCleanupErr != nil {
			return ctrl.Result{RequeueAfter: RequeueDelayError}, drCleanupErr
		}
		if stdCleanupErr != nil {
			return ctrl.Result{RequeueAfter: RequeueDelayError}, stdCleanupErr
		}
		if registryErr != nil {
			return ctrl.Result{RequeueAfter: RequeueDelayError}, registryErr
		}
		return ctrl.Result{}, nil
	}

	if services.IsDataRegistryCREnabled(cr) {
		if gateResult, stop, gateErr := r.guardDataRegistry(ctx, cr, caps); stop {
			return gateResult, gateErr
		}
	} else if !caps.FeatureStoreEnabled {
		condition = metav1.Condition{
			Type:    feastdevv1.ReadyType,
			Status:  metav1.ConditionFalse,
			Reason:  feastdevv1.FeatureStoreDisabledReason,
			Message: feastdevv1.FeatureStoreDisabledMessage,
		}
		apimeta.SetStatusCondition(&cr.Status.Conditions, condition)
		cr.Status.Phase = feastdevv1.FailedPhase
		logger.Info(condition.Message)

		// Clean up standard Feast owned resources so they don't remain
		// active while the CR reports the capability as disabled.
		feast := services.FeastServices{
			Handler: feasthandler.FeastHandler{
				Client:       r.Client,
				Context:      ctx,
				FeatureStore: cr,
				Scheme:       r.Scheme,
			},
		}
		if cleanupErr := feast.CleanupStandardResources(); cleanupErr != nil {
			logger.Error(cleanupErr, "Failed to cleanup standard resources after capability disabled")
			return ctrl.Result{RequeueAfter: RequeueDelayError}, cleanupErr
		}

		return ctrl.Result{}, nil
	}

	feast := services.FeastServices{
		Handler: feasthandler.FeastHandler{
			Client:       r.Client,
			Context:      ctx,
			FeatureStore: cr,
			Scheme:       r.Scheme,
		},
	}
	authz := authz.FeastAuthorization{
		Handler: feast.Handler,
	}

	// status defaults must be applied before deployments
	errResult := ctrl.Result{Requeue: true, RequeueAfter: RequeueDelayError}
	if err = feast.ApplyDefaults(); err != nil {
		result = errResult
	} else if err = authz.Deploy(); err != nil {
		result = errResult
	} else if err = feast.Deploy(); err != nil {
		result = errResult
	}
	if err != nil {
		condition = metav1.Condition{
			Type:    feastdevv1.ReadyType,
			Status:  metav1.ConditionFalse,
			Reason:  feastdevv1.FailedReason,
			Message: "Error: " + err.Error(),
		}
	} else {
		deployment, deploymentErr := feast.GetDeployment()
		if deploymentErr != nil {
			condition = metav1.Condition{
				Type:    feastdevv1.ReadyType,
				Status:  metav1.ConditionUnknown,
				Reason:  feastdevv1.DeploymentNotAvailableReason,
				Message: feastdevv1.DeploymentNotAvailableMessage,
			}

			result = errResult
		} else {
			isDeployAvailable := services.IsDeploymentAvailable(deployment.Status.Conditions)
			if !isDeployAvailable {
				msg := feastdevv1.DeploymentNotAvailableMessage
				if podMsg := feast.GetPodContainerFailureMessage(deployment); podMsg != "" {
					msg = msg + ": " + podMsg
				}
				condition = metav1.Condition{
					Type:    feastdevv1.ReadyType,
					Status:  metav1.ConditionUnknown,
					Reason:  feastdevv1.DeploymentNotAvailableReason,
					Message: msg,
				}

				result = errResult
			}
		}
	}

	if services.IsDataRegistryCREnabled(cr) &&
		condition.Type == feastdevv1.ReadyType &&
		condition.Status == metav1.ConditionTrue {
		condition.Message = feastdevv1.DataRegistryReadyStatusMessage
	}
	logger.Info(condition.Message)
	apimeta.SetStatusCondition(&cr.Status.Conditions, condition)
	if apimeta.IsStatusConditionTrue(cr.Status.Conditions, feastdevv1.ReadyType) {
		cr.Status.Phase = feastdevv1.ReadyPhase
	} else if apimeta.IsStatusConditionFalse(cr.Status.Conditions, feastdevv1.ReadyType) {
		cr.Status.Phase = feastdevv1.FailedPhase
	} else {
		cr.Status.Phase = feastdevv1.PendingPhase
	}

	return result, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *FeatureStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&feastdevv1.FeatureStore{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&rbacv1.Role{}).
		Owns(&batchv1.CronJob{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&feastdevv1.FeatureStore{}, handler.EnqueueRequestsFromMapFunc(r.mapFeastRefsToFeastRequests))

	if services.IsOpenShift() {
		bldr = bldr.Owns(&routev1.Route{})
	}
	if services.HasServiceMonitorCRD() {
		sm := &unstructured.Unstructured{}
		sm.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "monitoring.coreos.com",
			Version: "v1",
			Kind:    "ServiceMonitor",
		})
		bldr = bldr.Owns(sm)
	}
	if services.HasMlflowCRD() {
		mlflow := &unstructured.Unstructured{}
		mlflow.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "mlflow.opendatahub.io",
			Version: "v1",
			Kind:    "MLflow",
		})
		bldr = bldr.Watches(mlflow,
			handler.EnqueueRequestsFromMapFunc(r.mapMlflowToFeastRequests),
			builder.WithPredicates(mlflowStatusChangedPredicate()),
		)
	}

	if capsNS := capabilities.OperatorNamespace(); capsNS != "" {
		// Watches() uses the manager cache. That cache lists ConfigMaps only
		// when they carry app.kubernetes.io/managed-by=feast-operator, and
		// feast-capabilities-config is written by the module operator without
		// that label, so those events never arrive. Watch the one object by
		// name instead. Reads already bypass the filtered cache.
		capsCache, err := newCapabilitiesConfigCache(mgr, capsNS)
		if err != nil {
			return fmt.Errorf("create capabilities ConfigMap cache: %w", err)
		}
		if err := mgr.Add(capsCache); err != nil {
			return fmt.Errorf("start capabilities ConfigMap cache: %w", err)
		}
		bldr = bldr.WatchesRawSource(source.Kind[client.Object](
			capsCache,
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.mapCapabilitiesConfigToFeatureStores),
			capabilitiesConfigMapPredicate(capsNS),
		))
	}

	return bldr.Complete(r)

}

// mlflowStatusChangedPredicate triggers the mapper only when the MLflow CR's
// status changes (readiness transitions) or on create/delete — not on
// annotation, label, or metadata-only updates.
func mlflowStatusChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(_ event.CreateEvent) bool { return true },
		DeleteFunc: func(_ event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldU, ok1 := e.ObjectOld.(*unstructured.Unstructured)
			newU, ok2 := e.ObjectNew.(*unstructured.Unstructured)
			if !ok1 || !ok2 {
				return true
			}
			oldStatus, _, _ := unstructured.NestedMap(oldU.Object, "status")
			newStatus, _, _ := unstructured.NestedMap(newU.Object, "status")
			return !reflect.DeepEqual(oldStatus, newStatus)
		},
		GenericFunc: func(_ event.GenericEvent) bool { return true },
	}
}

// mapMlflowToFeastRequests re-queues FeatureStores that use MLflow (those that
// have not explicitly opted out) when the cluster MLflow CR's status changes.
func (r *FeatureStoreReconciler) mapMlflowToFeastRequests(ctx context.Context, _ client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)
	var feastList feastdevv1.FeatureStoreList
	if err := r.List(ctx, &feastList, client.InNamespace("")); err != nil {
		logger.Error(err, "could not list FeatureStores for MLflow watch")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(feastList.Items))
	for i := range feastList.Items {
		fs := &feastList.Items[i]
		if fs.Spec.Mlflow != nil && !fs.Spec.Mlflow.Enabled {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(fs),
		})
	}
	return requests
}

func capabilitiesConfigMapPredicate(operatorNamespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return false
		}
		return cm.GetName() == capabilities.ConfigMapName && cm.GetNamespace() == operatorNamespace
	})
}

// capabilitiesConfigCacheOptions limits an informer to feast-capabilities-config
// in the operator namespace. The object has no managed-by label, so it is not
// in the manager's ConfigMap informer. No label selector is set: one would
// hide the ConfigMap this watch exists to see.
func capabilitiesConfigCacheOptions(namespace string) cache.Options {
	return cache.Options{
		DefaultNamespaces: map[string]cache.Config{
			namespace: {
				FieldSelector: fields.OneTermEqualSelector(metav1.ObjectNameField, capabilities.ConfigMapName),
			},
		},
	}
}

func newCapabilitiesConfigCache(mgr ctrl.Manager, namespace string) (cache.Cache, error) {
	opts := capabilitiesConfigCacheOptions(namespace)
	opts.Scheme = mgr.GetScheme()
	opts.Mapper = mgr.GetRESTMapper()
	opts.HTTPClient = mgr.GetHTTPClient()
	return cache.New(mgr.GetConfig(), opts)
}

// mapCapabilitiesConfigToFeatureStores enqueues every FeatureStore so any
// surviving CR can finish the ConfigMap handshake. An empty list enqueues
// nothing: a CR deleted in this process is released by its not-found
// reconcile, and a cluster that already has no FeatureStores falls through
// to the module operator's uninstall timeout.
func (r *FeatureStoreReconciler) mapCapabilitiesConfigToFeatureStores(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}
	if cm.GetName() != capabilities.ConfigMapName {
		return nil
	}
	logger := log.FromContext(ctx)
	var feastList feastdevv1.FeatureStoreList
	if err := r.List(ctx, &feastList, client.InNamespace("")); err != nil {
		logger.Error(err, "could not list FeatureStores for capabilities ConfigMap watch")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(feastList.Items))
	for i := range feastList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&feastList.Items[i]),
		})
	}
	return requests
}

// cleanupOpenLineageDiscovery removes the feature store instance from the OpenLineage discovery ConfigMap
func (r *FeatureStoreReconciler) cleanupOpenLineageDiscovery(ctx context.Context, cr *feastdevv1.FeatureStore) error {
	feast := services.FeastServices{
		Handler: feasthandler.FeastHandler{
			Client:       r.Client,
			Context:      ctx,
			FeatureStore: cr,
			Scheme:       r.Scheme,
		},
	}

	return feast.RemoveFromOpenLineageDiscovery()
}

// if a remotely referenced FeatureStore is changed, reconcile any FeatureStores that reference it.
func (r *FeatureStoreReconciler) mapFeastRefsToFeastRequests(ctx context.Context, object client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)

	feastRef, ok := object.(*feastdevv1.FeatureStore)
	if !ok {
		logger.Error(nil, "Unexpected object type in mapFeastRefsToFeastRequests")
		return nil
	}

	// list all FeatureStores in the cluster
	var feastList feastdevv1.FeatureStoreList
	if err := r.List(ctx, &feastList, client.InNamespace("")); err != nil {
		logger.Error(err, "could not list FeatureStores. "+
			"FeatureStores affected by changes to the referenced FeatureStore object will not be reconciled.")
		return nil
	}

	feastRefNsName := client.ObjectKeyFromObject(feastRef)
	var requests []reconcile.Request
	for _, obj := range feastList.Items {
		objNsName := client.ObjectKeyFromObject(&obj)
		// this if statement is extra protection against any potential infinite reconcile loops
		if feastRefNsName != objNsName {
			feast := services.FeastServices{
				Handler: feasthandler.FeastHandler{
					Client:       r.Client,
					Context:      ctx,
					FeatureStore: &obj,
					Scheme:       r.Scheme,
				}}
			if feast.IsRemoteRefRegistry() {
				remoteRef := obj.Status.Applied.Services.Registry.Remote.FeastRef
				remoteRefNsName := types.NamespacedName{Name: remoteRef.Name, Namespace: remoteRef.Namespace}
				if feastRefNsName == remoteRefNsName {
					requests = append(requests, reconcile.Request{NamespacedName: objNsName})
				}
			}
		}
	}

	return requests
}
