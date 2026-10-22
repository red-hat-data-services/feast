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

import ast
from pathlib import Path

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from pydantic import BaseModel

import feast.api.data_catalog as data_catalog_pkg
from feast.api.data_catalog.config import get_config_router
from feast.api.data_catalog.errors import (
    BadRequestException,
    IcebergRESTException,
    NamespaceAlreadyExistsException,
    NamespaceNotEmptyException,
    NoSuchNamespaceException,
    NoSuchTableException,
    NotImplementedException,
    ServiceFailureException,
    missing_required_fields,
    register_error_handlers,
)

CASES = [
    ("no-such-namespace", NoSuchNamespaceException, 404, "NoSuchNamespaceException"),
    ("no-such-table", NoSuchTableException, 404, "NoSuchTableException"),
    (
        "namespace-exists",
        NamespaceAlreadyExistsException,
        409,
        "AlreadyExistsException",
    ),
    (
        "namespace-not-empty",
        NamespaceNotEmptyException,
        409,
        "NamespaceNotEmptyException",
    ),
    ("bad-request", BadRequestException, 400, "BadRequestException"),
    ("not-implemented", NotImplementedException, 501, "NotImplementedException"),
    ("service-failure", ServiceFailureException, 500, "ServiceFailureException"),
]


class _CreateBody(BaseModel):
    name: str
    schema_fields: str


def _client() -> TestClient:
    app = FastAPI()
    register_error_handlers(app)
    app.include_router(get_config_router())

    @app.get("/v1/_test/raise/{kind}")
    def raise_kind(kind: str) -> dict:
        mapping = {slug: cls for slug, cls, *_ in CASES}
        raise mapping[kind](f"test {kind}")

    @app.post("/v1/_test/create")
    def create(body: _CreateBody) -> _CreateBody:
        return body

    return TestClient(app, raise_server_exceptions=False)


@pytest.mark.parametrize("kind,cls,status,error_type", CASES)
def test_exception_returns_iceberg_error_json(kind, cls, status, error_type):
    response = _client().get(f"/v1/_test/raise/{kind}")
    assert response.status_code == status
    body = response.json()
    assert set(body.keys()) == {"error"}
    error = body["error"]
    assert set(error.keys()) == {"message", "type", "code"}
    assert error["type"] == error_type
    assert error["code"] == status
    assert error["message"] == f"test {kind}"
    assert "detail" not in body
    assert cls.error_type == error_type
    assert issubclass(cls, IcebergRESTException)


def test_missing_body_is_iceberg_400_not_fastapi_detail():
    response = _client().post("/v1/_test/create", json={})
    assert response.status_code == 400
    body = response.json()
    assert "detail" not in body
    error = body["error"]
    assert error["type"] == "BadRequestException"
    assert error["code"] == 400
    assert "Missing required fields:" in error["message"]
    assert "name" in error["message"]
    assert "schema_fields" in error["message"]


def test_missing_required_fields_helper():
    exc = missing_required_fields("name", "schema")
    assert exc.http_status == 400
    assert exc.to_payload() == {
        "error": {
            "message": "Missing required fields: name, schema",
            "type": "BadRequestException",
            "code": 400,
        }
    }


def test_data_catalog_package_does_not_mount_feast_server():
    catalog_dir = Path(data_catalog_pkg.__file__).resolve().parent
    forbidden = {
        "RestRegistryServer",
        "DATA_REGISTRY_ENABLED",
        "add_data_catalog_routes",
    }
    for path in catalog_dir.glob("*.py"):
        tree = ast.parse(path.read_text())
        for node in ast.walk(tree):
            if isinstance(node, ast.Name):
                assert node.id not in forbidden, f"{path.name} uses {node.id}"
            if isinstance(node, ast.ImportFrom) and node.module:
                assert "rest_registry_server" not in node.module
