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
	"github.com/feast-dev/feast/infra/feast-operator/api/feastversion"
	feastdevv1 "github.com/feast-dev/feast/infra/feast-operator/api/v1"
	handler "github.com/feast-dev/feast/infra/feast-operator/internal/controller/handler"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	TmpFeatureStoreYamlEnvVar      = "TMP_FEATURE_STORE_YAML_BASE64"
	FeatureStoreYamlEnvVar         = "FEATURE_STORE_YAML_BASE64"
	IntraCommunicationBase64EnvVar = "INTRA_COMMUNICATION_BASE64"
	intraCommunicationTokenKey     = "token"
	packagedFeatureRepoEnvVar      = "FEAST_PACKAGED_FEATURE_REPO_PATH"
	stagedFeatureRepoEnvVar        = "FEAST_STAGED_FEATURE_REPO_PATH"
	feastServerImageVar            = "RELATED_IMAGE_FEATURE_SERVER"
	cronJobImageVar                = "RELATED_IMAGE_CRON_JOB"
	FeatureStoreYamlCmKey          = "feature_store.yaml"
	EphemeralPath                  = "/feast-data"
	FeatureRepoDir                 = "feature_repo"
	DefaultRegistryPath            = "registry.db"
	DefaultOnlineStorePath         = "online_store.db"
	svcDomain                      = ".svc.cluster.local"

	// Namespace registry ConfigMap constants
	NamespaceRegistryConfigMapName = "feast-configs-registry"
	NamespaceRegistryDataKey       = "namespaces"
	DefaultKubernetesNamespace     = "feast-operator-system"

	// OpenLineage discovery ConfigMap constants
	OpenLineageDiscoveryConfigMapName = "feast-openlineage-config"
	OpenLineageDiscoveryEndpointsKey  = "endpoints"
	OpenLineageDiscoveryYamlKey       = "openlineage.yml"
	OpenLineageDiscoveryUrlKey        = "url"

	// ProtectedProjectAnnotation is the annotation key on a FeatureStore CR
	// that marks its project as protected. Protected projects are excluded
	// from project listings and shielded from teardown by other instances.
	// When this annotation is "true", the operator sets FEAST_PROTECTED_PROJECT=true
	// on the server pods, which causes the server to tag the project in the
	// shared registry on startup.
	ProtectedProjectAnnotation = "feast.dev/protected-project"

	// DataRegistryAnnotation is the annotation key on a FeatureStore CR that
	// enables Data Registry reconciliation. When set to "true",
	// the operator switches to an exclusive data-registry mode: standard
	// online/offline store resources are removed and a single registry-only
	// Deployment with a kube-rbac-proxy sidecar is deployed instead.
	DataRegistryAnnotation = "dataregistry.opendatahub.io/enabled"

	// Data Registry env var names injected into the data-registry-server container.
	DataRegistryEnabledEnvVar      = "DATA_REGISTRY_ENABLED"
	DataRegistrySARApiGroupEnvVar  = "DATA_REGISTRY_SAR_API_GROUP"
	DataRegistrySARResourcesEnvVar = "DATA_REGISTRY_SAR_RESOURCES"
	FeastProjectEnvVar             = "FEAST_PROJECT"

	DataRegistryContainerName       = "data-registry-server"
	DataRegistryPort          int32 = 6572
	DataRegistryLocalhostAddr       = "127.0.0.1"

	// kube-rbac-proxy sidecar for Data Registry authentication enforcement.
	// RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE is the platform-shared related
	// image env var present in the ODH CSV, manager.yaml, and params.env overlays,
	// so disconnected installs inject the mirrored digest. Standalone installs
	// fall back to DefaultKubeRBACProxyImage when the env var is unset.
	kubeRBACProxyImageVar                = "RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE"
	DataRegistryProxyContainerName       = "kube-rbac-proxy"
	DataRegistryProxyPort          int32 = 8443
	dataRegistryAuthConfigSuffix         = "-data-registry-auth"
	dataRegistryTlsSecretSuffix          = "-data-registry-tls"

	dataRegistryAuthDelegatorSuffix = "-data-registry-auth-delegator"
	dataRegistryOwnerUIDLabelKey    = "feast.dev/featurestore-uid"
	dataRegistryCaBundleSuffix      = "-data-registry-ca-bundle"

	dataRegistryAPIGroup = "dataregistry.opendatahub.io"

	// Fixed ClusterRole names for the data-registry aggregated RBAC.
	// Names are CR-independent because data-registry is a cluster singleton:
	// only one FeatureStore may have the data-registry annotation at a time.
	// Using fixed names avoids dynamic resourceNames in the operator ClusterRole
	// and prevents Forbidden errors when the OLM-installed operator tries to
	// get/update/delete a ClusterRole whose name was not pre-listed.
	DataRegistryViewerClusterRoleName = "feast-data-registry-viewer"
	DataRegistryEditorClusterRoleName = "feast-data-registry-editor"
	DataRegistryAdminClusterRoleName  = "feast-data-registry-admin"

	// DataRegistryProject is the fixed Feast project name used in
	// data-registry mode (Phase-1 single-project storage model).
	DataRegistryProject = "data_registry"

	// DataRegistryNamespaceLabel is the label that must be present on a
	// namespace for the operator to allow a data-registry CR in it.
	// This is a secondary guard; the primary enforcement is the exact namespace
	// name match (always rhoai-data-registry). Custom namespace selection is deferred.
	DataRegistryNamespaceLabel = "opendatahub.io/data-registry"
	// DataRegistryPlatformNamespaceLabel is applied by feast-module-operator on rhoai-data-registry.
	DataRegistryPlatformNamespaceLabel = "dataregistry.opendatahub.io/enabled"

	// DataRegistryFinalizer is added to the FeatureStore CR when the data-registry
	// annotation is enabled. It prevents the CR from being garbage-collected before
	// the operator can clean up cluster-scoped resources (ClusterRoles, CRBs)
	// that do not carry owner references and would otherwise be orphaned.
	DataRegistryFinalizer = "dataregistry.opendatahub.io/cleanup"

	// DefaultKubeRBACProxyImage is the fallback when RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE
	// is unset (standalone make deploy). ODH/RHOAI installs inject the related image via OLM.
	// Requires opendatahub-io/kube-rbac-proxy with Format2 endpoint rules (PR #28, v3.6.0-ea.2+).
	DefaultKubeRBACProxyImage = "quay.io/opendatahub/odh-kube-rbac-proxy@sha256:acd9de62b8339dbacd39635317e5e596e09a70ae26ac9eccdde4a4124388740f"

	DefaultKubeRBACProxyCPURequest    = "50m"
	DefaultKubeRBACProxyCPULimit      = "100m"
	DefaultKubeRBACProxyMemoryRequest = "128Mi"
	DefaultKubeRBACProxyMemoryLimit   = "256Mi"

	DefaultDataRegistryCPURequest    = "100m"
	DefaultDataRegistryCPULimit      = "500m"
	DefaultDataRegistryMemoryRequest = "256Mi"
	DefaultDataRegistryMemoryLimit   = "512Mi"

	HttpPort              = 80
	HttpsPort             = 443
	HttpScheme            = "http"
	HttpsScheme           = "https"
	tlsPath               = "/tls/"
	tlsPathCustomCABundle = "/etc/pki/tls/custom-certs/ca-bundle.crt"
	tlsNameSuffix         = "-tls"

	caBundleAnnotation                   = "config.openshift.io/inject-trusted-cabundle"
	caBundleName                         = "odh-trusted-ca-bundle"
	odhCaBundleKey                       = "odh-ca-bundle.crt"
	tlsPathOdhCABundle                   = "/etc/pki/tls/custom-certs/odh-ca-bundle.crt"
	tlsPathOidcCA                        = "/etc/pki/tls/oidc-ca/ca.crt"
	oidcCaVolumeName                     = "oidc-ca-cert"
	defaultCACertKey                     = "ca-bundle.crt"
	openshiftServingCertSecretAnnotation = "service.beta.openshift.io/serving-cert-secret-name" // pragma: allowlist secret
	openshiftServingCertSansAnnotation   = "service.beta.openshift.io/serving-cert-sans"
	openshiftInjectCaBundleAnnotation    = "service.beta.openshift.io/inject-cabundle"
	ErrorMessagePrefix                   = "Error: "

	DefaultOfflineStorageRequest        = "20Gi"
	DefaultOnlineStorageRequest         = "5Gi"
	DefaultRegistryStorageRequest       = "5Gi"
	MetricsPort                   int32 = 8000

	AuthzFeastType        FeastServiceType = "authorization"
	OfflineFeastType      FeastServiceType = "offline"
	OnlineFeastType       FeastServiceType = "online"
	RegistryFeastType     FeastServiceType = "registry"
	UIFeastType           FeastServiceType = "ui"
	LineageFeastType      FeastServiceType = "lineage"
	McpServerFeastType    FeastServiceType = "mcpserver"
	ClientFeastType       FeastServiceType = "client"
	ClientCaFeastType     FeastServiceType = "client-ca"
	CronJobFeastType      FeastServiceType = "cronjob"
	DataRegistryFeastType FeastServiceType = "data-registry"

	OfflineRemoteConfigType                 OfflineConfigType = "remote"
	OfflineFilePersistenceDaskConfigType    OfflineConfigType = "dask"
	OfflineFilePersistenceDuckDbConfigType  OfflineConfigType = "duckdb"
	OfflineDBPersistenceSnowflakeConfigType OfflineConfigType = "snowflake.offline"

	OnlineRemoteConfigType                 OnlineConfigType = "remote"
	OnlineSqliteConfigType                 OnlineConfigType = "sqlite"
	OnlineDBPersistenceSnowflakeConfigType OnlineConfigType = "snowflake.online"
	OnlineDBPersistenceCassandraConfigType OnlineConfigType = "cassandra"

	RegistryRemoteConfigType                 RegistryConfigType = "remote"
	RegistryFileConfigType                   RegistryConfigType = "file"
	RegistryDBPersistenceSnowflakeConfigType RegistryConfigType = "snowflake.registry"
	RegistryDBPersistenceSQLConfigType       RegistryConfigType = "sql"

	LocalProviderType FeastProviderType = "local"

	NoAuthAuthType     AuthzType = "no_auth"
	KubernetesAuthType AuthzType = "kubernetes"
	OidcAuthType       AuthzType = "oidc"

	OidcClientId         OidcPropertyType = "client_id"
	OidcAuthDiscoveryUrl OidcPropertyType = "auth_discovery_url"
	OidcClientSecret     OidcPropertyType = "client_secret"
	OidcUsername         OidcPropertyType = "username"
	OidcPassword         OidcPropertyType = "password"
	OidcTokenEnvVar      OidcPropertyType = "token_env_var"
	OidcVerifySsl        OidcPropertyType = "verify_ssl"
	OidcCaCertPath       OidcPropertyType = "ca_cert_path"
	OidcAudience         OidcPropertyType = "audience"
	OidcIssuer           OidcPropertyType = "issuer"

	OidcJwksCacheLifespanSeconds  OidcPropertyType = "jwks_cache_lifespan_seconds"
	OidcJwksRequestTimeoutSeconds OidcPropertyType = "jwks_request_timeout_seconds"

	OidcMissingSecretError string = "missing OIDC secret: %s"

	// Common string constants
	stringTrue              = "true"
	stringFalse             = "false"
	hostAllIPv4             = "0.0.0.0"
	hostAllIPv6             = "::"
	hostAllIPv6Bracketed    = "[::]"
	tlsCertKey              = "tls.crt"
	DefaultNs               = "default"
	feastCommand            = "feast"
	metricsPortName         = "metrics"
	registryName            = "registry"
	feastInitContainerName  = "feast-init"
	feastApplyContainerName = "feast-apply"

	// MCP server (standalone `feast mcp`) constants
	mcpServerConfigVolumeName = "mcp-server-config"
	mcpServerConfigMountPath  = "/etc/feast/mcp"
	mcpServerConfigDefaultKey = "feast_mcp.yaml"
	mcpServerTransportEnvVar  = "FEAST_MCP_TRANSPORT"

	// Test-specific constants
	dataOnlineDbPath              = "/data/online.db"
	dataRegistryDbPath            = "/data/registry.db"
	oidcSecretName                = "oidc-secret" // pragma: allowlist secret
	clientIDValue                 = "client-id"
	lineageSecretName             = "lineage-secret" // pragma: allowlist secret
	redisType                     = "redis"
	redisSecretName               = "redis-secret"    // pragma: allowlist secret
	registrySecretName            = "registry-secret" // pragma: allowlist secret
	celTestProject                = "celtest"
	kubernetesHostnameTopologyKey = "kubernetes.io/hostname"
	dailyMidnightCron             = "0 0 * * *"
	otelInjectPythonAnnotation    = "instrumentation.opentelemetry.io/inject-python"
	kubernetesOsLabel             = "kubernetes.io/os"
	computeNodeType               = "compute"
	nodeTypeLabel                 = "node-type"
	zoneLabel                     = "zone"
	linuxOS                       = "linux"
	TestValue                     = "test"
	OfflineStoreSecretName        = "offline-store-secret"  // pragma: allowlist secret
	OnlineStoreSecretName         = "online-store-secret"   // pragma: allowlist secret
	RegistryStoreSecretName       = "registry-store-secret" // pragma: allowlist secret
	FieldRefName                  = "fieldRefName"
	ConfigOne                     = "config-1"
	MetadataNameField             = "metadata.name"
	GrpcFlag                      = "--grpc"
	ExampleConfigMapName          = "example-configmap"
	ExampleSecretName             = "example-secret" // pragma: allowlist secret
)

