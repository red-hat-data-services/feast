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

import tempfile

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from feast.api.data_catalog.catalog_utils import (
    CATALOG_PROJECT,
    DEFAULT_COLLECTION,
    scoped_name,
)
from feast.api.data_catalog.config import get_config_router
from feast.api.data_catalog.errors import register_error_handlers
from feast.api.data_catalog.generic_tables import get_generic_table_router
from feast.api.data_catalog.namespaces import get_namespace_router
from feast.api.data_catalog.tables import get_table_router
from feast.api.data_catalog.volumes import get_volume_router
from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig
from feast.saved_dataset import SavedDataset

NS = "demo-user-1"
COL = "underwriting"
TABLE = "events"
PARQUET = "claims-parquet"
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
    return TestClient(app, raise_server_exceptions=False)


def _ensure_collection(client: TestClient, project: str = NS, collection: str = COL):
    client.post(
        f"/v1/{project}/namespaces",
        json={"namespace": [collection], "properties": {}},
    )


def _assert_501(response) -> None:
    assert response.status_code == 501, response.text
    assert response.json()["error"]["type"] == "NotImplementedException"


def _seed_iceberg_table(registry, name: str = TABLE) -> None:
    registry.apply_saved_dataset(
        SavedDataset(
            name=scoped_name(NS, COL, name),
            features=["fv:feature"],
            join_keys=["entity_id"],
            storage=SavedDatasetFileStorage(path="s3://bucket/events/"),
            namespace=NS,
            collection=COL,
            tags={
                "_catalog_managed": "true",
                "asset_type": "table",
                "format": "iceberg",
                "owner": "uw",
                "uuid": "00000000-0000-4000-8000-000000000001",
            },
        ),
        CATALOG_PROJECT,
    )


def test_iceberg_format_post_inserts_catalog_row(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp_default = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": TABLE},
        headers=AUTH,
    )
    assert resp_default.status_code == 400
    resp_explicit = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": "explicit-ice", "format": "iceberg"},
        headers=AUTH,
    )
    assert resp_explicit.status_code == 201
    listed = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables")
    assert len(listed.json()["assets"]) == 1
    iceberg = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert len(iceberg.json()["identifiers"]) == 1


def test_create_parquet_201_no_invented_user(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": PARQUET,
            "format": "parquet",
            "storage_location": "s3://bucket/claims.parquet",
            "schema_fields": [
                {"name": "claim_id", "type": "string", "nullable": False}
            ],
        },
        headers=AUTH,
    )
    assert created.status_code == 201, created.text
    body = created.json()
    assert body["name"] == PARQUET
    assert body["asset_type"] == "table"
    assert body["format"] == "parquet"
    assert body["storage_location"] == "s3://bucket/claims.parquet"
    assert body["collection"] == COL
    assert body["owner"] == "test-user"
    assert "registered_by" not in body
    assert "schema_fields" not in body
    assert body["columns"][0]["name"] == "claim_id"
    assert "document_count" not in body

    iceberg = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert iceberg.json() == {"identifiers": []}


def test_owner_from_header_only(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "csv"},
        headers={"X-User": "uw-analyst"},
    )
    assert created.status_code == 201
    assert created.json()["owner"] == "uw-analyst"
    assert "registered_by" not in created.json()


def test_duplicate_is_409(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    payload = {"name": PARQUET, "format": "parquet"}
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json=payload,
            headers=AUTH,
        ).status_code
        == 201
    )
    again = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables", json=payload, headers=AUTH
    )
    assert again.status_code == 409
    assert again.json()["error"]["type"] == "AlreadyExistsException"


def test_list_includes_seeded_iceberg_and_skips_volumes(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _seed_iceberg_table(sqlite_registry)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet"},
        headers=AUTH,
    )
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "docs",
            "format": "documents",
            "storage_location": "s3://bucket/docs/",
        },
        headers=AUTH,
    )
    listed = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables")
    assert listed.status_code == 200
    names = sorted(asset["name"] for asset in listed.json()["assets"])
    assert names == [PARQUET, TABLE]
    iceberg = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert iceberg.json()["identifiers"] == [{"namespace": [COL], "name": TABLE}]


def test_patch_replaces_schema_and_rejects_iceberg_format(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": PARQUET,
            "format": "parquet",
            "schema_fields": [{"name": "a", "type": "string"}],
            "labels": ["uw"],
        },
        headers=AUTH,
    )
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={
            "description": "claims",
            "schema_fields": [
                {"name": "b", "type": "long"},
                {"name": "c", "type": "string"},
            ],
            "add_labels": ["pii"],
        },
    )
    assert patched.status_code == 200, patched.text
    body = patched.json()
    assert body["description"] == "claims"
    assert [col["name"] for col in body["columns"]] == ["b", "c"]
    assert body["labels"] == ["uw", "pii"]
    patched_ice = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"format": "iceberg"},
    )
    assert patched_ice.status_code == 200
    assert patched_ice.json()["format"] == "iceberg"


