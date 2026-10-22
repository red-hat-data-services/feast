# Guide 9 — Data Registry

The operator can provision a **Data Registry** server — a shared, cross-namespace Feast REST
API for data asset discovery. It is deployed as a dedicated two-container pod (feast-server +
kube-rbac-proxy sidecar) with its own HTTPS Service, Route, RBAC, and observability stack.

---

## Enabling the Data Registry

The Data Registry is activated by annotating a `FeatureStore` CR:

```yaml
apiVersion: feast.dev/v1
kind: FeatureStore
metadata:
  name: data-registry
  annotations:
    dataregistry.opendatahub.io/enabled: "true"
spec:
  feastProject: data_registry
```

{% hint style="info" %}
The example above uses the minimal configuration. All standalone FeatureStore
customizations (registry backend, persistence, resources, TLS, etc.) are also
available for data-registry CRs. For instance, to use a SQL-backed registry:

```yaml
apiVersion: feast.dev/v1
kind: FeatureStore
metadata:
  name: data-registry
  annotations:
    dataregistry.opendatahub.io/enabled: "true"
spec:
  feastProject: data_registry
  services:
    registry:
      local:
        persistence:
          store:
            type: sql
            secretRef:
              name: registry-db-secret
```

See [Guide 1 — Basic](01-basic.md) and [Guide 5 — Security](05-security.md)
for the full set of configuration options.
{% endhint %}

Two prerequisites must be met before the annotation takes effect:

1. **Namespace label** — the namespace must carry `opendatahub.io/data-registry=true`.
   This label is applied by the platform operator (ODH/RHOAI module reconciliation), not by
   end users.
2. **Singleton** — only one annotated FeatureStore CR is allowed cluster-wide. If a second
   CR carries the annotation, it is rejected with `DataRegistryReady=False`. The oldest CR
   (by `metadata.creationTimestamp`) wins.

### Annotation strictness

Only the exact string `"true"` activates data-registry mode. Values like `"True"`, `"yes"`,
or `"1"` are logged as warnings and ignored — the CR reconciles in standard Feast mode.

---

## What gets deployed

When enabled, the operator creates the following resources:

| Resource | Name pattern | Scope |
|----------|-------------|-------|
| Deployment | `feast-{cr-name}-data-registry` | Namespaced |
| Service | `feast-{cr-name}-data-registry` (ports: 443→8443, 8000→8000) | Namespaced |
| ConfigMap (auth) | `feast-{cr-name}-data-registry-auth` | Namespaced |
| ConfigMap (CA bundle) | `feast-{cr-name}-data-registry-cabundle` | Namespaced |
| Route | `feast-{cr-name}-data-registry` (ReEncrypt) | Namespaced |
| ServiceMonitor | `feast-{cr-name}-data-registry` | Namespaced |
| ClusterRole (viewer) | `feast-data-registry-viewer` | Cluster |
| ClusterRole (editor) | `feast-data-registry-editor` | Cluster |
| ClusterRole (admin) | `feast-data-registry-admin` | Cluster |
| ClusterRoleBinding | `feast-{cr-name}-data-registry-auth-delegator` | Cluster |

All namespaced resources are owned by the FeatureStore CR and garbage-collected on deletion.
Cluster-scoped resources (ClusterRoles, ClusterRoleBinding) are managed via a finalizer —
they are cleaned up when the CR is deleted or the annotation is removed.

---

## Two-container pod architecture

```
kube-rbac-proxy :8443 (HTTPS)  ──HTTP 127.0.0.1:6572──►  feast-server
     │                                                         │
     │ TokenReview + SAR                           SAR (/projects)
     │ on "registries" resource                   per-namespace filtering
     │                                                         │
     └─── :8000 /metrics (Prometheus, direct scrape) ──────────┘
```

- **kube-rbac-proxy** terminates TLS and enforces per-tenant authorization via
  SubjectAccessReview on `dataregistry.opendatahub.io/registries`. The SAR **namespace**
  is the Feast project (Kubernetes namespace) from `/v1/{project}/...` path capture or from
  `?project=` on legacy registry REST — not the data-registry install namespace.