const (
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "feast-operator"
)

var (
	DefaultImage          = "quay.io/feastdev/feature-server:" + feastversion.FeastVersion
	DefaultCronJobImage   = "registry.k8s.io/kubectl:v1.35.9"
	DefaultPVCAccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	NameLabelKey          = feastdevv1.GroupVersion.Group + "/name"
	ServiceTypeLabelKey   = feastdevv1.GroupVersion.Group + "/service-type"

	// dataRegistryPseudoResources are Data Registry pseudo-resources granted on the
	// aggregated ClusterRoles. kube-rbac-proxy's coarse gate uses `registries`;
	// the rest are consumed by server-side SAR (namespaces/tables/volumes).
	dataRegistryPseudoResources = []string{"registries", "namespaces", "tables", "volumes", "generic-tables"}

	FeastServiceConstants = map[FeastServiceType]deploymentSettings{
		OfflineFeastType: {
			Args:            []string{"serve_offline", "-h", hostAllIPv4},
			TargetHttpPort:  8815,
			TargetHttpsPort: 8816,
		},
		OnlineFeastType: {
			Args:            []string{"serve", "-h", hostAllIPv4},
			TargetHttpPort:  6566,
			TargetHttpsPort: 6567,
		},
		RegistryFeastType: {
			Args:                []string{"serve_registry"},
			TargetHttpPort:      6570,
			TargetHttpsPort:     6571,
			TargetRestHttpPort:  6572,
			TargetRestHttpsPort: 6573,
		},
		UIFeastType: {
			Args:            []string{"ui", "-h", hostAllIPv4},
			TargetHttpPort:  8888,
			TargetHttpsPort: 8443,
		},
		LineageFeastType: {
			Args:            []string{"serve_lineage", "-h", hostAllIPv4},
			TargetHttpPort:  6580,
			TargetHttpsPort: 6581,
		},
		McpServerFeastType: {
			Args:            []string{"mcp"},
			TargetHttpPort:  8100,
			TargetHttpsPort: 8101,
		},
		DataRegistryFeastType: {
			Args:               []string{"serve_registry", "--rest-api"},
			TargetHttpPort:     DataRegistryPort,
			TargetRestHttpPort: DataRegistryPort,
		},
	}

	FeastServiceConditions = map[FeastServiceType]map[metav1.ConditionStatus]metav1.Condition{
		OfflineFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.OfflineStoreReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.OfflineStoreReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.OfflineStoreReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.OfflineStoreFailedReason,
			},
		},
		OnlineFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.OnlineStoreReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.OnlineStoreReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.OnlineStoreReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.OnlineStoreFailedReason,
			},
		},
		RegistryFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.RegistryReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.RegistryReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.RegistryReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.RegistryFailedReason,
			},
		},
		UIFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.UIReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.UIReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.UIReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.UIFailedReason,
			},
		},
		LineageFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.LineageReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.LineageReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.LineageReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.LineageFailedReason,
			},
		},
		McpServerFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.McpServerReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.McpServerReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.McpServerReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.McpServerFailedReason,
			},
		},
		ClientFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.ClientReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.ClientReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.ClientReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.ClientFailedReason,
			},
		},
		CronJobFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.CronJobReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.CronJobReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.CronJobReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.CronJobFailedReason,
			},
		},
		DataRegistryFeastType: {
			metav1.ConditionTrue: {
				Type:    feastdevv1.DataRegistryReadyType,
				Status:  metav1.ConditionTrue,
				Reason:  feastdevv1.ReadyReason,
				Message: feastdevv1.DataRegistryReadyMessage,
			},
			metav1.ConditionFalse: {
				Type:   feastdevv1.DataRegistryReadyType,
				Status: metav1.ConditionFalse,
				Reason: feastdevv1.DataRegistryFailedReason,
			},
		},
	}

	OidcOptionalSecretProperties = []OidcPropertyType{OidcAuthDiscoveryUrl, OidcClientId, OidcClientSecret, OidcUsername, OidcPassword, OidcAudience, OidcIssuer}
)

