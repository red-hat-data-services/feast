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
	"testing"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/capabilities"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/services"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
		Name: "registry", Namespace: capabilities.DefaultDataRegistryNamespace,
	}}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: capabilities.ConfigMapName, Namespace: "operator-ns"},
		Data: map[string]string{
			capabilities.KeyFeatureStoreEnabled:   "false",
			capabilities.KeyDataRegistryEnabled:   "true",
			capabilities.KeyDataRegistryNamespace: capabilities.DefaultDataRegistryNamespace,
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cr).WithObjects(cr, cm).Build()
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
}
