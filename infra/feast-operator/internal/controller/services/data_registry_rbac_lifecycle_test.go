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
	"testing"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	"github.com/feast-dev/feast/infra/feast-operator/internal/controller/handler"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDataRegistryRBACSurvivesSameNameRecreation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := feastdevv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	makeServices := func(uid types.UID) *FeastServices {
		return &FeastServices{Handler: handler.FeastHandler{
			Client: cl, Context: ctx, Scheme: scheme,
			FeatureStore: &feastdevv1.FeatureStore{ObjectMeta: metav1.ObjectMeta{
				Name: "registry", Namespace: "rhoai-data-registry", UID: uid,
			}},
		}}
	}
	old := makeServices(types.UID("old-instance"))
	newInstance := makeServices(types.UID("new-instance"))
	if err := old.deployDataRegistryClusterRoles(); err != nil {
		t.Fatal(err)
	}
	if err := old.deployDataRegistryAuthDelegatorBinding(); err != nil {
		t.Fatal(err)
	}
	legacyName := old.legacyDataRegistryAuthDelegatorCRBName()
	legacy := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: legacyName,
		Labels: map[string]string{
			NameLabelKey: "registry", ManagedByLabelKey: ManagedByLabelValue,
		},
	}}
	if err := cl.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := newInstance.deployDataRegistryClusterRoles(); err != nil {
		t.Fatal(err)
	}
	if err := newInstance.deployDataRegistryAuthDelegatorBinding(); err != nil {
		t.Fatal(err)
	}
	if old.dataRegistryAuthDelegatorCRBName() == newInstance.dataRegistryAuthDelegatorCRBName() {
		t.Fatal("recreated FeatureStore reused the old binding name")
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: legacyName}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy binding was not migrated: %v", err)
	}
	if err := old.CleanupDataRegistryClusterRoles(); err != nil {
		t.Fatal(err)
	}
	if err := old.CleanupDataRegistryAuthDelegatorBinding(); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: DataRegistryEditorClusterRoleName}, &rbacv1.ClusterRole{}); err != nil {
		t.Fatalf("new instance ClusterRole was removed: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Name: newInstance.dataRegistryAuthDelegatorCRBName()}, &rbacv1.ClusterRoleBinding{}); err != nil {
		t.Fatalf("new instance ClusterRoleBinding was removed: %v", err)
	}
	err := cl.Get(ctx, types.NamespacedName{Name: old.dataRegistryAuthDelegatorCRBName()}, &rbacv1.ClusterRoleBinding{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("old binding was not removed: %v", err)
	}
}