// Feast server types: Reserved only for server types like Online, Offline, and Registry servers. Should not be used for client types like the UI, etc.
var feastServerTypes = []FeastServiceType{
	RegistryFeastType,
	OfflineFeastType,
	OnlineFeastType,
}

// AuthzType defines the authorization type
type AuthzType string

// OidcPropertyType defines the OIDC property type
type OidcPropertyType string

// FeastServiceType is the type of feast service
type FeastServiceType string

// OfflineConfigType provider name or a class name that implements Offline Store
type OfflineConfigType string

// RegistryConfigType provider name or a class name that implements Registry
type RegistryConfigType string

// OnlineConfigType provider name or a class name that implements Online Store
type OnlineConfigType string

// FeastProviderType defines an implementation of a feature store object
type FeastProviderType string

// FeastServices is an interface for configuring and deploying feast services
type FeastServices struct {
	Handler handler.FeastHandler
}

// RepoConfig is the Repo config. Typically loaded from feature_store.yaml.
// https://rtd.feast.dev/en/stable/#feast.repo_config.RepoConfig
type RepoConfig struct {
	Project                       string                           `yaml:"project,omitempty"`
	Provider                      FeastProviderType                `yaml:"provider,omitempty"`
	OfflineStore                  OfflineStoreConfig               `yaml:"offline_store,omitempty"`
	OnlineStore                   OnlineStoreConfig                `yaml:"online_store,omitempty"`
	Registry                      RegistryConfig                   `yaml:"registry,omitempty"`
	AuthzConfig                   AuthzConfig                      `yaml:"auth,omitempty"`
	EntityKeySerializationVersion int                              `yaml:"entity_key_serialization_version,omitempty"`
	BatchEngine                   *ComputeEngineConfig             `yaml:"batch_engine,omitempty"`
	FeatureServer                 *FeatureServerYamlConfig         `yaml:"feature_server,omitempty"`
	Materialization               *MaterializationYamlConfig       `yaml:"materialization,omitempty"`
	OpenLineage                   *OpenLineageYamlConfig           `yaml:"openlineage,omitempty"`
	Mlflow                        *MlflowYamlConfig                `yaml:"mlflow,omitempty"`
	DataQualityMonitoring         *DataQualityMonitoringYamlConfig `yaml:"data_quality_monitoring,omitempty"`
}

