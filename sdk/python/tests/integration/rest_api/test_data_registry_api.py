"""
Data Registry REST API Integration Tests

Validates the Data Registry service deployed on OpenShift with kube-rbac-proxy
sidecar for authentication. Tests cover namespace CRUD, volume CRUD, generic
table CRUD, search, conflict handling, and auth rejection scenarios.

The Data Registry is a Feast-based service whose REST API is scoped by
Kubernetes namespace ({ns} in URL paths), not by Feast project name. The
underlying Feast project is always 'data_registry', but the URL path segment
is the Kubernetes namespace where the CR is deployed.

Tests are numbered (test_dr_01_ ... test_dr_24_) to enforce execution order
because later tests depend on resources created by earlier ones.
"""

import os

import pytest

NAMESPACE = "test-ns-feast-rest"
DR_FEAST_PROJECT = "data_registry"

TEST_NS_NAME = "test-ns-integ"
TEST_VOLUME_NAME = "integ-test-volume"
TEST_TABLE_NAME = "integ-test-table"


@pytest.mark.integration
@pytest.mark.skipif(
    not os.path.exists(os.path.expanduser("~/.kube/config")),
    reason="Kube config not available",
)
class TestDataRegistryAPI:
    """Data Registry REST API integration tests.

    All tests share state through the live Data Registry service. Tests are
    ordered by name so that resource creation runs before reads, reads before
    conflict/error checks, and cleanup runs last.
    """

    # ------------------------------------------------------------------
    # 01-02: Health and config
    # ------------------------------------------------------------------

    def test_dr_01_health(self, data_registry_client):
        """GET /projects returns 200, confirming the service is reachable."""
        response = data_registry_client.get("/projects")
        assert response.status_code == 200

    def test_dr_02_config(self, data_registry_client):
        """GET /v1/config?warehouse=<ns> is Iceberg bootstrap and returns 200.

        kube-rbac-proxy SAR for /v1/config rewrites the tenant from
        ?warehouse=. Omitting it is 400 from the proxy, not a catalog 200.
        """
        response = data_registry_client.get(
            "/v1/config", params={"warehouse": NAMESPACE}
        )
        assert response.status_code == 200
        data = response.json()
        assert "overrides" in data
        assert "defaults" in data
        assert data.get("overrides", {}).get("prefix") == NAMESPACE

    # ------------------------------------------------------------------
    # 03-07: Namespace lifecycle
    # ------------------------------------------------------------------

    def test_dr_03_namespace_create(self, data_registry_client):
        """POST /v1/{ns}/namespaces creates a new namespace."""
        response = data_registry_client.post(
            f"/v1/{NAMESPACE}/namespaces",
            json={
                "namespace": [TEST_NS_NAME],
                "properties": {"description": "Integration test namespace"},
            },
        )
        assert response.status_code == 200
        data = response.json()
        assert data["namespace"] == [TEST_NS_NAME]
        assert data["properties"]["description"] == "Integration test namespace"

    def test_dr_04_namespace_list(self, data_registry_client):
        """GET /v1/{ns}/namespaces returns a list that includes test-ns-integ."""
        response = data_registry_client.get(f"/v1/{NAMESPACE}/namespaces")
        assert response.status_code == 200
        data = response.json()
        namespaces_flat = [ns[0] for ns in data["namespaces"]]
        assert TEST_NS_NAME in namespaces_flat

    def test_dr_05_namespace_get(self, data_registry_client):
        """GET /v1/{ns}/namespaces/test-ns-integ returns the namespace."""
        response = data_registry_client.get(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}"
        )
        assert response.status_code == 200
        data = response.json()
        assert data["namespace"] == [TEST_NS_NAME]

    def test_dr_06_namespace_head(self, data_registry_client):
        """HEAD /v1/{ns}/namespaces/test-ns-integ returns 204."""
        response = data_registry_client.head(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}"
        )
        assert response.status_code == 204

    def test_dr_07_namespace_update_properties(self, data_registry_client):
        """POST /v1/{ns}/namespaces/test-ns-integ/properties updates properties."""
        response = data_registry_client.post(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/properties",
            json={"updates": {"env": "ci"}, "removals": []},
        )
        assert response.status_code == 200
        data = response.json()
        assert "env" in data.get("updated", [])

    # ------------------------------------------------------------------
    # 08-11: Volume lifecycle
    # ------------------------------------------------------------------

    def test_dr_08_volume_create(self, data_registry_client):
        """POST .../volumes creates a volume asset."""
        response = data_registry_client.post(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/volumes",
            json={
                "name": TEST_VOLUME_NAME,
                "format": "documents",
                "storage_location": "s3://test-bucket/integ-volumes/",
                "properties": {"source": "integration-test"},
            },
        )
        assert response.status_code in (200, 201)
        data = response.json()
        assert data["name"] == TEST_VOLUME_NAME
        assert data["asset_type"] == "volume"

    def test_dr_09_volume_list(self, data_registry_client):
        """GET .../volumes returns a list including our test volume."""
        response = data_registry_client.get(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/volumes"
        )
        assert response.status_code == 200
        data = response.json()
        volume_names = [v["name"] for v in data["volumes"]]
        assert TEST_VOLUME_NAME in volume_names

    def test_dr_10_volume_get(self, data_registry_client):
        """GET .../volumes/{name} returns the volume details."""
        response = data_registry_client.get(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/volumes/{TEST_VOLUME_NAME}"
        )
        assert response.status_code == 200
        data = response.json()
        assert data["name"] == TEST_VOLUME_NAME
        assert data["asset_type"] == "volume"

    def test_dr_11_volume_head(self, data_registry_client):
        """HEAD .../volumes/{name} returns 204."""
        response = data_registry_client.head(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/volumes/{TEST_VOLUME_NAME}"
        )
        assert response.status_code == 204

    # ------------------------------------------------------------------
    # 12-14: Generic table lifecycle
    # ------------------------------------------------------------------

    def test_dr_12_generic_table_create(self, data_registry_client):
        """POST .../generic-tables creates a generic table asset."""
        response = data_registry_client.post(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/generic-tables",
            json={
                "name": TEST_TABLE_NAME,
                "format": "parquet",
                "storage_location": "s3://test-bucket/integ-tables/",
                "description": "Integration test table",
                "schema_fields": [
                    {"name": "id", "type": "int", "description": "Primary key"},
                    {"name": "value", "type": "string", "description": "Test value"},
                ],
                "properties": {"source": "integration-test"},
            },
        )
        assert response.status_code == 201
        data = response.json()
        assert data["name"] == TEST_TABLE_NAME
        assert data["asset_type"] == "table"

    def test_dr_13_generic_table_list(self, data_registry_client):
        """GET .../generic-tables returns a list including our test table."""
        response = data_registry_client.get(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/generic-tables"
        )
        assert response.status_code == 200
        data = response.json()
        table_names = [t["name"] for t in data["assets"]]
        assert TEST_TABLE_NAME in table_names

    def test_dr_14_generic_table_get(self, data_registry_client):
        """GET .../generic-tables/{name} returns the table details."""
        response = data_registry_client.get(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/generic-tables/{TEST_TABLE_NAME}"
        )
        assert response.status_code == 200
        data = response.json()
        assert data["name"] == TEST_TABLE_NAME
        assert data["asset_type"] == "table"

    # ------------------------------------------------------------------
    # 15-16: Search
    # ------------------------------------------------------------------

    def test_dr_15_search(self, data_registry_client):
        """GET /v1/{ns}/search with query=test returns 200 with results array."""
        response = data_registry_client.get(
            f"/v1/{NAMESPACE}/search", params={"query": "test"}
        )
        assert response.status_code == 200
        data = response.json()
        assert "results" in data
        assert isinstance(data["results"], list)

    def test_dr_16_search_empty_query(self, data_registry_client):
        """GET /v1/{ns}/search with empty query returns results."""
        response = data_registry_client.get(
            f"/v1/{NAMESPACE}/search", params={"query": ""}
        )
        assert response.status_code == 200
        data = response.json()
        assert "results" in data
        assert isinstance(data["results"], list)

    # ------------------------------------------------------------------
    # 17-18: Conflict / error scenarios
    # ------------------------------------------------------------------

    def test_dr_17_conflict_duplicate_namespace(self, data_registry_client):
        """POST /v1/{ns}/namespaces with the same namespace again returns 409."""
        response = data_registry_client.post(
            f"/v1/{NAMESPACE}/namespaces",
            json={
                "namespace": [TEST_NS_NAME],
                "properties": {"description": "Duplicate attempt"},
            },
        )
        assert response.status_code == 409

    def test_dr_18_delete_non_empty_namespace(self, data_registry_client):
        """DELETE namespace while tables/volumes exist returns 409."""
        response = data_registry_client.delete(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}"
        )
        assert response.status_code == 409

    # ------------------------------------------------------------------
    # 19-20: Auth rejection
    # ------------------------------------------------------------------

    def test_dr_19_auth_no_token(self, data_registry_no_token_client):
        """GET /v1/{ns}/namespaces without a token is rejected with 401 by kube-rbac-proxy."""
        response = data_registry_no_token_client.get(f"/v1/{NAMESPACE}/namespaces")
        assert response.status_code == 401

    def test_dr_20_auth_unauthorized(self, data_registry_unauthorized_client):
        """GET /v1/{ns}/namespaces with a token lacking SAR bindings returns 403."""
        response = data_registry_unauthorized_client.get(f"/v1/{NAMESPACE}/namespaces")
        assert response.status_code == 403

    # ------------------------------------------------------------------
    # 21-24: Cleanup (must run last)
    # ------------------------------------------------------------------

    def test_dr_21_cleanup_volume(self, data_registry_client):
        """DELETE volume returns 204."""
        response = data_registry_client.delete(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/volumes/{TEST_VOLUME_NAME}"
        )
        assert response.status_code == 204

    def test_dr_22_cleanup_generic_table(self, data_registry_client):
        """DELETE generic table returns 204."""
        response = data_registry_client.delete(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}/generic-tables/{TEST_TABLE_NAME}"
        )
        assert response.status_code == 204

    def test_dr_23_cleanup_namespace(self, data_registry_client):
        """DELETE namespace returns 204 after assets are removed."""
        response = data_registry_client.delete(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}"
        )
        assert response.status_code == 204

    def test_dr_24_cleanup_verify(self, data_registry_client):
        """GET namespace returns 404 after deletion."""
        response = data_registry_client.get(
            f"/v1/{NAMESPACE}/namespaces/{TEST_NS_NAME}"
        )
        assert response.status_code == 404