def test_get_delete_and_missing(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "csv"},
        headers=AUTH,
    )
    got = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}")
    assert got.status_code == 200
    assert got.json()["format"] == "csv"
    deleted = client.delete(f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}")
    assert deleted.status_code == 204
    missing = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}")
    assert missing.status_code == 404
    assert missing.json()["error"]["type"] == "NoSuchTableException"


def test_missing_collection_404(sqlite_registry):
    client = _client(sqlite_registry)
    listed = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables")
    assert listed.status_code == 404
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet"},
    )
    assert created.status_code == 404


def test_create_in_default(sqlite_registry):
    client = _client(sqlite_registry)
    created = client.post(
        f"/v1/{NS}/namespaces/{DEFAULT_COLLECTION}/generic-tables",
        json={"name": PARQUET, "format": "postgresql"},
        headers=AUTH,
    )
    assert created.status_code == 201, created.text
    assert created.json()["collection"] == DEFAULT_COLLECTION


def test_label_query_filters_list(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet", "labels": ["uw"]},
        headers=AUTH,
    )
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": "other", "format": "csv", "labels": ["finance"]},
        headers=AUTH,
    )
    filtered = client.get(
        f"/v1/{NS}/namespaces/{COL}/generic-tables", params={"label": "uw"}
    )
    assert [asset["name"] for asset in filtered.json()["assets"]] == [PARQUET]


def test_properties_cannot_overwrite_reserved_tags(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet"},
        headers=AUTH,
    )
    assert created.status_code == 201, created.text
    original = created.json()
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={
            "properties": {
                "team": "uw",
                "format": "iceberg",
                "asset_type": "volume",
                "uuid": "00000000-0000-4000-8000-000000000000",
                "owner": "attacker",
            }
        },
    )
    assert patched.status_code == 200, patched.text
    body = patched.json()
    assert body["format"] == "parquet"
    assert body["asset_type"] == "table"
    assert body["uuid"] == original["uuid"]
    assert body["owner"] == original["owner"]
    assert body["properties"] == {"team": "uw"}
    iceberg = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert iceberg.json() == {"identifiers": []}


def test_storage_location_omitted_is_preserved_and_null_clears(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": PARQUET,
            "format": "parquet",
            "storage_location": "s3://bucket/claims.parquet",
        },
        headers=AUTH,
    )
    assert created.status_code == 201, created.text

    preserved = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"description": "claims"},
    )
    assert preserved.status_code == 200, preserved.text
    assert preserved.json()["storage_location"] == "s3://bucket/claims.parquet"

    cleared = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"storage_location": None},
    )
    assert cleared.status_code == 200, cleared.text
    assert cleared.json()["storage_location"] is None


