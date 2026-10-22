"""
Feast Auth-Enabled REST API Tests

This module contains integration tests for the auth-enabled Feast Feature Store
REST API. The feast-auth CR uses Kubernetes-based authentication (bearer tokens
from ServiceAccounts) matched against Roles via RoleBindings.

Tests validate RBAC enforcement: admin access, viewer (read-only) access,
unauthorized rejection, and no-token rejection.
"""

import os

import pytest

FEAST_AUTH_PROJECT = "feast_auth_project"


@pytest.mark.integration
@pytest.mark.skipif(
    not os.path.exists(os.path.expanduser("~/.kube/config")),
    reason="Kube config not available",
)
class TestFeastAuthAPI:
    """Test suite for auth-enabled Feast REST API endpoints."""

    # ----- Admin token: successful reads -----

    def test_auth_list_entities(self, feast_auth_client):
        """Admin token can list entities for the auth project."""
        response = feast_auth_client.get(f"/entities/?project={FEAST_AUTH_PROJECT}")
        assert response.status_code == 200
        data = response.json()

        assert "entities" in data
        entities = data["entities"]
        assert isinstance(entities, list)
        assert len(entities) > 0

    def test_auth_get_entity(self, feast_auth_client):
        """Admin token can retrieve a specific entity by name."""
        response = feast_auth_client.get(
            f"/entities/zipcode/?project={FEAST_AUTH_PROJECT}"
        )
        assert response.status_code == 200
        data = response.json()

        assert "spec" in data
        assert data["spec"]["name"] == "zipcode"

    def test_auth_list_feature_views(self, feast_auth_client):
        """Admin token can list feature views for the auth project."""
        response = feast_auth_client.get(
            f"/feature_views/?project={FEAST_AUTH_PROJECT}"
        )
        assert response.status_code == 200
        data = response.json()

        assert "featureViews" in data
        assert isinstance(data["featureViews"], list)
        assert len(data["featureViews"]) > 0

    def test_auth_list_features(self, feast_auth_client):
        """Admin token can list features with relationships included."""
        response = feast_auth_client.get(
            f"/features/?project={FEAST_AUTH_PROJECT}&include_relationships=true"
        )
        assert response.status_code == 200
        data = response.json()

        assert "features" in data
        features = data["features"]
        assert isinstance(features, list)
        assert len(features) > 0

    def test_auth_list_data_sources(self, feast_auth_client):
        """Admin token can list data sources for the auth project."""
        response = feast_auth_client.get(f"/data_sources/?project={FEAST_AUTH_PROJECT}")
        assert response.status_code == 200
        data = response.json()

        assert "dataSources" in data
        assert isinstance(data["dataSources"], list)
        assert len(data["dataSources"]) > 0

    def test_auth_list_saved_datasets(self, feast_auth_client):
        """Admin token can list saved datasets for the auth project."""
        response = feast_auth_client.get(
            f"/saved_datasets?project={FEAST_AUTH_PROJECT}&include_relationships=false"
        )
        assert response.status_code == 200
        data = response.json()

        assert "savedDatasets" in data
        saved_datasets = data["savedDatasets"]
        assert isinstance(saved_datasets, list)
        assert len(saved_datasets) > 0

    def test_auth_get_project(self, feast_auth_client):
        """Admin token can retrieve the auth project by name."""
        response = feast_auth_client.get(f"/projects/{FEAST_AUTH_PROJECT}")
        assert response.status_code == 200
        data = response.json()

        assert "spec" in data
        assert data["spec"]["name"] == FEAST_AUTH_PROJECT

    def test_auth_list_projects(self, feast_auth_client):
        """Admin token can list projects; data_registry must not be visible.

        The data_registry project is tagged as protected-project and must be
        isolated from the feast-auth CR's view of the shared Postgres registry.
        """
        response = feast_auth_client.get("/projects")
        assert response.status_code == 200
        data = response.json()

        assert "projects" in data
        projects = data["projects"]
        assert isinstance(projects, list)
        assert len(projects) > 0

        project_names = [p["spec"]["name"] for p in projects]
        assert "data_registry" not in project_names, (
            "data_registry must not be visible from the feast-auth CR "
            "(protected-project isolation)"
        )

    def test_auth_lineage_registry(self, feast_auth_client):
        """Admin token can retrieve registry lineage for the auth project."""
        response = feast_auth_client.get(
            f"/lineage/registry?project={FEAST_AUTH_PROJECT}"
        )
        assert response.status_code == 200
        data = response.json()

        assert "relationships" in data
        assert isinstance(data["relationships"], list)

    def test_auth_lineage_complete(self, feast_auth_client):
        """Admin token can retrieve complete lineage for the auth project."""
        response = feast_auth_client.get(
            f"/lineage/complete?project={FEAST_AUTH_PROJECT}"
        )
        assert response.status_code == 200
        data = response.json()

        assert "objects" in data

    def test_auth_list_permissions(self, feast_auth_client):
        """Admin token can list permissions for the auth project."""
        response = feast_auth_client.get(
            f"/permissions?project={FEAST_AUTH_PROJECT}&include_relationships=false"
        )
        assert response.status_code == 200
        data = response.json()

        assert "permissions" in data
        permissions = data["permissions"]
        assert isinstance(permissions, list)
        assert len(permissions) > 0

    # ----- No token: expect 401 -----

    def test_auth_no_token_rejected(self, feast_auth_no_token_client):
        """Requests without a bearer token must be rejected with 401."""
        response = feast_auth_no_token_client.get("/projects")
        assert response.status_code == 401

    # ----- Unauthorized SA token: expect 403 -----

    def test_auth_unauthorized_rejected(self, feast_auth_unauthorized_client):
        """An SA with no Feast roles must be rejected with 403."""
        response = feast_auth_unauthorized_client.get(
            f"/entities/?project={FEAST_AUTH_PROJECT}"
        )
        assert response.status_code == 403

    # ----- Viewer SA token: read allowed, write denied -----

    def test_auth_viewer_can_read(self, feast_auth_viewer_client):
        """Viewer SA (DESCRIBE permission) can read entities."""
        response = feast_auth_viewer_client.get(
            f"/entities/?project={FEAST_AUTH_PROJECT}"
        )
        assert response.status_code == 200

    def test_auth_viewer_cannot_write(self, feast_auth_viewer_client):
        """Viewer SA must be denied when attempting to create a permission (403)."""
        permission_body = {
            "name": "test_perm",
            "project": FEAST_AUTH_PROJECT,
            "types": ["ENTITY"],
            "name_patterns": ["*"],
            "actions": ["CREATE"],
            "policy": {
                "role_based_policy": {
                    "roles": ["test-role"],
                },
            },
        }
        response = feast_auth_viewer_client.post("/permissions", json=permission_body)
        assert response.status_code == 403
