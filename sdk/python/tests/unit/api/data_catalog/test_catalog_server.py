# Copyright 2026 The Feast Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import pytest
from fastapi.testclient import TestClient

from feast import FeatureStore
from feast.api.registry.rest.rest_registry_server import RestRegistryServer
from feast.repo_config import RepoConfig


@pytest.fixture
def sql_repo_config(tmp_path):
    registry_path = tmp_path / "registry.db"
    return RepoConfig.model_validate(
        {
            "registry": {
                "registry_type": "sql",
                "path": f"sqlite:///{registry_path}",
            },
            "project": "demo_project",
            "provider": "local",
            "offline_store": {"type": "file"},
            "online_store": {"type": "sqlite", "path": ":memory:"},
        }
    )


def test_s1_flag_off_config_is_404(sql_repo_config, monkeypatch):
    monkeypatch.delenv("DATA_REGISTRY_ENABLED", raising=False)
    store = FeatureStore(config=sql_repo_config)
    client = TestClient(RestRegistryServer(store).app, raise_server_exceptions=False)
    response = client.get("/v1/config")
    assert response.status_code == 404
    entities = client.get("/entities?project=demo_project")
    assert entities.status_code == 200


def test_s2_flag_on_config_200(sql_repo_config, monkeypatch):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    store = FeatureStore(config=sql_repo_config)
    client = TestClient(RestRegistryServer(store).app, raise_server_exceptions=False)
    response = client.get("/v1/config")
    assert response.status_code == 200
    assert "defaults" in response.json()
    namespaces = client.get("/v1/demo-user-1/namespaces")
    assert namespaces.status_code == 200
    assert ["default"] in namespaces.json()["namespaces"]


def test_s3_flag_on_table_create(sql_repo_config, monkeypatch):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    store = FeatureStore(config=sql_repo_config)
    client = TestClient(RestRegistryServer(store).app, raise_server_exceptions=False)
    created = client.post("/v1/demo-user-1/namespaces/default/tables")
    assert created.status_code == 501, created.text
    assert created.json()["error"]["type"] == "NotImplementedException"


def test_s5_flag_on_feast_validation_body_unchanged(sql_repo_config, monkeypatch):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    store = FeatureStore(config=sql_repo_config)
    client = TestClient(RestRegistryServer(store).app, raise_server_exceptions=False)
    response = client.post("/entities", json={})
    assert response.status_code == 422
    body = response.json()
    assert "detail" in body or body.get("error_type") == "RequestValidationError"
    assert "error" not in body or "message" not in body.get("error", {})


def test_s6_flag_on_catalog_missing_body_is_iceberg_400(sql_repo_config, monkeypatch):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    store = FeatureStore(config=sql_repo_config)
    client = TestClient(RestRegistryServer(store).app, raise_server_exceptions=False)
    response = client.post("/v1/demo-user-1/namespaces", json={})
    assert response.status_code == 400
    body = response.json()
    assert "detail" not in body
    assert body["error"]["type"] == "BadRequestException"


def test_s7_flag_on_volume_and_search(sql_repo_config, monkeypatch):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    store = FeatureStore(config=sql_repo_config)
    client = TestClient(RestRegistryServer(store).app, raise_server_exceptions=False)
    assert client.get("/v1/projects").status_code == 404
    created = client.post(
        "/v1/demo-user-1/namespaces/default/volumes",
        json={
            "name": "docs",
            "format": "documents",
            "storage_location": "s3://bucket/docs/",
        },
        headers={"X-User": "test-user"},
    )
    assert created.status_code == 200, created.text
    assert created.json()["asset_type"] == "volume"
    iceberg = client.post("/v1/demo-user-1/namespaces/default/tables")
    assert iceberg.status_code == 501
    parquet = client.post(
        "/v1/demo-user-1/namespaces/default/generic-tables",
        json={"name": "events", "format": "parquet"},
        headers={"X-User": "test-user"},
    )
    assert parquet.status_code == 201, parquet.text
    search = client.get("/v1/demo-user-1/search")
    assert search.status_code == 200, search.text
    assert search.json()["query"] == ""
    assert "pagination" in search.json()


def test_connection_ref_openapi_schema(sql_repo_config, monkeypatch):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    store = FeatureStore(config=sql_repo_config)
    client = TestClient(RestRegistryServer(store).app, raise_server_exceptions=False)

    openapi = client.get("/openapi.json").json()
    schemas = openapi["components"]["schemas"]
    dch = schemas["DchConnectionRef"]["properties"]
    secret = schemas["RhaiConnectionRef"]["properties"]

    assert openapi["paths"]["/v1/config"]["get"]["operationId"] == "getBootstrapConfig"
    assert (
        openapi["paths"]["/v1/{project}/config"]["get"]["operationId"]
        == "getProjectConfig"
    )
    assert (
        openapi["paths"]["/v1/{project}/search"]["get"]["operationId"]
        == "searchDataRegistry"
    )
    assert "Catalog stores" not in schemas["DchConnectionRef"]["description"]
    assert "Catalog stores" not in schemas["RhaiConnectionRef"]["description"]
    assert dch["type"]["const"] == "dch"
    assert secret["type"]["const"] == "secret"
    for properties in (dch, secret):
        assert properties["name"]["readOnly"] is True
        assert properties["connectionType"]["readOnly"] is True
