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
from sqlalchemy.pool import NullPool

from feast.api.data_catalog.catalog_utils import (
    CATALOG_PROJECT,
    DEFAULT_COLLECTION,
    ns_meta_key,
    scoped_name,
)
from feast.api.data_catalog.config import CATALOG_CONFIG_ENDPOINTS, get_config_router
from feast.api.data_catalog.errors import register_error_handlers
from feast.api.data_catalog.namespaces import get_namespace_router
from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig
from feast.saved_dataset import SavedDataset

NS = "demo-user-1"
NS2 = "demo-user-2"
COL = "underwriting"


@pytest.fixture
def sqlite_registry():
    fd, registry_path = tempfile.mkstemp()
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
    return TestClient(app, raise_server_exceptions=False)


def _make_saved_dataset(name: str, namespace: str, collection: str) -> SavedDataset:
    return SavedDataset(
        name=name,
        features=["fv:feature"],
        join_keys=["entity_id"],
        storage=SavedDatasetFileStorage(path="file:///tmp/dataset.parquet"),
        namespace=namespace,
        collection=collection,
        tags={"_catalog_managed": "true", "asset_type": "table"},
    )


def test_n1_list_empty_registry_has_default(sqlite_registry):
    response = _client(sqlite_registry).get(f"/v1/{NS}/namespaces")
    assert response.status_code == 200
    assert response.json() == {"namespaces": [[DEFAULT_COLLECTION]]}


def test_n2_create_round_trip_and_scoped_tag(sqlite_registry):
    client = _client(sqlite_registry)
    response = client.post(
        f"/v1/{NS}/namespaces",
        json={"namespace": [COL], "properties": {"owner": "uw"}},
    )
    assert response.status_code == 200
    assert response.json() == {
        "namespace": [COL],
        "properties": {"owner": "uw"},
    }

    listed = client.get(f"/v1/{NS}/namespaces")
    assert listed.status_code == 200
    assert listed.json()["namespaces"] == [[DEFAULT_COLLECTION], [COL]]

    got = client.get(f"/v1/{NS}/namespaces/{COL}")
    assert got.status_code == 200
    assert got.json()["properties"] == {"owner": "uw"}

    head = client.head(f"/v1/{NS}/namespaces/{COL}")
    assert head.status_code == 204

    project = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    key = ns_meta_key(NS, COL)
    assert key in project.tags
    assert NS in key
    assert f"_ns_meta_{COL}" not in project.tags
    assert project.description == "RHOAI Data Registry"


def test_n3_duplicate_create_is_409_already_exists(sqlite_registry):
    client = _client(sqlite_registry)
    client.post(f"/v1/{NS}/namespaces", json={"namespace": [COL]})
    response = client.post(f"/v1/{NS}/namespaces", json={"namespace": [COL]})
    assert response.status_code == 409
    error = response.json()["error"]
    assert error["type"] == "AlreadyExistsException"
    assert error["code"] == 409
    assert "detail" not in response.json()


def test_create_when_tables_exist_is_409(sqlite_registry):
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(
            scoped_name(NS, COL, "risk_scores"),
            NS,
            COL,
        ),
        CATALOG_PROJECT,
    )
    response = _client(sqlite_registry).post(
        f"/v1/{NS}/namespaces", json={"namespace": [COL]}
    )
    assert response.status_code == 409
    assert response.json()["error"]["type"] == "AlreadyExistsException"


def test_n4_same_collection_name_isolated_across_tenants(sqlite_registry):
    client = _client(sqlite_registry)
    first = client.post(
        f"/v1/{NS}/namespaces",
        json={"namespace": [COL], "properties": {"owner": "uw-1"}},
    )
    assert first.status_code == 200
    second = client.post(
        f"/v1/{NS2}/namespaces",
        json={"namespace": [COL], "properties": {"owner": "uw-2"}},
    )
    assert second.status_code == 200
    assert client.get(f"/v1/{NS}/namespaces/{COL}").json()["properties"] == {
        "owner": "uw-1"
    }
    assert client.get(f"/v1/{NS2}/namespaces/{COL}").json()["properties"] == {
        "owner": "uw-2"
    }
    project = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert ns_meta_key(NS, COL) in project.tags
    assert ns_meta_key(NS2, COL) in project.tags
    assert f"_ns_meta_{COL}" not in project.tags


def test_n5_head_unknown_is_404(sqlite_registry):
    response = _client(sqlite_registry).head(f"/v1/{NS}/namespaces/missing")
    assert response.status_code == 404