// FeatureServerYamlConfig maps to the feature_server section of feature_store.yaml.
// Field names match Feast's Python SDK YAML keys exactly.
type FeatureServerYamlConfig struct {
	Type                                    string             `yaml:"type"`
	Metrics                                 *MetricsYamlConfig `yaml:"metrics,omitempty"`
	OfflinePushBatchingEnabled              *bool              `yaml:"offline_push_batching_enabled,omitempty"`
	OfflinePushBatchingBatchSize            *int32             `yaml:"offline_push_batching_batch_size,omitempty"`
	OfflinePushBatchingBatchIntervalSeconds *int32             `yaml:"offline_push_batching_batch_interval_seconds,omitempty"`
	McpEnabled                              *bool              `yaml:"mcp_enabled,omitempty"`
	McpServerName                           *string            `yaml:"mcp_server_name,omitempty"`
	McpServerVersion                        *string            `yaml:"mcp_server_version,omitempty"`
	McpTransport                            *string            `yaml:"mcp_transport,omitempty"`
}

// MetricsYamlConfig maps to the feature_server.metrics section of feature_store.yaml.
// Category booleans are merged inline so they sit at the same YAML level as
// "enabled". Keys must be valid Feast MetricsConfig field names for the SDK
// version in use (e.g. resource, request, online_features, push,
// materialization, freshness). Note: Feast's MetricsConfig uses
// extra="forbid", so unknown keys will be rejected by SDK validation.
type MetricsYamlConfig struct {
	Enabled    bool                   `yaml:"enabled"`
	Categories map[string]interface{} `yaml:",inline,omitempty"`
}

