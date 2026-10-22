# Copyright 2026 The Feast Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Tests for project-level label CRUD endpoints."""

import tempfile

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from feast.api.data_catalog.config import get_config_router
from feast.api.data_catalog.errors import register_error_handlers
from feast.api.data_catalog.generic_tables import get_generic_table_router
from feast.api.data_catalog.labels import get_label_router
from feast.api.data_catalog.namespaces import get_namespace_router
from feast.api.data_catalog.tables import get_table_router
from feast.api.data_catalog.volumes import get_volume_router
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig

NS = "demo-user-1"
COL = "underwriting"
AUTH = {"X-User": "test-user"}


@pytest.fixture
def sqlite_registry():
    _fd, registry_path = tempfile.mkstemp()
    registry = SqlRegistry(
        SqlRegistryConfig(
            registry_type="sql",
            path=f"sqlite:///{registry_path}",
            purge_feast_metadata=False,
        ),
        "scratch",
        None,
    )
    yield registry
    registry.teardown()


def _client(registry) -> TestClient:
    app = FastAPI()
    app.state.registry = registry
    register_error_handlers(app)
    app.include_router(get_config_router())
    app.include_router(get_namespace_router())
    app.include_router(get_table_router())
    app.include_router(get_volume_router())
    app.include_router(get_generic_table_router())
    app.include_router(get_label_router())
    return TestClient(app, raise_server_exceptions=False)


def _ensure_collection(client: TestClient, project: str = NS, collection: str = COL):
    client.post(
        f"/v1/{project}/namespaces",
        json={"namespace": [collection], "properties": {}},
    )


# ---------------------------------------------------------------------------
# listLabels
# ---------------------------------------------------------------------------


def test_list_labels_empty(sqlite_registry):
    client = _client(sqlite_registry)
    resp = client.get(f"/v1/{NS}/labels")
    assert resp.status_code == 200
    assert resp.json() == {"labels": []}


def test_list_labels_explicit_only(sqlite_registry):
    client = _client(sqlite_registry)
    client.post(f"/v1/{NS}/labels", json={"name": "pii"})
    client.post(f"/v1/{NS}/labels", json={"name": "finance"})
    resp = client.get(f"/v1/{NS}/labels")
    assert resp.status_code == 200
    assert resp.json()["labels"] == ["finance", "pii"]


def test_list_labels_discovered_from_volume(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "claims",
            "format": "documents",
            "storage_location": "s3://b/c/",
            "labels": ["uw", "claims"],
        },
        headers=AUTH,
    )
    resp = client.get(f"/v1/{NS}/labels")
    assert resp.status_code == 200
    assert resp.json()["labels"] == ["claims", "uw"]


def test_list_labels_discovered_from_generic_table(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": "scores", "format": "parquet", "labels": ["ml"]},
        headers=AUTH,
    )
    resp = client.get(f"/v1/{NS}/labels")
    assert resp.status_code == 200
    assert "ml" in resp.json()["labels"]


def test_list_labels_merges_explicit_and_discovered(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(f"/v1/{NS}/labels", json={"name": "pii"})
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "claims",
            "format": "documents",
            "storage_location": "s3://b/c/",
            "labels": ["uw"],
        },
        headers=AUTH,
    )
    resp = client.get(f"/v1/{NS}/labels")
    assert resp.status_code == 200
    assert resp.json()["labels"] == ["pii", "uw"]


def test_list_labels_no_duplicates(sqlite_registry):
    """Explicit label + same label on an asset = appears once."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(f"/v1/{NS}/labels", json={"name": "pii"})
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "claims",
            "format": "documents",
            "storage_location": "s3://b/c/",
            "labels": ["pii"],
        },
        headers=AUTH,
    )
    resp = client.get(f"/v1/{NS}/labels")
    assert resp.status_code == 200
    assert resp.json()["labels"] == ["pii"]


# ---------------------------------------------------------------------------
# createLabel
# ---------------------------------------------------------------------------


def test_create_label(sqlite_registry):
    client = _client(sqlite_registry)
    resp = client.post(f"/v1/{NS}/labels", json={"name": "pii"})
    assert resp.status_code == 201
    assert resp.json() == {"name": "pii"}


def test_create_label_duplicate_409(sqlite_registry):
    client = _client(sqlite_registry)
    assert client.post(f"/v1/{NS}/labels", json={"name": "pii"}).status_code == 201
    resp = client.post(f"/v1/{NS}/labels", json={"name": "pii"})
    assert resp.status_code == 409
    assert resp.json()["error"]["type"] == "AlreadyExistsException"


def test_create_label_discovered_on_asset_409(sqlite_registry):
    """POST a label that already exists on an asset → 409."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "claims",
            "format": "documents",
            "storage_location": "s3://b/c/",
            "labels": ["uw"],
        },
        headers=AUTH,
    )
    resp = client.post(f"/v1/{NS}/labels", json={"name": "uw"})
    assert resp.status_code == 409
    assert resp.json()["error"]["type"] == "AlreadyExistsException"


