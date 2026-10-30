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
	"encoding/json"
	"testing"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/capabilities"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/services"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestUnannotatedDataRegistryReportsStatus(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	t.Setenv("FEAST_PLATFORM_CAPABILITIES_REQUIRED", "true")
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := feastdevv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cr := &feastdevv1.FeatureStore{ObjectMeta: metav1.ObjectMeta{
		Name:       "registry",
		Namespace:  capabilities.DefaultDataRegistryNamespace,
		Finalizers: []string{services.DataRegistryFinalizer},
	}}
	clientConfigName := "feast-registry-client"
	registryData, err := json.Marshal(services.NamespaceRegistryData{
		Namespaces: map[string][]string{
			cr.Namespace: {clientConfigName, "feast-other-client"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      services.NamespaceRegistryConfigMapName,
			Namespace: services.DefaultKubernetesNamespace,
		},
		Data: map[string]string{
			services.NamespaceRegistryDataKey: string(registryData),
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: capabilities.ConfigMapName, Namespace: "operator-ns"},
		Data: map[string]string{
			capabilities.KeyFeatureStoreEnabled:   "false",
			capabilities.KeyDataRegistryEnabled:   "true",
			capabilities.KeyDataRegistryNamespace: capabilities.DefaultDataRegistryNamespace,
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cr).WithObjects(cr, cm, registryCM).Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}
	key := types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	updated := &feastdevv1.FeatureStore{}
	if err := cl.Get(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.ReadyType)
	if ready == nil || ready.Status != metav1.ConditionFalse ||
		ready.Reason != feastdevv1.DataRegistryAnnotationRequiredReason {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
	dr := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.DataRegistryReadyType)
	if dr == nil || dr.Status != metav1.ConditionFalse ||
		dr.Reason != feastdevv1.DataRegistryAnnotationRequiredReason {
		t.Fatalf("unexpected DataRegistry condition: %#v", dr)
	}
	if updated.Status.Phase != feastdevv1.FailedPhase {
		t.Fatalf("unexpected phase: %s", updated.Status.Phase)
	}
	if services.IsDataRegistryCREnabled(updated) {
		t.Fatal("test FeatureStore unexpectedly has data-registry annotation")
	}
	for _, finalizer := range updated.Finalizers {
		if finalizer == services.DataRegistryFinalizer {
			t.Fatal("data-registry finalizer still present")
		}
	}
	stored := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(registryCM), stored); err != nil {
		t.Fatal(err)
	}
	var after services.NamespaceRegistryData
	if err := json.Unmarshal([]byte(stored.Data[services.NamespaceRegistryDataKey]), &after); err != nil {
		t.Fatal(err)
	}
	if got := after.Namespaces[cr.Namespace]; len(got) != 1 || got[0] != "feast-other-client" {
		t.Fatalf("unannotated data registry was left in discovery: %#v", after.Namespaces)
	}
}

// Older operators called deployNamespaceRegistry from data-registry mode, so an
// upgraded cluster can still list this CR in feast-configs-registry. Disabling
// the capability deletes the workload and must delete that discovery entry.
func TestDataRegistryCapabilityDisableRemovesDiscoveryEntry(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	t.Setenv("FEAST_PLATFORM_CAPABILITIES_REQUIRED", "true")
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := feastdevv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const crName = "registry"
	crNamespace := capabilities.DefaultDataRegistryNamespace
	clientConfigName := "feast-" + crName + "-client"
	workloadName := "feast-" + crName + "-" + string(services.DataRegistryFeastType)
	controller := true
	cr := &feastdevv1.FeatureStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: crNamespace,
			UID:       types.UID("data-registry-uid"),
			Annotations: map[string]string{
				services.DataRegistryAnnotation: "true",
			},
			Finalizers: []string{services.DataRegistryFinalizer},
		},
		Status: feastdevv1.FeatureStoreStatus{
			ClientConfigMap: clientConfigName,
		},
	}
	ownerRef := metav1.OwnerReference{
		APIVersion: feastdevv1.GroupVersion.String(),
		Kind:       "FeatureStore",
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: &controller,
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            workloadName,
			Namespace:       crNamespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            workloadName,
			Namespace:       crNamespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	registryData := services.NamespaceRegistryData{
		Namespaces: map[string][]string{
			crNamespace: {clientConfigName, "feast-other-client"},
			"team-ns":   {"feast-remote-client"},
		},
	}
	dataBytes, err := json.Marshal(registryData)
	if err != nil {
		t.Fatal(err)
	}
	registryCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      services.NamespaceRegistryConfigMapName,
			Namespace: services.DefaultKubernetesNamespace,
		},
		Data: map[string]string{
			services.NamespaceRegistryDataKey: string(dataBytes),
		},
	}
	capsCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      capabilities.ConfigMapName,
			Namespace: "operator-ns",
		},
		Data: map[string]string{
			capabilities.KeyFeatureStoreEnabled:   "true",
			capabilities.KeyDataRegistryEnabled:   "false",
			capabilities.KeyDataRegistryNamespace: crNamespace,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(cr).
		WithObjects(cr, deploy, svc, registryCM, capsCM).
		Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}
	key := types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}

	updated := &feastdevv1.FeatureStore{}
	if err := cl.Get(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.ReadyType)
	if ready == nil || ready.Status != metav1.ConditionFalse ||
		ready.Reason != feastdevv1.DataRegistryDisabledReason {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
	dr := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.DataRegistryReadyType)
	if dr == nil || dr.Status != metav1.ConditionFalse ||
		dr.Reason != feastdevv1.DataRegistryDisabledReason {
		t.Fatalf("unexpected DataRegistry condition: %#v", dr)
	}
	if updated.Status.Phase != feastdevv1.FailedPhase {
		t.Fatalf("unexpected phase: %s", updated.Status.Phase)
	}
	for _, finalizer := range updated.Finalizers {
		if finalizer == services.DataRegistryFinalizer {
			t.Fatal("data-registry finalizer still present after capability disable")
		}
	}
	if updated.Status.ClientConfigMap != clientConfigName {
		t.Fatalf("client config status changed: %q", updated.Status.ClientConfigMap)
	}

	err = cl.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: crNamespace}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("data-registry Deployment still present: %v", err)
	}
	err = cl.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: crNamespace}, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("data-registry Service still present: %v", err)
	}

	stored := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(registryCM), stored); err != nil {
		t.Fatal(err)
	}
	var after services.NamespaceRegistryData
	if err := json.Unmarshal([]byte(stored.Data[services.NamespaceRegistryDataKey]), &after); err != nil {
		t.Fatal(err)
	}
	got := after.Namespaces[crNamespace]
	if len(got) != 1 || got[0] != "feast-other-client" {
		t.Fatalf("discovery entry for the disabled data registry was not removed: %#v", after.Namespaces)
	}
	remote := after.Namespaces["team-ns"]
	if len(remote) != 1 || remote[0] != "feast-remote-client" {
		t.Fatalf("unrelated discovery entry was removed: %#v", after.Namespaces)
	}
}