// DataQualityMonitoringYamlConfig mirrors the Python DqmConfig in feature_store.yaml.
type DataQualityMonitoringYamlConfig struct {
	AutoBaseline bool `yaml:"auto_baseline"`
}

// MaterializationYamlConfig maps to the materialization section of feature_store.yaml.
// ExtraConfig is merged inline so future Feast MaterializationConfig fields appear
// at the same YAML level as the typed fields above.
type MaterializationYamlConfig struct {
	OnlineWriteBatchSize *int32                 `yaml:"online_write_batch_size,omitempty"`
	ExtraConfig          map[string]interface{} `yaml:",inline,omitempty"`
}

// OpenLineageYamlConfig maps to the openlineage section of feature_store.yaml.
// ExtraConfig is merged inline so all extra key-value pairs (namespace, producer,
// emit_on_apply, emit_on_materialize, transport-specific options, etc.) appear at
// the same YAML level as the typed connection fields.
type OpenLineageYamlConfig struct {
	Enabled           bool                           `yaml:"enabled"`
	TransportType     *string                        `yaml:"transport_type,omitempty"`
	TransportUrl      *string                        `yaml:"transport_url,omitempty"`
	TransportEndpoint *string                        `yaml:"transport_endpoint,omitempty"`
	ApiKey            *string                        `yaml:"api_key,omitempty"`
	ExtraConfig       map[string]interface{}         `yaml:",inline,omitempty"`
	Consumer          *OpenLineageConsumerYamlConfig `yaml:"consumer,omitempty"`
}