def test_delete_unknown_is_404(sqlite_registry):
    response = _client(sqlite_registry).delete(f"/v1/{NS}/namespaces/{COL}")
    assert response.status_code == 404
    assert response.json()["error"]["type"] == "NoSuchNamespaceException"


def test_properties_unknown_is_404(sqlite_registry):
    response = _client(sqlite_registry).post(
        f"/v1/{NS}/namespaces/{COL}/properties",
        json={"updates": {"owner": "uw"}, "removals": []},
    )
    assert response.status_code == 404
    assert response.json()["error"]["type"] == "NoSuchNamespaceException"


def test_n6_get_and_head_default_on_empty_registry(sqlite_registry):
    client = _client(sqlite_registry)
    got = client.get(f"/v1/{NS}/namespaces/{DEFAULT_COLLECTION}")
    assert got.status_code == 200
    assert got.json() == {"namespace": [DEFAULT_COLLECTION], "properties": {}}
    head = client.head(f"/v1/{NS}/namespaces/{DEFAULT_COLLECTION}")
    assert head.status_code == 204


def test_n7_delete_empty_non_default(sqlite_registry):
    client = _client(sqlite_registry)
    client.post(f"/v1/{NS}/namespaces", json={"namespace": [COL]})
    response = client.delete(f"/v1/{NS}/namespaces/{COL}")
    assert response.status_code == 204
    listed = client.get(f"/v1/{NS}/namespaces")
    assert listed.json()["namespaces"] == [[DEFAULT_COLLECTION]]
    project = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert ns_meta_key(NS, COL) not in project.tags
    assert client.get(f"/v1/{NS}/namespaces/{COL}").status_code == 404


def test_n8_delete_with_asset_is_409_not_empty(sqlite_registry):
    client = _client(sqlite_registry)
    client.post(f"/v1/{NS}/namespaces", json={"namespace": [COL]})
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(
            scoped_name(NS, COL, "risk_scores"),
            NS,
            COL,
        ),
        CATALOG_PROJECT,
    )
    response = client.delete(f"/v1/{NS}/namespaces/{COL}")
    assert response.status_code == 409
    error = response.json()["error"]
    assert error["type"] == "NamespaceNotEmptyException"
    assert error["code"] == 409
    assert client.get(f"/v1/{NS}/namespaces/{COL}").status_code == 200


def test_delete_table_only_collection_is_409(sqlite_registry):
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(
            scoped_name(NS, COL, "risk_scores"),
            NS,
            COL,
        ),
        CATALOG_PROJECT,
    )
    client = _client(sqlite_registry)
    response = client.delete(f"/v1/{NS}/namespaces/{COL}")
    assert response.status_code == 409
    assert response.json()["error"]["type"] == "NamespaceNotEmptyException"
    assert client.get(f"/v1/{NS}/namespaces/{COL}").status_code == 200


def test_n9_delete_default_is_400(sqlite_registry):
    response = _client(sqlite_registry).delete(
        f"/v1/{NS}/namespaces/{DEFAULT_COLLECTION}"
    )
    assert response.status_code == 400
    error = response.json()["error"]
    assert error["type"] == "BadRequestException"
    assert "default" in error["message"].lower()


def test_n10_update_properties(sqlite_registry):
    client = _client(sqlite_registry)
    client.post(
        f"/v1/{NS}/namespaces",
        json={
            "namespace": [COL],
            "properties": {"owner": "uw", "deprecated-key": "x"},
        },
    )
    response = client.post(
        f"/v1/{NS}/namespaces/{COL}/properties",
        json={
            "updates": {"owner": "data-team", "status": "active"},
            "removals": ["deprecated-key", "absent"],
        },
    )
    assert response.status_code == 200
    body = response.json()
    assert set(body.keys()) == {"updated", "removed", "missing"}
    assert sorted(body["updated"]) == ["owner", "status"]
    assert body["removed"] == ["deprecated-key"]
    assert body["missing"] == ["absent"]
    got = client.get(f"/v1/{NS}/namespaces/{COL}")
    assert got.json()["properties"] == {
        "owner": "data-team",
        "status": "active",
    }