- **feast-server** runs `feast serve_registry --rest-api` with `DATA_REGISTRY_ENABLED=true`.
  It performs server-side SubjectAccessReview (SAR) for `/projects`, `/v1/projects`, and
  `/search` (listed in `--ignore-paths` because they span multiple tenants). Catalog
  `GET /v1/{project}/search` is tenant-scoped and goes through the proxy SAR, not
  ignore-paths. Iceberg `HEAD` is authorized as Kubernetes `get`.

### Resource limits

| Container | CPU request | CPU limit | Memory request | Memory limit |
|-----------|-------------|-----------|----------------|-------------|
| feast-server | 100m | 500m | 256Mi | 512Mi |
| kube-rbac-proxy | 50m | 100m | 128Mi | 256Mi |

---

## RBAC — aggregated ClusterRoles

The operator creates three ClusterRoles with standard Kubernetes RBAC aggregation labels:

| ClusterRole | Aggregates into | Verbs |
|-------------|----------------|-------|
| `feast-data-registry-viewer` | view, edit, admin, cluster-reader | get, list, watch |
| `feast-data-registry-editor` | edit, admin | + create, update, patch, delete |
| `feast-data-registry-admin` | admin | editor verbs + `connections:use` |

These operate on pseudo-resources in the `dataregistry.opendatahub.io` API group (registries,
namespaces, tables, volumes, generic-tables). Users with the standard OpenShift `view` role
automatically gain data-registry viewer access via aggregation.

---

## TLS architecture

```
Client ──TLS (router cert)──► OCP Router ──TLS (service-ca)──► kube-rbac-proxy ──HTTP──► feast-server
```

TLS between the Router and the proxy is auto-provisioned via OpenShift's `serving-cert-secret-name`
annotation. No manual certificate management is required.

---

## Observability

Data Registry monitoring follows the same model as the [online feature server](03-serving-and-observability.md)
and the [Feast feature server monitoring guide](https://feast.dev/blog/feast-feature-server-monitoring/):

- The feast-server starts a Prometheus metrics endpoint on **`:8000/metrics`** when
  `DATA_REGISTRY_ENABLED=true` (automatic in Data Registry mode).
- The operator exposes port `8000` on the Service (named `metrics`) so Prometheus can scrape
  directly — **no bearer token required** (metrics bypass kube-rbac-proxy).
- When the `ServiceMonitor` CRD is available, the operator creates a **ServiceMonitor** targeting
  the `metrics` port at `/metrics` (same pattern as the online store).

The operator does **not** create `PrometheusRule` resources. Customers define their own alert
rules and tune thresholds for their environment. Example alerts you may want to author:

| Alert | Expression (example) | Severity |
|---|---|---|
| High 5xx rate | `sum(rate(feast_feature_server_request_total{status=~"5.."}[5m])) / sum(rate(feast_feature_server_request_total[5m])) > 0.05` | critical |
| High search latency | `histogram_quantile(0.95, sum(rate(feast_feature_server_request_latency_seconds_bucket{endpoint="/search"}[5m])) by (le)) > 0.5` | warning |

See [Guide 3 — Serving & Observability](03-serving-and-observability.md) for standard Feast metrics
and Grafana dashboard patterns.

---

## Mode transition — important warnings

{% hint style="warning" %}
**Enabling data-registry mode on an existing FeatureStore CR cleans up standard-mode
resources** (Deployment, Services, HPAs) to avoid orphaned objects. This is by design — the
data-registry CR runs a different workload.

Before annotating an existing CR:
- **Back up any data** in online/offline stores if needed — the PVCs are preserved (the PVC
  safety guard blocks the transition if the CR owns existing PVCs).
- Ensure downstream consumers (notebooks, pipelines) are updated to point to a different
  FeatureStore CR for their feature retrieval needs.
- The recommended approach is to use a **dedicated FeatureStore CR in a labeled namespace**
  rather than converting an existing feature store CR.
{% endhint %}

---

## Disabling the Data Registry

Remove the annotation (or set it to any value other than `"true"`):

```sh
kubectl annotate featurestore data-registry dataregistry.opendatahub.io/enabled- -n <namespace>
```

The operator cleans up all data-registry resources (Deployment, Service, ConfigMaps, Route,
ClusterRoles, ClusterRoleBinding, ServiceMonitor) and removes the finalizer.

---

## See also

- [Guide 3 — Serving & Observability](03-serving-and-observability.md) — standard Feast
  metrics and observability
- [Guide 5 — Security](05-security.md) — Kubernetes RBAC and TLS configuration
