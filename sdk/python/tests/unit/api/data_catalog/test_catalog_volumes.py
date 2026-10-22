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

import json
import tempfile
import threading

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy.pool import NullPool

from feast.api.data_catalog.catalog_utils import (
    CATALOG_PROJECT,
    DEFAULT_COLLECTION,
    scoped_name,
)
from feast.api.data_catalog.config import get_config_router
from feast.api.data_catalog.errors import register_error_handlers
from feast.api.data_catalog.namespaces import get_namespace_router
from feast.api.data_catalog.tables import get_table_router
from feast.api.data_catalog.volumes import get_volume_router
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig

NS = "demo-user-1"
COL = "underwriting"
VOL = "claims-pdfs"
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
    return TestClient(app, raise_server_exceptions=False)


def _ensure_collection(client: TestClient, project: str = NS, collection: str = COL):
    client.post(
        f"/v1/{project}/namespaces",
        json={"namespace": [collection], "properties": {}},
    )


def _create_volume(client: TestClient, **body):
    payload = {
        "name": VOL,
        "format": "documents",
        "storage_location": "s3://bucket/claims/",
    }
    payload.update(body)
    return client.post(f"/v1/{NS}/namespaces/{COL}/volumes", json=payload, headers=AUTH)


def test_list_empty_default(sqlite_registry):
    response = _client(sqlite_registry).get(
        f"/v1/{NS}/namespaces/{DEFAULT_COLLECTION}/volumes"
    )
    assert response.status_code == 200
    assert response.json() == {"volumes": []}


def test_list_missing_collection_404(sqlite_registry):
    response = _client(sqlite_registry).get(f"/v1/{NS}/namespaces/{COL}/volumes")
    assert response.status_code == 404
    assert response.json()["error"]["type"] == "NoSuchNamespaceException"


def test_create_get_head_delete(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client)
    assert created.status_code == 200, created.text
    body = created.json()
    assert body["name"] == VOL
    assert body["asset_type"] == "volume"
    assert body["format"] == "documents"
    assert body["collection"] == COL
    assert body["uuid"]
    assert body["owner"] == "test-user"
    assert body["storage_location"] == "s3://bucket/claims/"
    assert body["columns"] is None
    for key in (
        "schema-name",
        "volume-type",
        "config",
        "comment",
        "location",
        "registered_by",
    ):
        assert key not in body
    assert "document_count" not in body

    listed = client.get(f"/v1/{NS}/namespaces/{COL}/volumes")
    assert listed.status_code == 200
    assert listed.json()["volumes"][0]["name"] == VOL

    got = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert got.status_code == 200
    assert got.json()["name"] == VOL
    assert client.head(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}").status_code == 204

    iceberg = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert iceberg.json() == {"identifiers": []}

    dropped = client.delete(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert dropped.status_code == 204
    assert dropped.content == b""
    missing = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert missing.status_code == 404
    assert missing.json()["error"]["type"] == "NoSuchVolumeException"


def test_create_in_default_without_namespace_post(sqlite_registry):
    client = _client(sqlite_registry)
    created = client.post(
        f"/v1/{NS}/namespaces/{DEFAULT_COLLECTION}/volumes",
        json={
            "name": VOL,
            "format": "documents",
            "storage_location": "s3://bucket/claims/",
        },
        headers=AUTH,
    )
    assert created.status_code == 200, created.text
    assert created.json()["collection"] == DEFAULT_COLLECTION
    stored = sqlite_registry.get_saved_dataset(
        scoped_name(NS, DEFAULT_COLLECTION, VOL), CATALOG_PROJECT, allow_cache=False
    )
    assert stored.tags["asset_type"] == "volume"
    assert stored.tags["_catalog_managed"] == "true"


def test_duplicate_is_409(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert _create_volume(client).status_code == 200
    again = _create_volume(client)
    assert again.status_code == 409
    assert again.json()["error"]["type"] == "AlreadyExistsException"


def test_missing_volume_404(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert client.get(f"/v1/{NS}/namespaces/{COL}/volumes/missing").status_code == 404
    assert client.head(f"/v1/{NS}/namespaces/{COL}/volumes/missing").status_code == 404
    deleted = client.delete(f"/v1/{NS}/namespaces/{COL}/volumes/missing")
    assert deleted.status_code == 404


def test_update_description_and_storage_location(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _create_volume(client)
    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={
            "description": "claims PDFs",
            "storage_location": "s3://bucket/claims-v2/",
        },
    )
    assert updated.status_code == 200
    body = updated.json()
    assert body["description"] == "claims PDFs"
    assert body["storage_location"] == "s3://bucket/claims-v2/"


def test_storage_location_omitted_is_preserved_and_null_clears(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert _create_volume(client).status_code == 200

    preserved = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"description": "claims PDFs"},
    )
    assert preserved.status_code == 200, preserved.text
    assert preserved.json()["storage_location"] == "s3://bucket/claims/"

    cleared = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"storage_location": None},
    )
    assert cleared.status_code == 200, cleared.text
    assert cleared.json()["storage_location"] is None