def test_unmanaged_saved_dataset_does_not_list_or_block_create(sqlite_registry):
    sqlite_registry.apply_saved_dataset(
        SavedDataset(
            name=scoped_name(NS, "ml-leak", "training"),
            features=["fv:feature"],
            join_keys=["entity_id"],
            storage=SavedDatasetFileStorage(path="file:///tmp/dataset.parquet"),
            namespace=NS,
            collection="ml-leak",
        ),
        CATALOG_PROJECT,
    )
    client = _client(sqlite_registry)
    listed = client.get(f"/v1/{NS}/namespaces")
    assert listed.status_code == 200
    assert listed.json()["namespaces"] == [[DEFAULT_COLLECTION]]
    assert client.head(f"/v1/{NS}/namespaces/ml-leak").status_code == 404
    created = client.post(f"/v1/{NS}/namespaces", json={"namespace": ["ml-leak"]})
    assert created.status_code == 200
    assert client.get(f"/v1/{NS}/namespaces").json()["namespaces"] == [
        [DEFAULT_COLLECTION],
        ["ml-leak"],
    ]
    dropped = client.delete(f"/v1/{NS}/namespaces/ml-leak")
    assert dropped.status_code == 204
    assert client.get(f"/v1/{NS}/namespaces").json()["namespaces"] == [
        [DEFAULT_COLLECTION]
    ]


def test_n11_multipart_namespace_is_iceberg_400(sqlite_registry):
    client = _client(sqlite_registry)
    response = client.post(
        f"/v1/{NS}/namespaces",
        json={"namespace": ["a", "b"]},
    )
    assert response.status_code == 400
    body = response.json()
    assert "detail" not in body
    assert body["error"]["type"] == "BadRequestException"

    nested = client.post(
        f"/v1/{NS}/namespaces",
        json={"namespace": [f"a{chr(0x1F)}b"]},
    )
    assert nested.status_code == 400
    assert "detail" not in nested.json()
    assert nested.json()["error"]["type"] == "BadRequestException"


def test_n12_config_advertises_namespace_and_tables(sqlite_registry):
    response = _client(sqlite_registry).get("/v1/config")
    assert response.status_code == 200
    endpoints = response.json()["endpoints"]
    assert endpoints == CATALOG_CONFIG_ENDPOINTS
    for sig in (
        "GET /v1/{prefix}/namespaces",
        "POST /v1/{prefix}/namespaces",
        "GET /v1/{prefix}/namespaces/{namespace}",
        "HEAD /v1/{prefix}/namespaces/{namespace}",
        "DELETE /v1/{prefix}/namespaces/{namespace}",
        "POST /v1/{prefix}/namespaces/{namespace}/properties",
    ):
        assert sig in endpoints
    assert "GET /v1/{prefix}/namespaces/{namespace}/tables" in endpoints
    assert "HEAD /v1/{prefix}/namespaces/{namespace}/tables/{table}" in endpoints
    assert "GET /v1/{prefix}/namespaces/{namespace}/tables/{table}" in endpoints
    assert "POST /v1/{prefix}/tables/rename" not in endpoints
    assert "GET /v1/{prefix}/namespaces/{namespace}/volumes" in endpoints
    assert "GET /v1/projects" not in endpoints
    assert "GET /v1/{prefix}/search" in endpoints
    assert "GET /v1/{prefix}/labels" in endpoints
    assert "POST /v1/{prefix}/labels" in endpoints
    assert "DELETE /v1/{prefix}/labels/{label}" in endpoints


def test_post_default_is_409(sqlite_registry):
    response = _client(sqlite_registry).post(
        f"/v1/{NS}/namespaces",
        json={"namespace": [DEFAULT_COLLECTION]},
    )
    assert response.status_code == 409
    assert response.json()["error"]["type"] == "AlreadyExistsException"


def test_missing_create_body_is_iceberg_400(sqlite_registry):
    response = _client(sqlite_registry).post(f"/v1/{NS}/namespaces", json={})
    assert response.status_code == 400
    body = response.json()
    assert "detail" not in body
    assert body["error"]["type"] == "BadRequestException"
    assert "namespace" in body["error"]["message"]


def _threaded_sqlite_registry():
    fd, registry_path = tempfile.mkstemp()
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


def test_n13_concurrent_create_same_collection_is_200_and_409():
    import threading

    from feast.api.data_catalog.catalog_utils import create_namespace_meta
    from feast.api.data_catalog.errors import NamespaceAlreadyExistsException

    registry, _path = _threaded_sqlite_registry()
    try:
        barrier = threading.Barrier(2)
        outcomes: list[str] = []
        errors: list[BaseException] = []

        def worker():
            barrier.wait()
            try:
                create_namespace_meta(registry, NS, COL, {"owner": "uw"})
                outcomes.append("200")
            except NamespaceAlreadyExistsException:
                outcomes.append("409")
            except BaseException as exc:
                errors.append(exc)

        threads = [threading.Thread(target=worker) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=30)
            assert not thread.is_alive()

        assert not errors, errors
        assert sorted(outcomes) == ["200", "409"]
        project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
        assert ns_meta_key(NS, COL) in project.tags

        client = _client(registry)
        response = client.post(f"/v1/{NS}/namespaces", json={"namespace": [COL]})
        assert response.status_code == 409
        assert response.json()["error"]["type"] == "AlreadyExistsException"
    finally:
        registry.teardown()


