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

package controller

import (
	"context"
	"testing"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/capabilities"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/services"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func capabilitiesFinalizerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := feastdevv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func healthyCapabilitiesCM() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      capabilities.ConfigMapName,
			Namespace: "operator-ns",
			Finalizers: []string{
				"components.platform.opendatahub.io/feast-capabilities",
			},
		},
		Data: map[string]string{
			capabilities.KeyFeatureStoreEnabled: "true",
			capabilities.KeyDataRegistryEnabled: "true",
		},
	}
}

// terminatingCapabilitiesCM returns a capabilities ConfigMap held in
// Terminating state by both the module operator and feast operator
// finalizers, carrying disabled capability flags (uninstall intent).
func terminatingCapabilitiesCM(teardownIntent bool) *corev1.ConfigMap {
	cm := healthyCapabilitiesCM()
	cm.Finalizers = append(cm.Finalizers, capabilities.CapabilitiesConfigFinalizer)
	if teardownIntent {
		cm.Data[capabilities.KeyFeatureStoreEnabled] = "false"
		cm.Data[capabilities.KeyDataRegistryEnabled] = "false"
	}
	now := metav1.Now()
	cm.DeletionTimestamp = &now
	return cm
}

func dataRegistryCR(withFinalizer bool) *feastdevv1.FeatureStore {
	cr := &feastdevv1.FeatureStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-registry",
			Namespace: capabilities.DefaultDataRegistryNamespace,
			Annotations: map[string]string{
				services.DataRegistryAnnotation: "true",
			},
		},
	}
	if withFinalizer {
		cr.Finalizers = []string{services.DataRegistryFinalizer}
	}
	return cr
}

func cmHasFinalizer(ctx context.Context, cl client.Client, finalizer string) bool {
	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, types.NamespacedName{
		Name:      capabilities.ConfigMapName,
		Namespace: "operator-ns",
	}, cm); err != nil {
		return false
	}
	return controllerutil.ContainsFinalizer(cm, finalizer)
}

func TestReconcileCapabilitiesConfigFinalizerStates(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	ctx := context.Background()

	annotatedCR := dataRegistryCR(true)
	cleanedCR := dataRegistryCR(false)

	tests := []struct {
		name string
		caps capabilities.Config
		objs []client.Object
		// wantHeld asserts the feast operator finalizer is still on the
		// ConfigMap after the call; wantAdded asserts it was added.
		wantHeld  bool
		wantAdded bool
	}{
		{
			name: "healthy ConfigMap gains the finalizer",
			caps: capabilities.Config{
				PlatformConfigPresent:     true,
				PlatformConfigTerminating: false,
				FeatureStoreEnabled:       true,
				DataRegistryEnabled:       true,
				DataRegistryNamespace:     capabilities.DefaultDataRegistryNamespace,
			},
			objs:      []client.Object{healthyCapabilitiesCM()},
			wantAdded: true,
		},
		{
			name: "absent ConfigMap is a no-op",
			caps: capabilities.Config{PlatformConfigPresent: false},
			objs: nil,
		},
		{
			name: "manual deletion without teardown intent releases the finalizer",
			caps: capabilities.Config{
				PlatformConfigPresent:     true,
				PlatformConfigTerminating: true,
				FeatureStoreEnabled:       true,
				DataRegistryEnabled:       true,
			},
			objs: []client.Object{terminatingCapabilitiesCM(false)},
		},
		{
			name: "teardown intent holds while a data-registry CR still carries its cleanup finalizer",
			caps: capabilities.Config{
				PlatformConfigPresent:     true,
				PlatformConfigTerminating: true,
				FeatureStoreEnabled:       false,
				DataRegistryEnabled:       false,
			},
			objs:     []client.Object{terminatingCapabilitiesCM(true), annotatedCR},
			wantHeld: true,
		},
		{
			name: "teardown intent releases once the data-registry CR is cleaned",
			caps: capabilities.Config{
				PlatformConfigPresent:     true,
				PlatformConfigTerminating: true,
				FeatureStoreEnabled:       false,
				DataRegistryEnabled:       false,
			},
			objs: []client.Object{terminatingCapabilitiesCM(true), cleanedCR},
		},
		{
			name: "teardown intent releases when no data-registry CR exists",
			caps: capabilities.Config{
				PlatformConfigPresent:     true,
				PlatformConfigTerminating: true,
				FeatureStoreEnabled:       false,
				DataRegistryEnabled:       false,
			},
			objs: []client.Object{terminatingCapabilitiesCM(true)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := capabilitiesFinalizerTestScheme(t)
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if len(tt.objs) > 0 {
				builder = builder.WithObjects(tt.objs...)
			}
			cl := builder.Build()
			r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}

			if err := r.reconcileCapabilitiesConfigFinalizer(ctx, tt.caps, types.NamespacedName{}); err != nil {
				t.Fatalf("reconcileCapabilitiesConfigFinalizer: %v", err)
			}

			held := cmHasFinalizer(ctx, cl, capabilities.CapabilitiesConfigFinalizer)
			if tt.wantHeld && !held {
				t.Fatal("expected feast operator finalizer to be held on the ConfigMap")
			}
			if !tt.wantHeld && tt.wantAdded && !held {
				t.Fatal("expected feast operator finalizer to be added to the ConfigMap")
			}
			if !tt.wantHeld && !tt.wantAdded && held {
				t.Fatal("expected feast operator finalizer to be released from the ConfigMap")
			}
		})
	}
}

