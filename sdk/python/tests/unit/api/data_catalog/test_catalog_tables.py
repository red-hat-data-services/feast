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

import tempfile

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from feast.api.data_catalog.catalog_utils import (
    CATALOG_PROJECT,
    DEFAULT_COLLECTION,
    scoped_name,
)
from feast.api.data_catalog.config import CATALOG_CONFIG_ENDPOINTS, get_config_router
from feast.api.data_catalog.errors import register_error_handlers
from feast.api.data_catalog.namespaces import get_namespace_router
from feast.api.data_catalog.tables import get_table_router
from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig
from feast.saved_dataset import SavedDataset

NS = "demo-user-1"
NS2 = "demo-user-2"
COL = "underwriting"
TABLE = "events"


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
    return TestClient(app, raise_server_exceptions=False)


def _ensure_collection(client: TestClient, project: str = NS, collection: str = COL):
    client.post(
        f"/v1/{project}/namespaces",
        json={"namespace": [collection], "properties": {}},
    )


def _seed_iceberg_table(
    registry,
    *,
    project: str = NS,
    collection: str = COL,
    name: str = TABLE,
    extra_tags: dict | None = None,
) -> SavedDataset:
    tags = {
        "_catalog_managed": "true",
        "asset_type": "table",
        "format": "iceberg",
        "owner": "uw",
    }
    if extra_tags:
        tags.update(extra_tags)
    dataset = SavedDataset(
        name=scoped_name(project, collection, name),
        features=["fv:feature"],
        join_keys=["entity_id"],
        storage=SavedDatasetFileStorage(path="s3://bucket/events/"),
        namespace=project,
        collection=collection,
        tags=tags,
    )
    registry.apply_saved_dataset(dataset, CATALOG_PROJECT)
    return dataset


def _assert_501(response) -> None:
    assert response.status_code == 501, response.text
    assert response.json()["error"]["type"] == "NotImplementedException"
    assert "feast://" not in (response.text or "")
    if response.headers.get("content-type", "").startswith("application/json"):
        body = response.json()
        if isinstance(body, dict):
            assert "metadata-location" not in body


def test_t1_list_empty_default(sqlite_registry):
    response = _client(sqlite_registry).get(
        f"/v1/{NS}/namespaces/{DEFAULT_COLLECTION}/tables"
    )
    assert response.status_code == 200
    assert response.json() == {"identifiers": []}


def test_t2_list_missing_collection_404(sqlite_registry):
    response = _client(sqlite_registry).get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert response.status_code == 404
    assert response.json()["error"]["type"] == "NoSuchNamespaceException"


def test_t3_list_head_after_seed(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _assert_501(client.post(f"/v1/{NS}/namespaces/{COL}/tables"))
    _seed_iceberg_table(sqlite_registry)
    listed = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert listed.status_code == 200
    assert listed.json()["identifiers"] == [{"namespace": [COL], "name": TABLE}]
    loaded = client.get(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}")
    assert loaded.status_code == 200
    assert loaded.json()["config"] == {}
    assert loaded.json()["metadata"]["location"] == "s3://bucket/events/"
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}").status_code == 204