def test_n14_concurrent_create_different_collections_both_tags_survive():
    import threading

    from feast.api.data_catalog.catalog_utils import create_namespace_meta

    registry, _path = _threaded_sqlite_registry()
    try:
        col_a = "underwriting"
        col_b = "pricing"
        barrier = threading.Barrier(2)
        outcomes: list[tuple[str, str]] = []
        errors: list[BaseException] = []

        def worker(collection: str):
            barrier.wait()
            try:
                create_namespace_meta(registry, NS, collection, {"owner": collection})
                outcomes.append(("200", collection))
            except BaseException as exc:
                errors.append(exc)

        threads = [
            threading.Thread(target=worker, args=(col_a,)),
            threading.Thread(target=worker, args=(col_b,)),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=30)
            assert not thread.is_alive()

        assert not errors, errors
        assert len(outcomes) == 2
        assert all(status == "200" for status, _ in outcomes)
        assert {c for _, c in outcomes} == {col_a, col_b}
        project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
        assert ns_meta_key(NS, col_a) in project.tags
        assert ns_meta_key(NS, col_b) in project.tags
        listed = _client(registry).get(f"/v1/{NS}/namespaces").json()["namespaces"]
        assert [col_a] in listed
        assert [col_b] in listed
    finally:
        registry.teardown()


def test_n15_concurrent_property_updates_merge_keys():
    import threading

    registry, _path = _threaded_sqlite_registry()
    try:
        client = _client(registry)
        assert (
            client.post(
                f"/v1/{NS}/namespaces",
                json={"namespace": [COL], "properties": {"keep": "1"}},
            ).status_code
            == 200
        )
        barrier = threading.Barrier(2)
        errors: list[BaseException] = []

        def worker(key: str, value: str):
            barrier.wait()
            try:
                response = _client(registry).post(
                    f"/v1/{NS}/namespaces/{COL}/properties",
                    json={"updates": {key: value}, "removals": []},
                )
                assert response.status_code == 200, response.json()
            except BaseException as exc:
                errors.append(exc)

        threads = [
            threading.Thread(target=worker, args=("a", "A")),
            threading.Thread(target=worker, args=("b", "B")),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=30)
            assert not thread.is_alive()
        assert not errors, errors
        got = client.get(f"/v1/{NS}/namespaces/{COL}").json()["properties"]
        assert got["keep"] == "1"
        assert got["a"] == "A"
        assert got["b"] == "B"
    finally:
        registry.teardown()


def test_n16_apply_saved_dataset_does_not_drop_namespace_tag():
    import threading

    from feast.api.data_catalog.catalog_utils import create_namespace_meta
    from feast.api.data_catalog.errors import NamespaceAlreadyExistsException

    for _ in range(10):
        registry, _path = _threaded_sqlite_registry()
        try:
            barrier = threading.Barrier(2)
            created: list[str] = []
            errors: list[BaseException] = []

            def create_ns():
                barrier.wait()
                try:
                    create_namespace_meta(registry, NS, COL, {"owner": "uw"})
                    created.append("ok")
                except NamespaceAlreadyExistsException:
                    created.append("exists")
                except BaseException as exc:
                    errors.append(exc)

            def apply_table():
                barrier.wait()
                try:
                    registry.apply_saved_dataset(
                        _make_saved_dataset(
                            scoped_name(NS, COL, "risk_scores"),
                            NS,
                            COL,
                        ),
                        CATALOG_PROJECT,
                    )
                except BaseException as exc:
                    errors.append(exc)

            threads = [
                threading.Thread(target=create_ns),
                threading.Thread(target=apply_table),
            ]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join(timeout=30)
                assert not thread.is_alive()
            assert not errors, errors
            if "ok" in created:
                project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
                assert ns_meta_key(NS, COL) in project.tags
        finally:
            registry.teardown()


def test_n17_delete_namespace_meta_with_assets_is_409():
    from feast.api.data_catalog.catalog_utils import (
        create_namespace_meta,
        delete_namespace_meta,
    )
    from feast.api.data_catalog.errors import NamespaceNotEmptyException

    registry, _path = _threaded_sqlite_registry()
    try:
        create_namespace_meta(registry, NS, COL, {"owner": "uw"})
        registry.apply_saved_dataset(
            _make_saved_dataset(
                scoped_name(NS, COL, "risk_scores"),
                NS,
                COL,
            ),
            CATALOG_PROJECT,
        )
        with pytest.raises(NamespaceNotEmptyException):
            delete_namespace_meta(registry, NS, COL)
        project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
        assert ns_meta_key(NS, COL) in project.tags
    finally:
        registry.teardown()