// TestDataRegistryTeardownReleasesConfigMapFinalizer verifies the full wiring
// through Reconcile: a Terminating capabilities ConfigMap carrying disabled
// flags (module operator uninstall intent) triggers the Data Registry
// teardown, and the ConfigMap finalizer is released only on the reconcile
// after the data-registry CR has dropped its cleanup finalizer.
func TestDataRegistryTeardownReleasesConfigMapFinalizer(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	t.Setenv("FEAST_PLATFORM_CAPABILITIES_REQUIRED", "true")
	ctx := context.Background()
	scheme := capabilitiesFinalizerTestScheme(t)

	cr := dataRegistryCR(true)
	cr.UID = types.UID("data-registry-uid")
	registryCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      services.NamespaceRegistryConfigMapName,
			Namespace: services.DefaultKubernetesNamespace,
		},
		Data: map[string]string{},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(cr).
		WithObjects(terminatingCapabilitiesCM(true), cr, registryCM).
		Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}
	key := types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}

	// First reconcile: the capability gate tears down the Data Registry and
	// removes the CR finalizer, but the ConfigMap finalizer is held because
	// the CR still carried its cleanup finalizer when it was checked.
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	updated := &feastdevv1.FeatureStore{}
	if err := cl.Get(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(updated, services.DataRegistryFinalizer) {
		t.Fatal("expected data-registry finalizer to be removed after teardown")
	}
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.ReadyType)
	if ready == nil || ready.Reason != feastdevv1.DataRegistryDisabledReason {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
	if !cmHasFinalizer(ctx, cl, capabilities.CapabilitiesConfigFinalizer) {
		t.Fatal("expected ConfigMap finalizer to be held while teardown runs")
	}

	// Second reconcile (e.g. triggered by the owned-resource deletions): the
	// cleaned CR no longer blocks, so the ConfigMap finalizer is released.
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if cmHasFinalizer(ctx, cl, capabilities.CapabilitiesConfigFinalizer) {
		t.Fatal("expected ConfigMap finalizer to be released after Data Registry cleanup")
	}
	// The module operator finalizer still holds the Terminating ConfigMap.
	if !cmHasFinalizer(ctx, cl, "components.platform.opendatahub.io/feast-capabilities") {
		t.Fatal("expected module operator finalizer to still hold the ConfigMap")
	}
}

// TestDeletedFeatureStoreCRReleasesConfigMapFinalizer covers the gap where
// the data-registry CR is deleted while the capabilities ConfigMap is
// Terminating with teardown intent: once the CR is gone the ConfigMap watch
// mapper enqueues nothing, so the not-found reconcile triggered by the CR
// delete event must release this operator's ConfigMap finalizer. Without it
// the module operator times out and the ConfigMap stays Terminating forever
// after this operator is uninstalled.
func TestDeletedFeatureStoreCRReleasesConfigMapFinalizer(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	ctx := context.Background()
	scheme := capabilitiesFinalizerTestScheme(t)

	registryCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      services.NamespaceRegistryConfigMapName,
			Namespace: services.DefaultKubernetesNamespace,
		},
		Data: map[string]string{},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(terminatingCapabilitiesCM(true), registryCM).
		Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}

	// Reconcile for a CR that no longer exists (the delete event enqueues
	// one final request): the not-found path must release the finalizer.
	key := types.NamespacedName{Name: "data-registry", Namespace: capabilities.DefaultDataRegistryNamespace}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cmHasFinalizer(ctx, cl, capabilities.CapabilitiesConfigFinalizer) {
		t.Fatal("expected ConfigMap finalizer to be released on the not-found reconcile")
	}
	if !cmHasFinalizer(ctx, cl, "components.platform.opendatahub.io/feast-capabilities") {
		t.Fatal("expected module operator finalizer to still hold the ConfigMap")
	}
}

