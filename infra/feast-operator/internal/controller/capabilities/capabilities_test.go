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

package capabilities

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
)

func TestLoadDefaultsWhenConfigMapMissing(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "feast-operator-system")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = feastdevv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	cfg, err := Load(context.Background(), cl)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.FeatureStoreEnabled || !cfg.DataRegistryEnabled {
		t.Fatalf("expected both enabled, got %+v", cfg)
	}
	if cfg.DataRegistryNamespace != DefaultDataRegistryNamespace {
		t.Fatalf("expected default DR namespace %q, got %q", DefaultDataRegistryNamespace, cfg.DataRegistryNamespace)
	}
	if cfg.PlatformConfigPresent {
		t.Fatal("expected PlatformConfigPresent=false when ConfigMap is missing")
	}
}

func TestLoadFromConfigMap(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{
			KeyFeatureStoreEnabled: "false",
			KeyDataRegistryEnabled: "true",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	cfg, err := Load(context.Background(), cl)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FeatureStoreEnabled {
		t.Fatal("expected feature store disabled")
	}
	if !cfg.DataRegistryEnabled {
		t.Fatal("expected data registry enabled")
	}
	if !cfg.PlatformConfigPresent {
		t.Fatal("expected PlatformConfigPresent=true when ConfigMap exists")
	}
}

func TestLoadDataRegistryNamespaceFromConfigMap(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{
			KeyFeatureStoreEnabled:   "true",
			KeyDataRegistryEnabled:   "true",
			KeyDataRegistryNamespace: "rhoai-data-registry",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	cfg, err := Load(context.Background(), cl)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataRegistryNamespace != "rhoai-data-registry" {
		t.Fatalf("expected DR namespace %q, got %q", "rhoai-data-registry", cfg.DataRegistryNamespace)
	}
}

func TestLoadDataRegistryNamespaceDefaultsWhenAbsent(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{
			KeyFeatureStoreEnabled: "true",
			KeyDataRegistryEnabled: "true",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	cfg, err := Load(context.Background(), cl)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataRegistryNamespace != DefaultDataRegistryNamespace {
		t.Fatalf("expected default DR namespace %q, got %q", DefaultDataRegistryNamespace, cfg.DataRegistryNamespace)
	}
}

func TestStrictModeBlocksWhenConfigMapMissing(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")
	t.Setenv(platformCapabilitiesRequiredEnvVar, "true")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = feastdevv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := Load(context.Background(), cl)
	if err == nil {
		t.Fatal("expected error in strict mode with missing ConfigMap")
	}
	var pmErr *PlatformConfigMissingError
	if !errors.As(err, &pmErr) {
		t.Fatalf("expected PlatformConfigMissingError, got %T: %v", err, err)
	}
}

func TestStrictModeSucceedsWithValidConfigMap(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")
	t.Setenv(platformCapabilitiesRequiredEnvVar, "true")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{
			KeyFeatureStoreEnabled:   "false",
			KeyDataRegistryEnabled:   "true",
			KeyDataRegistryNamespace: "rhoai-data-registry",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	cfg, err := Load(context.Background(), cl)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FeatureStoreEnabled {
		t.Fatal("expected feature store disabled")
	}
	if !cfg.DataRegistryEnabled {
		t.Fatal("expected data registry enabled")
	}
	if cfg.DataRegistryNamespace != "rhoai-data-registry" {
		t.Fatalf("expected DR namespace %q, got %q", "rhoai-data-registry", cfg.DataRegistryNamespace)
	}
}

func TestStrictModeRejectsMissingRequiredKeys(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")
	t.Setenv(platformCapabilitiesRequiredEnvVar, "true")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	_, err := Load(context.Background(), cl)
	if err == nil {
		t.Fatal("expected error for missing required keys in strict mode")
	}
}

func TestStrictModeRejectsInvalidBoolValues(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")
	t.Setenv(platformCapabilitiesRequiredEnvVar, "true")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{
			KeyFeatureStoreEnabled: "yes",
			KeyDataRegistryEnabled: "true",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	_, err := Load(context.Background(), cl)
	if err == nil {
		t.Fatal("expected error for invalid bool value in strict mode")
	}
}

func TestNonStrictModeDefaultsWhenConfigMapMissing(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")
	t.Setenv(platformCapabilitiesRequiredEnvVar, "false")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = feastdevv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	cfg, err := Load(context.Background(), cl)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.FeatureStoreEnabled || !cfg.DataRegistryEnabled {
		t.Fatalf("expected both enabled in non-strict mode, got %+v", cfg)
	}
}

func TestStrictModeRejectsExplicitlyEmptyNamespace(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")
	t.Setenv(platformCapabilitiesRequiredEnvVar, "true")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{
			KeyFeatureStoreEnabled:   "false",
			KeyDataRegistryEnabled:   "true",
			KeyDataRegistryNamespace: "",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	_, err := Load(context.Background(), cl)
	if err == nil {
		t.Fatal("expected error for explicitly empty namespace with data registry enabled in strict mode")
	}
	if !strings.Contains(err.Error(), KeyDataRegistryNamespace) {
		t.Fatalf("error should mention %s, got: %v", KeyDataRegistryNamespace, err)
	}
}

func TestStrictModeAcceptsWhitespaceOnlyNamespaceAsEmpty(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")
	t.Setenv(platformCapabilitiesRequiredEnvVar, "true")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{
			KeyFeatureStoreEnabled:   "false",
			KeyDataRegistryEnabled:   "true",
			KeyDataRegistryNamespace: "  ",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	_, err := Load(context.Background(), cl)
	if err == nil {
		t.Fatal("expected error for whitespace-only namespace with data registry enabled in strict mode")
	}
}

func TestNonStrictModeDefaultsNamespaceWhenExplicitlyEmpty(t *testing.T) {
	t.Setenv(podNamespaceEnvVar, "redhat-ods-applications")
	t.Setenv(platformCapabilitiesRequiredEnvVar, "false")

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: "redhat-ods-applications",
		},
		Data: map[string]string{
			KeyFeatureStoreEnabled:   "true",
			KeyDataRegistryEnabled:   "true",
			KeyDataRegistryNamespace: "",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	cfg, err := Load(context.Background(), cl)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataRegistryNamespace != DefaultDataRegistryNamespace {
		t.Fatalf("expected default namespace %q for empty value in non-strict mode, got %q",
			DefaultDataRegistryNamespace, cfg.DataRegistryNamespace)
	}
}