def test_volume_put_returns_405(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _create_volume(client)
    assert (
        client.put(
            f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
            json={"description": "x"},
        ).status_code
        == 405
    )


def test_v1_projects_route_removed(sqlite_registry):
    client = _client(sqlite_registry)
    assert client.get("/v1/projects").status_code == 404


def test_properties_cannot_overwrite_reserved_tags(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client)
    assert created.status_code == 200, created.text
    original = created.json()
    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={
            "properties": {
                "team": "uw",
                "asset_type": "table",
                "format": "iceberg",
                "_catalog_managed": "false",
                "uuid": "00000000-0000-4000-8000-000000000000",
                "owner": "attacker",
            }
        },
    )
    assert updated.status_code == 200, updated.text
    body = updated.json()
    assert body["properties"] == {"team": "uw"}
    assert body["format"] == "documents"
    assert body["asset_type"] == "volume"
    assert body["uuid"] == original["uuid"]
    assert body["owner"] == original["owner"]
    iceberg = client.get(f"/v1/{NS}/namespaces/{COL}/tables")
    assert iceberg.json() == {"identifiers": []}
    got = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert got.status_code == 200
    assert got.json()["name"] == VOL


def _threaded_sqlite_registry():
    _fd, registry_path = tempfile.mkstemp()
    registry = SqlRegistry(
        SqlRegistryConfig(
            registry_type="sql",
            path=f"sqlite:///{registry_path}",
            purge_feast_metadata=False,
            sqlalchemy_config_kwargs={
                "echo": False,
                "connect_args": {"check_same_thread": False, "timeout": 30},
                "poolclass": NullPool,
            },
        ),
        "scratch",
        None,
    )
    return registry, registry_path


def test_default_apply_saved_dataset_still_updates(sqlite_registry):
    from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
    from feast.saved_dataset import SavedDataset

    name = scoped_name(NS, COL, VOL)
    first = SavedDataset(
        name=name,
        features=["fv:feature"],
        join_keys=["entity_id"],
        storage=SavedDatasetFileStorage(path="s3://first/"),
        namespace=NS,
        collection=COL,
        tags={"_catalog_managed": "true", "asset_type": "volume"},
    )
    sqlite_registry.apply_saved_dataset(first, CATALOG_PROJECT)
    second = SavedDataset(
        name=name,
        features=["fv:feature"],
        join_keys=["entity_id"],
        storage=SavedDatasetFileStorage(path="s3://second/"),
        namespace=NS,
        collection=COL,
        tags={"_catalog_managed": "true", "asset_type": "volume"},
    )
    sqlite_registry.apply_saved_dataset(second, CATALOG_PROJECT)
    got = sqlite_registry.get_saved_dataset(name, CATALOG_PROJECT, allow_cache=False)
    assert got.storage.file_options.uri == "s3://second/"


def test_concurrent_create_same_volume_is_created_and_409():
    from feast.api.data_catalog.catalog_assets import insert_catalog_dataset
    from feast.api.data_catalog.errors import AlreadyExistsException

    registry, _path = _threaded_sqlite_registry()
    try:
        client = _client(registry)
        _ensure_collection(client)
        barrier = threading.Barrier(2)
        outcomes: list[str] = []
        errors: list[BaseException] = []

        def worker(location: str) -> None:
            barrier.wait()
            try:
                insert_catalog_dataset(
                    registry,
                    rhai_ns=NS,
                    collection=COL,
                    display_name=VOL,
                    location=location,
                    tags={
                        "asset_type": "volume",
                        "format": "documents",
                        "owner": "test-user",
                    },
                )
                outcomes.append("created")
            except AlreadyExistsException:
                outcomes.append("409")
            except BaseException as exc:
                errors.append(exc)

        threads = [
            threading.Thread(target=worker, args=(f"s3://bucket/{i}/",))
            for i in range(2)
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=30)
            assert not thread.is_alive()

        assert not errors, errors
        assert sorted(outcomes) == ["409", "created"]
        listed = client.get(f"/v1/{NS}/namespaces/{COL}/volumes")
        assert listed.status_code == 200
        assert len(listed.json()["volumes"]) == 1
        assert listed.json()["volumes"][0]["name"] == VOL
        again = client.post(
            f"/v1/{NS}/namespaces/{COL}/volumes",
            json={
                "name": VOL,
                "format": "documents",
                "storage_location": "s3://after-race/",
            },
            headers=AUTH,
        )
        assert again.status_code == 409
        assert again.json()["error"]["type"] == "AlreadyExistsException"
    finally:
        registry.teardown()