def test_t4_http_create_does_not_insert(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _assert_501(client.post(f"/v1/{NS}/namespaces/{COL}/tables"))
    assert client.get(f"/v1/{NS}/namespaces/{COL}/tables").json() == {"identifiers": []}


def test_t5_seeded_same_name_other_project_ok(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _ensure_collection(client, NS2, COL)
    _seed_iceberg_table(sqlite_registry, project=NS)
    _seed_iceberg_table(sqlite_registry, project=NS2)
    first = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    second = client.get(f"/v1/{NS2}/namespaces/{COL}/tables")
    assert first.json()["identifiers"] == [{"namespace": [COL], "name": TABLE}]
    assert second.json()["identifiers"] == [{"namespace": [COL], "name": TABLE}]
    first_row = sqlite_registry.get_saved_dataset(
        scoped_name(NS, COL, TABLE), CATALOG_PROJECT, allow_cache=False
    )
    second_row = sqlite_registry.get_saved_dataset(
        scoped_name(NS2, COL, TABLE), CATALOG_PROJECT, allow_cache=False
    )
    assert first_row.name != second_row.name


def test_t6_create_missing_collection_is_501(sqlite_registry):
    _assert_501(_client(sqlite_registry).post(f"/v1/{NS}/namespaces/{COL}/tables"))


def test_load_table_returns_metadata_and_empty_config(sqlite_registry):
    from feast.saved_dataset import SavedDatasetColumn

    client = _client(sqlite_registry)
    _ensure_collection(client)
    _seed_iceberg_table(
        sqlite_registry,
        extra_tags={
            "uuid": "aaaa-bbbb-cccc",
            "_connection_ref": '{"type":"secret","secret_name":"my-s3"}',  # pragma: allowlist secret
        },
    )
    stored = sqlite_registry.get_saved_dataset(
        scoped_name(NS, COL, TABLE), CATALOG_PROJECT, allow_cache=False
    )
    stored.columns = [
        SavedDatasetColumn(name="id", type="long", description="", nullable=False),
        SavedDatasetColumn(name="name", type="string", description="", nullable=True),
    ]
    sqlite_registry.apply_saved_dataset(stored, CATALOG_PROJECT)

    resp = client.get(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}")
    assert resp.status_code == 200
    body = resp.json()
    assert body["metadata-location"] == "s3://bucket/events/"
    assert body["config"] == {}

    meta = body["metadata"]
    assert meta["table-uuid"] == "aaaa-bbbb-cccc"
    assert meta["location"] == "s3://bucket/events/"
    assert meta["format-version"] == 2
    assert meta["last-updated-ms"] >= 0

    assert len(meta["schemas"]) == 1
    fields = meta["schemas"][0]["fields"]
    assert fields[0] == {"id": 1, "name": "id", "required": True, "type": "long"}
    assert fields[1] == {"id": 2, "name": "name", "required": False, "type": "string"}

    assert (
        meta["properties"]["connection_ref"]
        == '{"type":"secret","secret_name":"my-s3"}'  # pragma: allowlist secret
    )
    assert meta["properties"]["owner"] == "uw"
    assert "_connection_ref" not in meta["properties"]

    # Hardcoded Iceberg structural defaults (agreed with Mateusz)
    assert meta["current-schema-id"] == 0
    assert meta["last-sequence-number"] == 0
    assert meta["current-snapshot-id"] == -1
    assert meta["snapshots"] == []
    assert meta["last-partition-id"] == 999
    assert meta["partition-specs"] == [{"spec-id": 0, "fields": []}]
    assert meta["default-spec-id"] == 0
    assert meta["sort-orders"] == []
    assert meta["default-sort-order-id"] == 0
    assert meta["last-column-id"] == 2  # two columns seeded above
    assert meta["schemas"][0]["schema-id"] == 0
    assert meta["schemas"][0]["type"] == "struct"


def test_load_missing_table_is_404(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.get(f"/v1/{NS}/namespaces/{COL}/tables/nonexistent")
    assert resp.status_code == 404
    assert resp.json()["error"]["type"] == "NoSuchTableException"


def test_load_non_iceberg_format_is_404(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    sqlite_registry.apply_saved_dataset(
        SavedDataset(
            name=scoped_name(NS, COL, "csv-data"),
            features=["fv:feature"],
            join_keys=["entity_id"],
            storage=SavedDatasetFileStorage(path="s3://bucket/csv/"),
            namespace=NS,
            collection=COL,
            tags={
                "_catalog_managed": "true",
                "asset_type": "table",
                "format": "parquet",
            },
        ),
        CATALOG_PROJECT,
    )
    resp = client.get(f"/v1/{NS}/namespaces/{COL}/tables/csv-data")
    assert resp.status_code == 404


def test_t9_head_missing_404(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/missing").status_code == 404


def test_t10_delete_is_501_row_remains(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _seed_iceberg_table(sqlite_registry)
    _assert_501(client.delete(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}"))
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}").status_code == 204
    assert client.get(f"/v1/{NS}/namespaces/{COL}/tables").json()["identifiers"] == [
        {"namespace": [COL], "name": TABLE}
    ]


def test_t11_update_is_501_tags_unchanged(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _seed_iceberg_table(sqlite_registry)
    _assert_501(
        client.post(
            f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}",
            json={"updates": [{"action": "set-properties"}]},
        )
    )
    stored = sqlite_registry.get_saved_dataset(
        scoped_name(NS, COL, TABLE), CATALOG_PROJECT, allow_cache=False
    )
    assert stored.tags["owner"] == "uw"
    assert "tier" not in (stored.tags or {})


def test_no_write_models():
    from feast.api.data_catalog import models

    assert hasattr(models, "LoadTableResponse")
    assert not hasattr(models, "CreateTableRequest")
    assert not hasattr(models, "UpdateTableRequest")
    assert not hasattr(models, "RenameTableRequest")


def test_t13_rename_is_501_source_remains(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _seed_iceberg_table(sqlite_registry)
    _assert_501(client.post(f"/v1/{NS}/tables/rename"))
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}").status_code == 204


def test_t15_list_skips_volumes(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _seed_iceberg_table(sqlite_registry)
    sqlite_registry.apply_saved_dataset(
        SavedDataset(
            name=scoped_name(NS, COL, "docs"),
            features=["fv:feature"],
            join_keys=["entity_id"],
            storage=SavedDatasetFileStorage(path="s3://bucket/docs/"),
            namespace=NS,
            collection=COL,
            tags={"_catalog_managed": "true", "asset_type": "volume"},
        ),
        CATALOG_PROJECT,
    )
    listed = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert listed.json()["identifiers"] == [{"namespace": [COL], "name": TABLE}]


def test_list_skips_iceberg_tags_without_catalog_managed(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    sqlite_registry.apply_saved_dataset(
        SavedDataset(
            name=scoped_name(NS, COL, "unmanaged"),
            features=["fv:feature"],
            join_keys=["entity_id"],
            storage=SavedDatasetFileStorage(path="s3://bucket/unmanaged/"),
            namespace=NS,
            collection=COL,
            tags={"asset_type": "table", "format": "iceberg"},
        ),
        CATALOG_PROJECT,
    )
    listed = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert listed.json() == {"identifiers": []}
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/unmanaged").status_code == 404


def test_list_skips_untagged_saved_dataset(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    sqlite_registry.apply_saved_dataset(
        SavedDataset(
            name=scoped_name(NS, COL, "plain"),
            features=["fv:feature"],
            join_keys=["entity_id"],
            storage=SavedDatasetFileStorage(path="s3://bucket/plain/"),
            namespace=NS,
            collection=COL,
        ),
        CATALOG_PROJECT,
    )
    listed = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert listed.json() == {"identifiers": []}
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/plain").status_code == 404


def test_t16_config_advertises_read_not_writes(sqlite_registry):
    endpoints = _client(sqlite_registry).get("/v1/config").json()["endpoints"]
    assert endpoints == CATALOG_CONFIG_ENDPOINTS
    assert "GET /v1/{prefix}/namespaces/{namespace}/tables" in endpoints
    assert "GET /v1/{prefix}/namespaces/{namespace}/tables/{table}" in endpoints
    assert "HEAD /v1/{prefix}/namespaces/{namespace}/tables/{table}" in endpoints
    assert "POST /v1/{prefix}/namespaces/{namespace}/tables" not in endpoints
    assert "POST /v1/{prefix}/namespaces/{namespace}/tables/{table}" not in endpoints
    assert "DELETE /v1/{prefix}/namespaces/{namespace}/tables/{table}" not in endpoints
    assert "POST /v1/{prefix}/tables/rename" not in endpoints
    assert "GET /v1/{prefix}/namespaces/{namespace}/volumes" in endpoints
    assert "POST /v1/{prefix}/namespaces/{namespace}/generic-tables" in endpoints
    assert "GET /v1/projects" not in endpoints
    assert "GET /v1/{prefix}/search" in endpoints
    assert "GET /v1/{prefix}/labels" in endpoints
    assert "POST /v1/{prefix}/labels" in endpoints
    assert "DELETE /v1/{prefix}/labels/{label}" in endpoints


def test_t17_create_in_default_is_501(sqlite_registry):
    _assert_501(
        _client(sqlite_registry).post(
            f"/v1/{NS}/namespaces/{DEFAULT_COLLECTION}/tables"
        )
    )


def test_seeded_table_blocks_delete_collection_409(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _seed_iceberg_table(sqlite_registry)
    dropped = client.delete(f"/v1/{NS}/namespaces/{COL}")
    assert dropped.status_code == 409
    assert dropped.json()["error"]["type"] == "NamespaceNotEmptyException"
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}").status_code == 204