func TestDataRegistryWrongNameRejected(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	t.Setenv("FEAST_PLATFORM_CAPABILITIES_REQUIRED", "true")
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := feastdevv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const crName = "primary-catalog"
	crNamespace := capabilities.DefaultDataRegistryNamespace
	clientConfigName := "feast-" + crName + "-client"
	workloadName := "feast-" + crName + "-" + string(services.DataRegistryFeastType)
	controller := true
	cr := &feastdevv1.FeatureStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: crNamespace,
			UID:       types.UID("wrong-name-uid"),
			Annotations: map[string]string{
				services.DataRegistryAnnotation: "true",
			},
			Finalizers: []string{services.DataRegistryFinalizer},
		},
		Spec: feastdevv1.FeatureStoreSpec{FeastProject: "customer_project"},
		Status: feastdevv1.FeatureStoreStatus{
			ClientConfigMap: clientConfigName,
		},
	}
	ownerRef := metav1.OwnerReference{
		APIVersion: feastdevv1.GroupVersion.String(),
		Kind:       "FeatureStore",
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: &controller,
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            workloadName,
			Namespace:       crNamespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            workloadName,
			Namespace:       crNamespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	registryData := services.NamespaceRegistryData{
		Namespaces: map[string][]string{
			crNamespace: {clientConfigName, "feast-other-client"},
			"team-ns":   {"feast-remote-client"},
		},
	}
	dataBytes, err := json.Marshal(registryData)
	if err != nil {
		t.Fatal(err)
	}
	registryCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      services.NamespaceRegistryConfigMapName,
			Namespace: services.DefaultKubernetesNamespace,
		},
		Data: map[string]string{
			services.NamespaceRegistryDataKey: string(dataBytes),
		},
	}
	capsCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      capabilities.ConfigMapName,
			Namespace: "operator-ns",
		},
		Data: map[string]string{
			capabilities.KeyFeatureStoreEnabled:   "true",
			capabilities.KeyDataRegistryEnabled:   "true",
			capabilities.KeyDataRegistryNamespace: crNamespace,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(cr).
		WithObjects(cr, deploy, svc, registryCM, capsCM).
		Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}
	key := types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	updated := &feastdevv1.FeatureStore{}
	if err := cl.Get(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.FeastProject != "customer_project" {
		t.Fatalf("spec.feastProject was mutated: %q", updated.Spec.FeastProject)
	}
	if updated.Status.Applied.FeastProject != "customer_project" {
		t.Fatalf("status.applied.feastProject = %q, want a CRD-valid project so the rejection status can be stored", updated.Status.Applied.FeastProject)
	}
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.ReadyType)
	if ready == nil || ready.Status != metav1.ConditionFalse ||
		ready.Reason != feastdevv1.DataRegistryNameRejectedReason {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
	dr := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.DataRegistryReadyType)
	if dr == nil || dr.Status != metav1.ConditionFalse ||
		dr.Reason != feastdevv1.DataRegistryNameRejectedReason {
		t.Fatalf("unexpected DataRegistry condition: %#v", dr)
	}
	if updated.Status.Phase != feastdevv1.FailedPhase {
		t.Fatalf("unexpected phase: %s", updated.Status.Phase)
	}
	for _, finalizer := range updated.Finalizers {
		if finalizer == services.DataRegistryFinalizer {
			t.Fatal("data-registry finalizer still present after name rejection")
		}
	}
	err = cl.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: crNamespace}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("data-registry Deployment still present: %v", err)
	}
	err = cl.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: crNamespace}, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("data-registry Service still present: %v", err)
	}
	stored := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(registryCM), stored); err != nil {
		t.Fatal(err)
	}
	var after services.NamespaceRegistryData
	if err := json.Unmarshal([]byte(stored.Data[services.NamespaceRegistryDataKey]), &after); err != nil {
		t.Fatal(err)
	}
	got := after.Namespaces[crNamespace]
	if len(got) != 1 || got[0] != "feast-other-client" {
		t.Fatalf("discovery entry for the rejected data registry was not removed: %#v", after.Namespaces)
	}
	remote := after.Namespaces["team-ns"]
	if len(remote) != 1 || remote[0] != "feast-remote-client" {
		t.Fatalf("unrelated discovery entry was removed: %#v", after.Namespaces)
	}
}