def test_timestamps_populated_on_create(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client)
    assert created.status_code == 200, created.text
    body = created.json()
    assert body["created_at"] is not None
    assert body["updated_at"] is not None


def test_updated_at_changes_after_update(sqlite_registry):
    import time

    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client)
    created_at = created.json()["created_at"]
    updated_at_v1 = created.json()["updated_at"]
    assert created_at is not None
    assert updated_at_v1 is not None

    time.sleep(0.05)
    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"description": "v2"},
    )
    assert updated.status_code == 200
    assert updated.json()["created_at"] == created_at  # unchanged
    assert updated.json()["updated_at"] >= updated_at_v1  # moved forward


def test_create_without_identity_header_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={"name": VOL, "format": "documents"},
    )
    assert resp.status_code == 400
    assert resp.json()["error"]["type"] == "BadRequestException"


def test_create_with_owner_in_body_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": VOL,
            "format": "documents",
            "owner": "uw-team",
        },
        headers=AUTH,
    )
    assert resp.status_code == 400


def test_get_without_format_is_400_not_other(sqlite_registry):
    """Stored volume_type=EXTERNAL is not a stand-in for UnstructuredFormat."""
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert _create_volume(client).status_code == 200
    stored = sqlite_registry.get_saved_dataset(
        scoped_name(NS, COL, VOL), CATALOG_PROJECT, allow_cache=False
    )
    tags = dict(stored.tags or {})
    tags.pop("format", None)
    tags["volume_type"] = "EXTERNAL"
    stored.tags = tags
    sqlite_registry.apply_saved_dataset(stored, CATALOG_PROJECT)
    resp = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert resp.status_code == 400
    assert resp.json()["error"]["type"] == "BadRequestException"
    assert "format" in resp.json()["error"]["message"].lower()


def test_get_without_owner_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert _create_volume(client).status_code == 200
    stored = sqlite_registry.get_saved_dataset(
        scoped_name(NS, COL, VOL), CATALOG_PROJECT, allow_cache=False
    )
    tags = dict(stored.tags or {})
    tags.pop("owner", None)
    tags["registered_by"] = "legacy-user"
    stored.tags = tags
    sqlite_registry.apply_saved_dataset(stored, CATALOG_PROJECT)
    resp = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert resp.status_code == 400
    assert resp.json()["error"]["type"] == "BadRequestException"
    assert "owner" in resp.json()["error"]["message"].lower()


def test_patch_does_not_change_owner(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={"name": VOL, "format": "documents"},
        headers={"X-User": "creator"},
    )
    assert created.status_code == 200, created.text
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"description": "updated"},
        headers={"X-User": "editor"},
    )
    assert patched.status_code == 200, patched.text
    assert patched.json()["owner"] == "creator"
    assert "registered_by" not in patched.json()
    assert "updated_by" not in patched.json()


def test_create_with_location_field_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={"name": VOL, "format": "documents", "location": "s3://x/"},
        headers=AUTH,
    )
    assert resp.status_code == 400


def test_create_without_format_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={"name": VOL},
        headers=AUTH,
    )
    assert resp.status_code == 400


def test_create_parquet_format_on_volume_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    resp = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={"name": VOL, "format": "parquet"},
        headers=AUTH,
    )
    assert resp.status_code == 400


def test_update_add_labels(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _create_volume(client)
    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"add_labels": ["pii"]},
    )
    assert updated.status_code == 200, updated.text
    assert updated.json()["labels"] == ["pii"]
    got = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert got.json()["labels"] == ["pii"]


def test_update_remove_labels(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": VOL,
            "format": "documents",
            "storage_location": "s3://bucket/claims/",
            "labels": ["uw", "pii"],
        },
        headers=AUTH,
    )
    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"remove_labels": ["pii"]},
    )
    assert updated.status_code == 200, updated.text
    assert updated.json()["labels"] == ["uw"]


def test_update_add_and_remove_labels_combined(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": VOL,
            "format": "documents",
            "storage_location": "s3://bucket/claims/",
            "labels": ["uw", "old"],
        },
        headers=AUTH,
    )
    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"add_labels": ["new"], "remove_labels": ["old"]},
    )
    assert updated.status_code == 200, updated.text
    assert updated.json()["labels"] == ["uw", "new"]