// OpenLineageConsumerYamlConfig maps to the openlineage.consumer section of feature_store.yaml.
type OpenLineageConsumerYamlConfig struct {
	Enabled                     bool              `yaml:"enabled"`
	StoreType                   *string           `yaml:"store_type,omitempty"`
	ConnectionString            *string           `yaml:"connection_string,omitempty"`
	ApiKey                      *string           `yaml:"api_key,omitempty"`
	NamespaceMapping            map[string]string `yaml:"namespace_mapping,omitempty"`
	RetentionDays               *int32            `yaml:"retention_days,omitempty"`
	RetentionCheckIntervalHours *int32            `yaml:"retention_check_interval_hours,omitempty"`
	StandaloneServer            *bool             `yaml:"standalone_server,omitempty"`
}

// MlflowYamlConfig maps to the mlflow section of feature_store.yaml.
// ExtraConfig is merged inline so additional key-value pairs appear at the same
// YAML level as the typed fields.
type MlflowYamlConfig struct {
	Enabled             bool                   `yaml:"enabled"`
	TrackingUri         *string                `yaml:"tracking_uri,omitempty"`
	UiUrl               *string                `yaml:"ui_url,omitempty"`
	AutoLog             *bool                  `yaml:"auto_log,omitempty"`
	AutoLogEntityDf     *bool                  `yaml:"auto_log_entity_df,omitempty"`
	EntityDfMaxRows     *int32                 `yaml:"entity_df_max_rows,omitempty"`
	LogOperations       *bool                  `yaml:"log_operations,omitempty"`
	OpsExperimentSuffix *string                `yaml:"ops_experiment_suffix,omitempty"`
	ExtraConfig         map[string]interface{} `yaml:",inline,omitempty"`
}