// TestTerminatingFeatureStoreCRFinalizeReleasesConfigMapFinalizer covers the
// finalize path: a Terminating data-registry CR drops its cleanup finalizer,
// and the same reconcile releases this operator's ConfigMap finalizer while
// the ConfigMap is Terminating with teardown intent.
func TestTerminatingFeatureStoreCRFinalizeReleasesConfigMapFinalizer(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	ctx := context.Background()
	scheme := capabilitiesFinalizerTestScheme(t)

	cr := dataRegistryCR(true)
	cr.UID = types.UID("data-registry-uid")
	now := metav1.Now()
	cr.DeletionTimestamp = &now
	registryCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      services.NamespaceRegistryConfigMapName,
			Namespace: services.DefaultKubernetesNamespace,
		},
		Data: map[string]string{},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(cr).
		WithObjects(terminatingCapabilitiesCM(true), cr, registryCM).
		Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}
	key := types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The CR finished finalizing and is gone.
	if err := cl.Get(ctx, key, &feastdevv1.FeatureStore{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected CR to be deleted, got: %v", err)
	}
	// Its final reconcile released the ConfigMap finalizer.
	if cmHasFinalizer(ctx, cl, capabilities.CapabilitiesConfigFinalizer) {
		t.Fatal("expected ConfigMap finalizer to be released during CR finalize")
	}
	if !cmHasFinalizer(ctx, cl, "components.platform.opendatahub.io/feast-capabilities") {
		t.Fatal("expected module operator finalizer to still hold the ConfigMap")
	}
}

// TestCapabilitiesFinalizerIgnoresJustCleanedCR is the informer-cache case:
// this reconcile already removed the data-registry finalizer, but List still
// returns the object with that finalizer. The hold check must not treat that
// copy as cleanup still in flight.
func TestCapabilitiesFinalizerIgnoresJustCleanedCR(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	ctx := context.Background()
	scheme := capabilitiesFinalizerTestScheme(t)

	cr := dataRegistryCR(true)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(terminatingCapabilitiesCM(true), cr).
		Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}

	err := r.reconcileCapabilitiesConfigFinalizer(ctx, capabilities.Config{
		PlatformConfigPresent:     true,
		PlatformConfigTerminating: true,
		FeatureStoreEnabled:       false,
		DataRegistryEnabled:       false,
	}, types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	if cmHasFinalizer(ctx, cl, capabilities.CapabilitiesConfigFinalizer) {
		t.Fatal("expected ConfigMap finalizer to be released despite the stale cleanup finalizer")
	}
	if !cmHasFinalizer(ctx, cl, "components.platform.opendatahub.io/feast-capabilities") {
		t.Fatal("expected module operator finalizer to still hold the ConfigMap")
	}
}

// A second data-registry CR that still carries its cleanup finalizer must
// keep the ConfigMap finalizer held even when this reconcile has finished
// cleaning a different CR.
func TestCapabilitiesFinalizerHeldByAnotherCR(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	ctx := context.Background()
	scheme := capabilitiesFinalizerTestScheme(t)

	cleaned := dataRegistryCR(true)
	other := dataRegistryCR(true)
	other.Namespace = "other-ns"
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(terminatingCapabilitiesCM(true), cleaned, other).
		Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}

	err := r.reconcileCapabilitiesConfigFinalizer(ctx, capabilities.Config{
		PlatformConfigPresent:     true,
		PlatformConfigTerminating: true,
		DataRegistryEnabled:       false,
	}, types.NamespacedName{Name: cleaned.Name, Namespace: cleaned.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	if !cmHasFinalizer(ctx, cl, capabilities.CapabilitiesConfigFinalizer) {
		t.Fatal("expected ConfigMap finalizer to stay held while another data-registry CR is still cleaning up")
	}
}

func TestCapabilitiesConfigCacheTargetsModuleConfigMap(t *testing.T) {
	const namespace = "operator-ns"
	opts := capabilitiesConfigCacheOptions(namespace)
	if len(opts.DefaultNamespaces) != 1 {
		t.Fatalf("cache namespaces: %#v", opts.DefaultNamespaces)
	}
	cfg, ok := opts.DefaultNamespaces[namespace]
	if !ok {
		t.Fatalf("expected cache limited to %s", namespace)
	}
	want := "metadata.name=" + capabilities.ConfigMapName
	if cfg.FieldSelector == nil || cfg.FieldSelector.String() != want {
		t.Fatalf("field selector = %v, want %s", cfg.FieldSelector, want)
	}
	if cfg.LabelSelector != nil && cfg.LabelSelector.String() != "" {
		t.Fatalf("label selector %q would hide the unlabeled capabilities ConfigMap", cfg.LabelSelector)
	}
}