def test_update_add_label_idempotent(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": VOL,
            "format": "documents",
            "storage_location": "s3://bucket/claims/",
            "labels": ["uw"],
        },
        headers=AUTH,
    )
    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"add_labels": ["uw"]},
    )
    assert updated.status_code == 200, updated.text
    assert updated.json()["labels"] == ["uw"]


def test_update_remove_missing_label_noop(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": VOL,
            "format": "documents",
            "storage_location": "s3://bucket/claims/",
            "labels": ["uw"],
        },
        headers=AUTH,
    )
    updated = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"remove_labels": ["nonexistent"]},
    )
    assert updated.status_code == 200, updated.text
    assert updated.json()["labels"] == ["uw"]


def test_connection_ref_round_trips(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": VOL,
            "format": "documents",
            "storage_location": "s3://bucket/claims/",
            "connection_ref": {
                "type": "secret",
                "secret_name": "aws-creds",  # pragma: allowlist secret
                "name": "Production S3",
                "connectionType": "s3",
            },
        },
        headers=AUTH,
    )
    assert created.status_code == 200, created.text
    assert created.json()["connection_ref"] == {
        "type": "secret",
        "secret_name": "aws-creds",  # pragma: allowlist secret
    }
    stored = sqlite_registry.get_saved_dataset(
        scoped_name(NS, COL, VOL), CATALOG_PROJECT, allow_cache=False
    )
    assert json.loads(stored.tags["_connection_ref"]) == {
        "type": "secret",
        "secret_name": "aws-creds",  # pragma: allowlist secret
    }
    got = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert got.json()["connection_ref"] == {
        "type": "secret",
        "secret_name": "aws-creds",  # pragma: allowlist secret
    }
    bad = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "other",
            "format": "documents",
            "storage_location": "s3://bucket/other/",
            "connection_ref": {"type": "unknown"},
        },
        headers=AUTH,
    )
    assert bad.status_code == 400, bad.text
    assert bad.json()["error"]["type"] == "BadRequestException"

    legacy = client.post(
        f"/v1/{NS}/namespaces/{COL}/volumes",
        json={
            "name": "legacy",
            "format": "documents",
            "connection_ref": {
                "type": "rhai",
                "secret_name": "aws-creds",  # pragma: allowlist secret
            },
        },
        headers=AUTH,
    )
    assert legacy.status_code == 400, legacy.text
    assert legacy.json()["error"]["type"] == "BadRequestException"


def test_remove_custom_property(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client, properties={"team": "a", "cost": "high"})
    assert created.status_code == 200, created.text
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"remove_properties": ["cost"]},
    )
    assert patched.status_code == 200, patched.text
    got = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    props = got.json()["properties"]
    assert props.get("team") == "a"
    assert "cost" not in props


def test_remove_property_governance_key_rejected(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _create_volume(client)
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"remove_properties": ["purpose"]},
    )
    assert patched.status_code == 400


def test_remove_property_overlap_with_set_rejected(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _create_volume(client)
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"properties": {"x": "1"}, "remove_properties": ["x"]},
    )
    assert patched.status_code == 400


def test_remove_nonexistent_property_is_noop(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    _create_volume(client)
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"remove_properties": ["nokey"]},
    )
    assert patched.status_code == 200, patched.text


def test_properties_merge_not_replace(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client, properties={"a": "1", "b": "2"})
    assert created.status_code == 200, created.text
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"properties": {"b": "3"}},
    )
    assert patched.status_code == 200, patched.text
    got = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    props = got.json()["properties"]
    assert props.get("a") == "1"
    assert props.get("b") == "3"


def test_license_enum_valid(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client, license="cc-by-4.0")
    assert created.status_code == 200, created.text


def test_license_enum_invalid(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client, license="free-for-all")
    assert created.status_code == 400


def test_maturity_enum_valid(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client, maturity="production")
    assert created.status_code == 200, created.text


def test_pii_enum_invalid(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client, pii="maybe")
    assert created.status_code == 400


def test_governance_null_clears(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client, purpose="train", license="apache-2.0")
    assert created.status_code == 200, created.text
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"purpose": None, "license": None},
    )
    assert patched.status_code == 200, patched.text
    got = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    props = got.json().get("properties") or {}
    assert "purpose" not in props
    assert "license" not in props


def test_governance_absent_is_noop(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    created = _create_volume(client, purpose="train")
    assert created.status_code == 200, created.text
    patched = client.patch(
        f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}",
        json={"description": "new"},
    )
    assert patched.status_code == 200, patched.text
    got = client.get(f"/v1/{NS}/namespaces/{COL}/volumes/{VOL}")
    assert got.json()["properties"].get("purpose") == "train"