def test_create_label_empty_name_400(sqlite_registry):
    client = _client(sqlite_registry)
    resp = client.post(f"/v1/{NS}/labels", json={"name": "  "})
    assert resp.status_code == 400


def test_create_label_slash_in_name_400(sqlite_registry):
    """Labels with '/' are rejected because they break the DELETE path."""
    client = _client(sqlite_registry)
    resp = client.post(f"/v1/{NS}/labels", json={"name": "team/data"})
    assert resp.status_code == 400


def test_assign_label_with_slash_rejected(sqlite_registry):
    """Labels with '/' are rejected when assigned to assets too."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "v1",
            "format": "documents",
            "storage_location": "s3://b/c/",
            "labels": ["team/data"],
        },
        headers=AUTH,
    )
    assert resp.status_code == 400


def test_create_label_project_isolation(sqlite_registry):
    """Labels in different projects don't collide."""
    client = _client(sqlite_registry)
    assert client.post(f"/v1/{NS}/labels", json={"name": "pii"}).status_code == 201
    assert client.post("/v1/other-user/labels", json={"name": "pii"}).status_code == 201
    assert client.get(f"/v1/{NS}/labels").json()["labels"] == ["pii"]
    assert client.get("/v1/other-user/labels").json()["labels"] == ["pii"]


# ---------------------------------------------------------------------------
# deleteLabel
# ---------------------------------------------------------------------------


def test_delete_explicit_label(sqlite_registry):
    client = _client(sqlite_registry)
    client.post(f"/v1/{NS}/labels", json={"name": "pii"})
    resp = client.delete(f"/v1/{NS}/labels/pii")
    assert resp.status_code == 204
    assert client.get(f"/v1/{NS}/labels").json()["labels"] == []


def test_delete_label_not_found_404(sqlite_registry):
    client = _client(sqlite_registry)
    resp = client.delete(f"/v1/{NS}/labels/nonexistent")
    assert resp.status_code == 404
    assert resp.json()["error"]["type"] == "NoSuchLabelException"


def test_delete_discovered_label_cascades(sqlite_registry):
    """Delete a discovered-only label → removed from all assets."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "claims",
            "format": "documents",
            "storage_location": "s3://b/c/",
            "labels": ["uw", "pii"],
        },
        headers=AUTH,
    )
    resp = client.delete(f"/v1/{NS}/labels/pii")
    assert resp.status_code == 204
    labels_resp = client.get(f"/v1/{NS}/labels")
    assert labels_resp.json()["labels"] == ["uw"]
    vol = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/claims")
    assert vol.json()["labels"] == ["uw"]


def test_delete_explicit_label_cascades_to_assets(sqlite_registry):
    """Delete an explicit label → also removed from assets that have it."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(f"/v1/{NS}/labels", json={"name": "pii"})
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "claims",
            "format": "documents",
            "storage_location": "s3://b/c/",
            "labels": ["pii", "uw"],
        },
        headers=AUTH,
    )
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": "scores", "format": "parquet", "labels": ["pii"]},
        headers=AUTH,
    )
    resp = client.delete(f"/v1/{NS}/labels/pii")
    assert resp.status_code == 204
    labels_resp = client.get(f"/v1/{NS}/labels")
    assert labels_resp.json()["labels"] == ["uw"]
    vol = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/claims")
    assert vol.json()["labels"] == ["uw"]
    tbl = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables/scores")
    assert tbl.json()["labels"] is None or tbl.json()["labels"] == []


def test_delete_label_does_not_affect_other_projects(sqlite_registry):
    """Delete in one project does not cascade to another project's assets."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _ensure_collection(client, project="other-user", collection=COL)
    client.post(f"/v1/{NS}/labels", json={"name": "pii"})
    client.post("/v1/other-user/labels", json={"name": "pii"})
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "claims",
            "format": "documents",
            "storage_location": "s3://b/c/",
            "labels": ["pii"],
        },
        headers=AUTH,
    )
    client.post(
        f"/v1/other-user/namespaces/{COL}/volumes",
        json={
            "name": "claims",
            "format": "documents",
            "storage_location": "s3://b/d/",
            "labels": ["pii"],
        },
        headers=AUTH,
    )
    client.delete(f"/v1/{NS}/labels/pii")
    assert client.get(f"/v1/{NS}/labels").json()["labels"] == []
    assert client.get("/v1/other-user/labels").json()["labels"] == ["pii"]
    vol = client.get(f"/v1/other-user/namespaces/{COL}/volumes/claims")
    assert "pii" in vol.json()["labels"]


# ---------------------------------------------------------------------------
# Config endpoint advertises label routes
# ---------------------------------------------------------------------------


def test_config_advertises_label_endpoints(sqlite_registry):
    client = _client(sqlite_registry)
    resp = client.get("/v1/config")
    assert resp.status_code == 200
    endpoints = resp.json()["endpoints"]
    assert "GET /v1/{prefix}/labels" in endpoints
    assert "POST /v1/{prefix}/labels" in endpoints
    assert "DELETE /v1/{prefix}/labels/{label}" in endpoints