def test_metadata_fields_round_trip_in_properties(sqlite_registry):
    """purpose, license, maturity, domain, pii are returned inside properties."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": "annotated",
            "format": "parquet",
            "purpose": "analytics",
            "license": "proprietary",
            "maturity": "production",
            "domain": "claims",
            "pii": "none",
        },
        headers=AUTH,
    )
    assert created.status_code == 201, created.text
    props = created.json()["properties"]
    assert props["purpose"] == "analytics"
    assert props["license"] == "proprietary"
    assert props["maturity"] == "production"
    assert props["domain"] == "claims"
    assert props["pii"] == "none"

    got = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables/annotated")
    assert got.status_code == 200
    got_props = got.json()["properties"]
    assert got_props["purpose"] == "analytics"
    assert got_props["license"] == "proprietary"
    assert got_props["maturity"] == "production"
    assert got_props["domain"] == "claims"
    assert got_props["pii"] == "none"


def test_uuid_and_timestamps_on_create(sqlite_registry):
    """uuid, created_at, updated_at are populated on create."""
    import uuid as _uuid

    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet"},
        headers=AUTH,
    )
    assert created.status_code == 201, created.text
    body = created.json()
    _uuid.UUID(body["uuid"])  # valid UUID or raises
    assert body["created_at"] is not None
    assert body["updated_at"] is not None


def test_create_without_identity_header_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet"},
    )
    assert resp.status_code == 400
    assert resp.json()["error"]["type"] == "BadRequestException"


def test_create_with_owner_in_body_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet", "owner": "uw-team"},
        headers=AUTH,
    )
    assert resp.status_code == 400


def test_create_with_location_field_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": PARQUET,
            "format": "parquet",
            "location": "s3://bucket/x",
        },
        headers=AUTH,
    )
    assert resp.status_code == 400


def test_create_documents_format_on_table_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "documents"},
        headers=AUTH,
    )
    assert resp.status_code == 400


def test_patch_does_not_change_owner_from_header(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet"},
        headers={"X-User": "creator"},
    )
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"description": "updated"},
        headers={"X-User": "editor"},
    )
    assert patched.status_code == 200, patched.text
    assert patched.json()["owner"] == "creator"
    assert "registered_by" not in patched.json()
    assert "updated_by" not in patched.json()


def test_connection_ref_round_trips_on_generic_table(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": PARQUET,
            "format": "parquet",
        },
        headers=AUTH,
    )
    assert created.status_code == 201, created.text

    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={
            "connection_ref": {
                "type": "dch",
                "id": "550e8400-e29b-41d4-a716-446655440000",
                "name": "Production data",
                "connectionType": "s3",
            },
        },
    )
    assert updated.status_code == 200, updated.text
    assert updated.json()["connection_ref"] == {
        "type": "dch",
        "id": "550e8400-e29b-41d4-a716-446655440000",
    }
    got = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}")
    assert got.json()["connection_ref"] == {
        "type": "dch",
        "id": "550e8400-e29b-41d4-a716-446655440000",
    }


def test_updated_at_changes_after_patch(sqlite_registry):
    import time

    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet"},
        headers=AUTH,
    )
    assert created.status_code == 201
    created_at = created.json()["created_at"]
    updated_at_v1 = created.json()["updated_at"]
    assert created_at is not None
    assert updated_at_v1 is not None

    time.sleep(0.05)
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"description": "v2"},
    )
    assert patched.status_code == 200
    assert patched.json()["created_at"] == created_at  # unchanged
    assert patched.json()["updated_at"] >= updated_at_v1  # moved forward


def test_generic_delete_unregisters_iceberg_catalog_row(sqlite_registry):
    """Data Hub unregister (option A). Iceberg DELETE stays 501."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _seed_iceberg_table(sqlite_registry)
    _assert_501(client.delete(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}"))
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}").status_code == 204
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{TABLE}",
        json={"description": "stale warehouse table"},
    )
    assert patched.status_code == 200, patched.text
    assert patched.json()["description"] == "stale warehouse table"
    assert patched.json()["format"] == "iceberg"
    dropped = client.delete(f"/v1/{NS}/namespaces/{COL}/generic-tables/{TABLE}")
    assert dropped.status_code == 204
    iceberg = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert iceberg.json() == {"identifiers": []}
    assert client.head(f"/v1/{NS}/namespaces/{COL}/tables/{TABLE}").status_code == 404


def test_remove_custom_property(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": PARQUET,
            "format": "parquet",
            "properties": {"team": "a", "cost": "high"},
        },
        headers=AUTH,
    )
    assert created.status_code == 201, created.text
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"remove_properties": ["cost"]},
    )
    assert patched.status_code == 200, patched.text
    got = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}")
    props = got.json()["properties"]
    assert props.get("team") == "a"
    assert "cost" not in props


def test_remove_property_governance_key_rejected(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet"},
        headers=AUTH,
    )
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"remove_properties": ["purpose"]},
    )
    assert patched.status_code == 400


def test_properties_merge_not_replace(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": PARQUET,
            "format": "parquet",
            "properties": {"a": "1", "b": "2"},
        },
        headers=AUTH,
    )
    assert created.status_code == 201, created.text
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"properties": {"b": "3"}},
    )
    assert patched.status_code == 200, patched.text
    got = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}")
    props = got.json()["properties"]
    assert props.get("a") == "1"
    assert props.get("b") == "3"


def test_governance_null_clears(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={
            "name": PARQUET,
            "format": "parquet",
            "purpose": "train",
            "license": "apache-2.0",
        },
        headers=AUTH,
    )
    assert created.status_code == 201, created.text
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}",
        json={"purpose": None, "license": None},
    )
    assert patched.status_code == 200, patched.text
    got = client.get(f"/v1/{NS}/namespaces/{COL}/generic-tables/{PARQUET}")
    props = got.json().get("properties") or {}
    assert "purpose" not in props
    assert "license" not in props


def test_maturity_enum_valid_on_create(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet", "maturity": "staging"},
        headers=AUTH,
    )
    assert created.status_code == 201, created.text


def test_pii_enum_invalid_on_create(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/generic-tables",
        json={"name": PARQUET, "format": "parquet", "pii": "unknown"},
        headers=AUTH,
    )
    assert created.status_code == 400
