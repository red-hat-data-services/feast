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
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ConfigMapName is written by feast-module-operator into the operand namespace.
	ConfigMapName = "feast-capabilities-config"

	KeyFeatureStoreEnabled       = "featureStoreEnabled"
	KeyDataRegistryEnabled       = "dataRegistryEnabled"
	KeyDataRegistryNamespace     = "dataRegistryNamespace"
	DefaultDataRegistryNamespace = "rhoai-data-registry"

	podNamespaceEnvVar                 = "POD_NAMESPACE"
	platformCapabilitiesRequiredEnvVar = "FEAST_PLATFORM_CAPABILITIES_REQUIRED"
)

// Config holds platform capability toggles projected by feast-module-operator.
type Config struct {
	FeatureStoreEnabled   bool
	DataRegistryEnabled   bool
	DataRegistryNamespace string
	PlatformConfigPresent bool
}

// OperatorNamespace returns the namespace where the manager pod runs (operand install NS).
func OperatorNamespace() string {
	return os.Getenv(podNamespaceEnvVar)
}

// IsPlatformCapabilitiesRequired returns true when the operator is deployed
// in strict platform mode (FEAST_PLATFORM_CAPABILITIES_REQUIRED=true).
func IsPlatformCapabilitiesRequired() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(platformCapabilitiesRequiredEnvVar)), "true")
}

// Load reads feast-capabilities-config from the operator namespace.
// If the ConfigMap or POD_NAMESPACE is missing, behavior depends on
// FEAST_PLATFORM_CAPABILITIES_REQUIRED:
//   - "true": returns an error so the controller blocks reconciliation
//   - unset/false: both capabilities default to enabled (standalone mode)
func Load(ctx context.Context, c client.Client) (Config, error) {
	defaults := Config{
		FeatureStoreEnabled:   true,
		DataRegistryEnabled:   true,
		DataRegistryNamespace: DefaultDataRegistryNamespace,
		PlatformConfigPresent: false,
	}

	strictMode := IsPlatformCapabilitiesRequired()

	ns := OperatorNamespace()
	if ns == "" {
		if strictMode {
			return defaults, fmt.Errorf(
				"%s ConfigMap is required but POD_NAMESPACE is not set; "+
					"cannot locate the capabilities ConfigMap", ConfigMapName)
		}
		return defaults, nil
	}

	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: ns}, cm)
	if apierrors.IsNotFound(err) {
		if strictMode {
			return defaults, &PlatformConfigMissingError{
				Namespace: ns,
			}
		}
		return defaults, nil
	}
	if err != nil {
		return defaults, fmt.Errorf("read %s/%s: %w", ns, ConfigMapName, err)
	}

	out := Config{
		FeatureStoreEnabled:   true,
		DataRegistryEnabled:   true,
		DataRegistryNamespace: DefaultDataRegistryNamespace,
		PlatformConfigPresent: true,
	}

	if v, ok := cm.Data[KeyFeatureStoreEnabled]; ok {
		parsed, parseErr := parseBoolData(v)
		if parseErr != nil {
			if strictMode {
				return defaults, fmt.Errorf("invalid %s ConfigMap: %s: %w", ConfigMapName, KeyFeatureStoreEnabled, parseErr)
			}
			return defaults, fmt.Errorf("%s: %w", KeyFeatureStoreEnabled, parseErr)
		}
		out.FeatureStoreEnabled = parsed
	} else if strictMode {
		return defaults, fmt.Errorf("invalid %s ConfigMap: required key %q is missing", ConfigMapName, KeyFeatureStoreEnabled)
	}

	if v, ok := cm.Data[KeyDataRegistryEnabled]; ok {
		parsed, parseErr := parseBoolData(v)
		if parseErr != nil {
			if strictMode {
				return defaults, fmt.Errorf("invalid %s ConfigMap: %s: %w", ConfigMapName, KeyDataRegistryEnabled, parseErr)
			}
			return defaults, fmt.Errorf("%s: %w", KeyDataRegistryEnabled, parseErr)
		}
		out.DataRegistryEnabled = parsed
	} else if strictMode {
		return defaults, fmt.Errorf("invalid %s ConfigMap: required key %q is missing", ConfigMapName, KeyDataRegistryEnabled)
	}

	if v, ok := cm.Data[KeyDataRegistryNamespace]; ok {
		trimmed := strings.TrimSpace(v)
		if trimmed == "" && strictMode && out.DataRegistryEnabled {
			return defaults, fmt.Errorf("invalid %s ConfigMap: %s is enabled but %s is empty",
				ConfigMapName, KeyDataRegistryEnabled, KeyDataRegistryNamespace)
		}
		if trimmed != "" {
			out.DataRegistryNamespace = trimmed
		}
	}

	return out, nil
}

// PlatformConfigMissingError is returned when FEAST_PLATFORM_CAPABILITIES_REQUIRED=true
// and the feast-capabilities-config ConfigMap does not exist.
type PlatformConfigMissingError struct {
	Namespace string
}

func (e *PlatformConfigMissingError) Error() string {
	return fmt.Sprintf(
		"%s ConfigMap is required but not found in namespace %q. "+
			"Waiting for the module operator to create it.",
		ConfigMapName, e.Namespace)
}

func parseBoolData(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q (want true or false)", value)
	}
}
