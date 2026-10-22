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

from fastapi import FastAPI
from fastapi.testclient import TestClient

from feast.api.data_catalog.config import CATALOG_CONFIG_ENDPOINTS, get_config_router
from feast.api.data_catalog.errors import register_error_handlers


def _client() -> TestClient:
    app = FastAPI()
    register_error_handlers(app)
    app.include_router(get_config_router())
    return TestClient(app, raise_server_exceptions=False)


def test_get_v1_config_shape():
    response = _client().get("/v1/config")
    assert response.status_code == 200
    body = response.json()
    assert set(body.keys()) == {"defaults", "overrides", "endpoints"}
    assert body["defaults"] == {}
    assert body["overrides"] == {}
    assert body["endpoints"] == CATALOG_CONFIG_ENDPOINTS
    assert "catalog" not in body
    assert "version" not in body
    assert "featureFlags" not in body
    assert "feature_flags" not in body


def test_get_v1_config_warehouse_sets_prefix():
    response = _client().get("/v1/config", params={"warehouse": "demo-user-1"})
    assert response.status_code == 200
    assert response.json()["overrides"] == {"prefix": "demo-user-1"}


def test_get_v1_project_config_sets_prefix():
    response = _client().get("/v1/demo-user-1/config")
    assert response.status_code == 200
    body = response.json()
    assert body["overrides"] == {"prefix": "demo-user-1"}
    assert body["endpoints"] == CATALOG_CONFIG_ENDPOINTS


def test_config_endpoints_are_iceberg_prefix_shaped():
    _UNPREFIXED = {"GET /v1/config"}
    for signature in CATALOG_CONFIG_ENDPOINTS:
        assert "{prefix}" in signature or signature in _UNPREFIXED
        assert "{project}" not in signature


def test_config_endpoints_include_namespace_crud():
    assert "GET /v1/{prefix}/namespaces" in CATALOG_CONFIG_ENDPOINTS
    assert (
        "POST /v1/{prefix}/namespaces/{namespace}/properties"
        in CATALOG_CONFIG_ENDPOINTS
    )
    assert "GET /v1/{prefix}/namespaces/{namespace}/tables" in CATALOG_CONFIG_ENDPOINTS
    assert (
        "GET /v1/{prefix}/namespaces/{namespace}/tables/{table}"
        in CATALOG_CONFIG_ENDPOINTS
    )
    assert (
        "POST /v1/{prefix}/namespaces/{namespace}/tables"
        not in CATALOG_CONFIG_ENDPOINTS
    )
    assert "POST /v1/{prefix}/tables/rename" not in CATALOG_CONFIG_ENDPOINTS
    assert "GET /v1/{prefix}/namespaces/{namespace}/volumes" in CATALOG_CONFIG_ENDPOINTS
    assert (
        "POST /v1/{prefix}/namespaces/{namespace}/generic-tables"
        in CATALOG_CONFIG_ENDPOINTS
    )
    assert "GET /v1/projects" not in CATALOG_CONFIG_ENDPOINTS
    assert (
        "PATCH /v1/{prefix}/namespaces/{namespace}/volumes/{volume}"
        in CATALOG_CONFIG_ENDPOINTS
    )
    assert "GET /v1/{prefix}/search" in CATALOG_CONFIG_ENDPOINTS
    assert "GET /v1/{prefix}/labels" in CATALOG_CONFIG_ENDPOINTS
    assert "POST /v1/{prefix}/labels" in CATALOG_CONFIG_ENDPOINTS
    assert "DELETE /v1/{prefix}/labels/{label}" in CATALOG_CONFIG_ENDPOINTS


def test_empty_warehouse_does_not_set_prefix():
    response = _client().get("/v1/config", params={"warehouse": ""})
    assert response.status_code == 200
    assert response.json()["overrides"] == {}


def _assert_iceberg_400(response) -> None:
    assert response.status_code == 400
    body = response.json()
    assert "detail" not in body
    error = body["error"]
    assert error["type"] == "BadRequestException"
    assert error["code"] == 400
    assert "lowercase" in error["message"].lower()


def test_warehouse_mixed_case_is_400_not_prefix():
    response = _client().get("/v1/config", params={"warehouse": "Demo-User"})
    _assert_iceberg_400(response)


def test_path_project_mixed_case_is_400_not_prefix():
    response = _client().get("/v1/Demo-User/config")
    _assert_iceberg_400(response)


def test_get_v1_projects_route_removed():
    response = _client().get("/v1/projects")
    assert response.status_code == 404
