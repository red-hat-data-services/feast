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

import "strings"

// Template for SAR namespace from catalog /v1/{project}/... path captures.
// Requires opendatahub-io/kube-rbac-proxy v3.6.0-ea.2+ (named path captures, PR #28).
const dataRegistryAuthProjectFromPath = `{{ index .PathParams "project" }}`

// Template for SAR namespace from legacy ?project= or bootstrap ?warehouse= query params.
const dataRegistryAuthProjectFromQuery = `{{ .Value }}`

// Mutating catalog methods. HEAD is a separate mapping that SAR as get
// (opendatahub-io/kube-rbac-proxy HTTPToKubeVerb maps HEAD → "head", which
// is not a Kubernetes RBAC verb).
const dataRegistryAuthMutatingMethods = "[get, post, put, patch, delete]"

var dataRegistryAuthCatalogEndpointPaths = []string{
	"/v1/{project}/config",
	"/v1/{project}/namespaces",
	"/v1/{project}/namespaces/{collection}",
	"/v1/{project}/namespaces/{collection}/properties",
	"/v1/{project}/namespaces/{collection}/tables",
	"/v1/{project}/namespaces/{collection}/tables/{table}",
	"/v1/{project}/tables/rename",
	"/v1/{project}/namespaces/{collection}/volumes",
	"/v1/{project}/namespaces/{collection}/volumes/{volume}",
	"/v1/{project}/namespaces/{collection}/generic-tables",
	"/v1/{project}/namespaces/{collection}/generic-tables/{table}",
	"/v1/{project}/labels",
	"/v1/{project}/labels/{label}",
	// Iceberg catalog search is tenant-scoped; proxy SAR uses {project}.
	// Distinct from Feast registry GET /search (ignore-paths, server-side SAR).
	"/v1/{project}/search",
}

var dataRegistryAuthLegacyQueryPaths = []string{
	"/entities",
	"/entities/*",
	"/feature_views",
	"/feature_views/*",
	"/feature_services",
	"/feature_services/*",
	"/data_sources",
	"/data_sources/*",
	"/saved_datasets",
	"/saved_datasets/*",
	"/saved_datasets/data/*",
	"/permissions",
	"/permissions/*",
	"/features",
	"/features/*",
	"/features/*/*",
	"/labels",
	"/labels/*",
	"/label_views",
	"/label_views/*",
}

// buildDataRegistryAuthYaml generates kube-rbac-proxy auth.yaml with per-tenant
// SubjectAccessReview rules only. Unmatched paths are denied (no Format1 fallback
// on the data-registry install namespace).
//
// Tenant = Feast project = Kubernetes namespace, taken from:
//   - URL path {project} on catalog /v1/{project}/... routes
//   - ?project= on legacy Feast registry REST routes
//   - ?warehouse= on GET /v1/config bootstrap
//
// /projects, /v1/projects, /api/v1/projects, /search, and /api/v1/search are
// listed in --ignore-paths; the feast-server performs server-side SAR there.
// Catalog GET /v1/{project}/search is NOT ignored: the proxy SARs that path.
func (feast *FeastServices) buildDataRegistryAuthYaml() string {
	apiGroup := dataRegistryAPIGroup

	var b strings.Builder
	b.WriteString("authorization:\n")
	b.WriteString("  endpoints:\n")

	writeDataRegistryAuthCatalogEndpoints(&b, apiGroup)
	writeDataRegistryAuthV1ConfigBootstrap(&b, apiGroup)
	writeDataRegistryAuthLegacyQueryEndpoints(&b, apiGroup)

	return b.String()
}

func writeDataRegistryAuthCatalogEndpoints(b *strings.Builder, apiGroup string) {
	for _, path := range dataRegistryAuthCatalogEndpointPaths {
		writeDataRegistryAuthPathProjectEndpoint(b, path, apiGroup)
	}
}

func writeDataRegistryAuthV1ConfigBootstrap(b *strings.Builder, apiGroup string) {
	// Iceberg bootstrap. warehouse is required by the proxy rewrite; omit it
	// and kube-rbac-proxy returns 400. HEAD and GET both SAR as get.
	b.WriteString("    - path: /v1/config\n")
	b.WriteString("      mappings:\n")
	writeDataRegistryAuthQueryRewriteMapping(b, apiGroup, "[get, head]", "warehouse", "get")
}

func writeDataRegistryAuthLegacyQueryEndpoints(b *strings.Builder, apiGroup string) {
	for _, path := range dataRegistryAuthLegacyQueryPaths {
		writeDataRegistryAuthQueryProjectEndpoint(b, path, apiGroup)
		writeDataRegistryAuthQueryProjectEndpoint(b, "/api/v1"+path, apiGroup)
	}
}

func writeDataRegistryAuthPathProjectEndpoint(b *strings.Builder, path, apiGroup string) {
	b.WriteString("    - path: " + path + "\n")
	b.WriteString("      mappings:\n")
	writeDataRegistryAuthPathMapping(b, apiGroup, "[head]", "get")
	writeDataRegistryAuthPathMapping(b, apiGroup, dataRegistryAuthMutatingMethods, "")
}

func writeDataRegistryAuthQueryProjectEndpoint(b *strings.Builder, path, apiGroup string) {
	b.WriteString("    - path: " + path + "\n")
	b.WriteString("      mappings:\n")
	writeDataRegistryAuthQueryRewriteMapping(b, apiGroup, "[head]", "project", "get")
	writeDataRegistryAuthQueryRewriteMapping(b, apiGroup, dataRegistryAuthMutatingMethods, "project", "")
}

func writeDataRegistryAuthPathMapping(b *strings.Builder, apiGroup, methods, verb string) {
	b.WriteString("        - methods: " + methods + "\n")
	b.WriteString("          resources:\n")
	b.WriteString("            - resourceAttributes:\n")
	// Single-quoted so the Go-template inner "project" does not break YAML.
	b.WriteString("                namespace: '" + dataRegistryAuthProjectFromPath + "'\n")
	b.WriteString("                apiGroup: " + apiGroup + "\n")
	b.WriteString("                resource: registries\n")
	if verb != "" {
		b.WriteString("                verb: " + verb + "\n")
	}
}

func writeDataRegistryAuthQueryRewriteMapping(b *strings.Builder, apiGroup, methods, queryParam, verb string) {
	b.WriteString("        - methods: " + methods + "\n")
	b.WriteString("          resources:\n")
	b.WriteString("            - rewrites:\n")
	b.WriteString("                byQueryParameter:\n")
	b.WriteString("                  name: " + queryParam + "\n")
	b.WriteString("              resourceAttributes:\n")
	b.WriteString("                namespace: \"" + dataRegistryAuthProjectFromQuery + "\"\n")
	b.WriteString("                apiGroup: " + apiGroup + "\n")
	b.WriteString("                resource: registries\n")
	if verb != "" {
		b.WriteString("                verb: " + verb + "\n")
	}
}