def test_n18_concurrent_deletes_are_204_and_404():
    import threading

    from feast.api.data_catalog.catalog_utils import (
        create_namespace_meta,
        delete_namespace_meta,
    )
    from feast.api.data_catalog.errors import NoSuchNamespaceException

    registry, _path = _threaded_sqlite_registry()
    try:
        create_namespace_meta(registry, NS, COL, {"owner": "uw"})
        barrier = threading.Barrier(2)
        outcomes: list[str] = []
        errors: list[BaseException] = []

        def worker():
            barrier.wait()
            try:
                delete_namespace_meta(registry, NS, COL)
                outcomes.append("204")
            except NoSuchNamespaceException:
                outcomes.append("404")
            except BaseException as exc:
                errors.append(exc)

        threads = [threading.Thread(target=worker) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=30)
            assert not thread.is_alive()
        assert not errors, errors
        assert sorted(outcomes) == ["204", "404"]
        project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
        assert ns_meta_key(NS, COL) not in project.tags
    finally:
        registry.teardown()


def test_n19_concurrent_delete_and_properties_does_not_undelete():
    import threading

    from feast.api.data_catalog.catalog_utils import (
        create_namespace_meta,
        delete_namespace_meta,
        merge_namespace_properties,
    )
    from feast.api.data_catalog.errors import NoSuchNamespaceException

    for _ in range(20):
        registry, _path = _threaded_sqlite_registry()
        try:
            create_namespace_meta(registry, NS, COL, {"owner": "uw"})
            barrier = threading.Barrier(2)
            delete_status: list[str] = []
            props_status: list[str] = []
            errors: list[BaseException] = []

            def drop():
                barrier.wait()
                try:
                    delete_namespace_meta(registry, NS, COL)
                    delete_status.append("204")
                except NoSuchNamespaceException:
                    delete_status.append("404")
                except BaseException as exc:
                    errors.append(exc)

            def props():
                barrier.wait()
                try:
                    merge_namespace_properties(registry, NS, COL, {"a": "A"}, [])
                    props_status.append("200")
                except NoSuchNamespaceException:
                    props_status.append("404")
                except BaseException as exc:
                    errors.append(exc)

            threads = [
                threading.Thread(target=drop),
                threading.Thread(target=props),
            ]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join(timeout=30)
                assert not thread.is_alive()
            assert not errors, errors
            assert delete_status and props_status
            project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
            assert ns_meta_key(NS, COL) not in project.tags
            listed = _client(registry).get(f"/v1/{NS}/namespaces").json()["namespaces"]
            assert [COL] not in listed
        finally:
            registry.teardown()


def test_n20_concurrent_delete_of_nonempty_stays_409():
    import threading

    from feast.api.data_catalog.catalog_utils import (
        create_namespace_meta,
        delete_namespace_meta,
    )
    from feast.api.data_catalog.errors import NamespaceNotEmptyException

    for _ in range(20):
        registry, _path = _threaded_sqlite_registry()
        try:
            create_namespace_meta(registry, NS, COL, {"owner": "uw"})
            registry.apply_saved_dataset(
                _make_saved_dataset(
                    scoped_name(NS, COL, "risk_scores"),
                    NS,
                    COL,
                ),
                CATALOG_PROJECT,
            )
            barrier = threading.Barrier(2)
            outcomes: list[str] = []
            errors: list[BaseException] = []

            def drop():
                barrier.wait()
                try:
                    delete_namespace_meta(registry, NS, COL)
                    outcomes.append("204")
                except NamespaceNotEmptyException:
                    outcomes.append("409")
                except BaseException as exc:
                    errors.append(exc)

            def other_drop():
                barrier.wait()
                try:
                    delete_namespace_meta(registry, NS, COL)
                    outcomes.append("204")
                except NamespaceNotEmptyException:
                    outcomes.append("409")
                except BaseException as exc:
                    errors.append(exc)

            threads = [
                threading.Thread(target=drop),
                threading.Thread(target=other_drop),
            ]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join(timeout=30)
                assert not thread.is_alive()
            assert not errors, errors
            assert outcomes == ["409", "409"]
            project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
            assert ns_meta_key(NS, COL) in project.tags
        finally:
            registry.teardown()