func TestDataRegistryWrongNamespaceRejected(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "operator-ns")
	t.Setenv("FEAST_PLATFORM_CAPABILITIES_REQUIRED", "true")
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := feastdevv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const crName = services.DataRegistryCRName
	crNamespace := "team-ns"
	clientConfigName := "feast-" + crName + "-client"
	workloadName := "feast-" + crName + "-" + string(services.DataRegistryFeastType)
	controller := true
	cr := &feastdevv1.FeatureStore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName,
			Namespace: crNamespace,
			UID:       types.UID("wrong-namespace-uid"),
			Annotations: map[string]string{
				services.DataRegistryAnnotation: "true",
			},
			Finalizers: []string{services.DataRegistryFinalizer},
		},
		Spec: feastdevv1.FeatureStoreSpec{FeastProject: "customer_project"},
		Status: feastdevv1.FeatureStoreStatus{
			ClientConfigMap: clientConfigName,
		},
	}
	ownerRef := metav1.OwnerReference{
		APIVersion: feastdevv1.GroupVersion.String(),
		Kind:       "FeatureStore",
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: &controller,
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            workloadName,
			Namespace:       crNamespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            workloadName,
			Namespace:       crNamespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	registryData := services.NamespaceRegistryData{
		Namespaces: map[string][]string{
			crNamespace: {clientConfigName, "feast-other-client"},
			capabilities.DefaultDataRegistryNamespace: {"feast-remote-client"},
		},
	}
	dataBytes, err := json.Marshal(registryData)
	if err != nil {
		t.Fatal(err)
	}
	registryCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      services.NamespaceRegistryConfigMapName,
			Namespace: services.DefaultKubernetesNamespace,
		},
		Data: map[string]string{
			services.NamespaceRegistryDataKey: string(dataBytes),
		},
	}
	capsCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      capabilities.ConfigMapName,
			Namespace: "operator-ns",
		},
		Data: map[string]string{
			capabilities.KeyFeatureStoreEnabled:   "true",
			capabilities.KeyDataRegistryEnabled:   "true",
			capabilities.KeyDataRegistryNamespace: capabilities.DefaultDataRegistryNamespace,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(cr).
		WithObjects(cr, deploy, svc, registryCM, capsCM).
		Build()
	r := &FeatureStoreReconciler{Client: cl, Scheme: scheme}
	key := types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	updated := &feastdevv1.FeatureStore{}
	if err := cl.Get(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.FeastProject != "customer_project" {
		t.Fatalf("spec.feastProject was mutated: %q", updated.Spec.FeastProject)
	}
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.ReadyType)
	if ready == nil || ready.Status != metav1.ConditionFalse ||
		ready.Reason != feastdevv1.DataRegistryNamespaceRejectedReason {
		t.Fatalf("unexpected Ready condition: %#v", ready)
	}
	dr := apimeta.FindStatusCondition(updated.Status.Conditions, feastdevv1.DataRegistryReadyType)
	if dr == nil || dr.Status != metav1.ConditionFalse ||
		dr.Reason != feastdevv1.DataRegistryNamespaceRejectedReason {
		t.Fatalf("unexpected DataRegistry condition: %#v", dr)
	}
	if updated.Status.Phase != feastdevv1.FailedPhase {
		t.Fatalf("unexpected phase: %s", updated.Status.Phase)
	}
	for _, finalizer := range updated.Finalizers {
		if finalizer == services.DataRegistryFinalizer {
			t.Fatal("data-registry finalizer still present after namespace rejection")
		}
	}
	err = cl.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: crNamespace}, &appsv1.Deployment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("data-registry Deployment still present: %v", err)
	}
	err = cl.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: crNamespace}, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("data-registry Service still present: %v", err)
	}
	stored := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(registryCM), stored); err != nil {
		t.Fatal(err)
	}
	var after services.NamespaceRegistryData
	if err := json.Unmarshal([]byte(stored.Data[services.NamespaceRegistryDataKey]), &after); err != nil {
		t.Fatal(err)
	}
	got := after.Namespaces[crNamespace]
	if len(got) != 1 || got[0] != "feast-other-client" {
		t.Fatalf("discovery entry for the rejected data registry was not removed: %#v", after.Namespaces)
	}
	remote := after.Namespaces[capabilities.DefaultDataRegistryNamespace]
	if len(remote) != 1 || remote[0] != "feast-remote-client" {
		t.Fatalf("unrelated discovery entry was removed: %#v", after.Namespaces)
	}
}