// OfflineStoreConfig is the configuration that relates to reading from and writing to the Feast offline store.
type OfflineStoreConfig struct {
	Host         string                 `yaml:"host,omitempty"`
	Type         OfflineConfigType      `yaml:"type,omitempty"`
	Port         int                    `yaml:"port,omitempty"`
	Scheme       string                 `yaml:"scheme,omitempty"`
	Cert         string                 `yaml:"cert,omitempty"`
	DBParameters map[string]interface{} `yaml:",inline,omitempty"`
}

// OnlineStoreConfig is the configuration that relates to reading from and writing to the Feast online store.
type OnlineStoreConfig struct {
	Path         string                 `yaml:"path,omitempty"`
	Type         OnlineConfigType       `yaml:"type,omitempty"`
	Cert         string                 `yaml:"cert,omitempty"`
	DBParameters map[string]interface{} `yaml:",inline,omitempty"`
}

// RegistryConfig is the configuration that relates to reading from and writing to the Feast registry.
type RegistryConfig struct {
	Path               string                 `yaml:"path,omitempty"`
	RegistryType       RegistryConfigType     `yaml:"registry_type,omitempty"`
	Cert               string                 `yaml:"cert,omitempty"`
	S3AdditionalKwargs *map[string]string     `yaml:"s3_additional_kwargs,omitempty"`
	CacheTTLSeconds    *int32                 `yaml:"cache_ttl_seconds,omitempty"`
	CacheMode          *string                `yaml:"cache_mode,omitempty"`
	Mcp                *RegistryMcpYamlConfig `yaml:"mcp,omitempty"`
	DBParameters       map[string]interface{} `yaml:",inline,omitempty"`
}

// RegistryMcpYamlConfig maps to the registry.mcp section of feature_store.yaml.
type RegistryMcpYamlConfig struct {
	Enabled bool `yaml:"enabled"`
}

// AuthzConfig is the RBAC authorization configuration.
type AuthzConfig struct {
	Type           AuthzType              `yaml:"type,omitempty"`
	OidcParameters map[string]interface{} `yaml:",inline,omitempty"`
}

// ComputeEngineConfig is the configuration for batch compute engine.
type ComputeEngineConfig struct {
	Type       string                 `yaml:"type,omitempty"`
	Parameters map[string]interface{} `yaml:",inline,omitempty"`
}

type deploymentSettings struct {
	Args                []string
	TargetHttpPort      int32
	TargetHttpsPort     int32
	TargetRestHttpPort  int32
	TargetRestHttpsPort int32
}

// CustomCertificatesBundle represents a custom CA bundle configuration
type CustomCertificatesBundle struct {
	IsDefined     bool
	VolumeName    string
	ConfigMapName string
}
