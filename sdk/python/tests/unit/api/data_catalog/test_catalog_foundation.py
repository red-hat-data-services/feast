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

from feast.api.data_catalog import (
    CATALOG_PROJECT,
    DEFAULT_COLLECTION,
    ensure_catalog_project,
    list_collections,
    ns_meta_key,
    resolve_namespace,
    scoped_name,
    set_namespace_properties,
    validate_namespace_exists,
)
from feast.api.data_catalog.catalog_utils import (
    create_namespace_meta,
    delete_namespace_meta,
)
from feast.api.data_catalog.errors import NoSuchNamespaceException
from feast.errors import ProjectObjectNotFoundException
from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig
from feast.saved_dataset import SavedDataset


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


def test_ensure_catalog_project_creates_once(sqlite_registry):
    with pytest.raises(ProjectObjectNotFoundException):
        sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)

    created = ensure_catalog_project(sqlite_registry)
    assert created.name == CATALOG_PROJECT
    first = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    ts = first.last_updated_timestamp

    again = ensure_catalog_project(sqlite_registry)
    assert again.name == CATALOG_PROJECT
    second = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert second.last_updated_timestamp == ts

    names = [p.name for p in sqlite_registry.list_projects(allow_cache=False)]
    assert CATALOG_PROJECT in names
    assert "demo-user-1" not in names
    assert names.count(CATALOG_PROJECT) == 1


def test_resolve_namespace_accepts_string_and_single_element_list():
    assert resolve_namespace("underwriting") == "underwriting"
    assert resolve_namespace(["underwriting"]) == "underwriting"


@pytest.mark.parametrize(
    "raw",
    [
        [],
        ["a", "b"],
        f"a{chr(0x1F)}b",
        " underwriting ",
    ],
)
def test_resolve_namespace_rejects_invalid(raw):
    with pytest.raises(ValueError):
        resolve_namespace(raw)


def test_list_collections_empty_registry_has_default(sqlite_registry):
    ensure_catalog_project(sqlite_registry)
    assert list_collections(sqlite_registry, "demo-user-1") == [DEFAULT_COLLECTION]


def test_list_collections_isolates_tenants(sqlite_registry):
    ensure_catalog_project(sqlite_registry)
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(
            scoped_name("demo-user-1", "underwriting", "risk_scores"),
            "demo-user-1",
            "underwriting",
        ),
        CATALOG_PROJECT,
    )
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(
            scoped_name("demo-user-2", "underwriting", "risk_scores"),
            "demo-user-2",
            "underwriting",
        ),
        CATALOG_PROJECT,
    )
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(
            scoped_name("demo-user-2", "ingestion", "raw_events"),
            "demo-user-2",
            "ingestion",
        ),
        CATALOG_PROJECT,
    )

    assert list_collections(sqlite_registry, "demo-user-1") == [
        DEFAULT_COLLECTION,
        "underwriting",
    ]
    assert list_collections(sqlite_registry, "demo-user-2") == [
        DEFAULT_COLLECTION,
        "ingestion",
        "underwriting",
    ]


def test_validate_namespace_exists_default_with_no_rows(sqlite_registry):
    ensure_catalog_project(sqlite_registry)
    assert validate_namespace_exists(sqlite_registry, "demo-user-1", DEFAULT_COLLECTION)


def test_validate_namespace_exists_scoped_collection_is_per_tenant(sqlite_registry):
    ensure_catalog_project(sqlite_registry)
    assert not validate_namespace_exists(sqlite_registry, "demo-user-1", "underwriting")
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(
            scoped_name("demo-user-1", "underwriting", "risk_scores"),
            "demo-user-1",
            "underwriting",
        ),
        CATALOG_PROJECT,
    )
    assert validate_namespace_exists(sqlite_registry, "demo-user-1", "underwriting")
    assert not validate_namespace_exists(sqlite_registry, "demo-user-2", "underwriting")


def test_list_and_exists_see_scoped_ns_meta_tag(sqlite_registry):
    ensure_catalog_project(sqlite_registry)
    create_namespace_meta(
        sqlite_registry, "demo-user-1", "underwriting", {"owner": "uw"}
    )
    assert list_collections(sqlite_registry, "demo-user-1") == [
        DEFAULT_COLLECTION,
        "underwriting",
    ]
    assert validate_namespace_exists(sqlite_registry, "demo-user-1", "underwriting")
    assert not validate_namespace_exists(sqlite_registry, "demo-user-2", "underwriting")
    project = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert ns_meta_key("demo-user-1", "underwriting") in project.tags
    assert "_ns_meta_underwriting" not in project.tags


def test_unmanaged_saved_dataset_is_not_an_iceberg_namespace(sqlite_registry):
    ensure_catalog_project(sqlite_registry)
    sqlite_registry.apply_saved_dataset(
        SavedDataset(
            name=scoped_name("demo-user-1", "ml-leak", "training"),
            features=["fv:feature"],
            join_keys=["entity_id"],
            storage=SavedDatasetFileStorage(path="file:///tmp/dataset.parquet"),
            namespace="demo-user-1",
            collection="ml-leak",
        ),
        CATALOG_PROJECT,
    )
    assert list_collections(sqlite_registry, "demo-user-1") == [DEFAULT_COLLECTION]
    assert not validate_namespace_exists(sqlite_registry, "demo-user-1", "ml-leak")


def test_set_namespace_properties_requires_existing_collection(sqlite_registry):
    ensure_catalog_project(sqlite_registry)
    with pytest.raises(NoSuchNamespaceException):
        set_namespace_properties(
            sqlite_registry, "demo-user-1", "underwriting", {"owner": "uw"}
        )
    create_namespace_meta(
        sqlite_registry, "demo-user-1", "underwriting", {"owner": "uw"}
    )
    set_namespace_properties(
        sqlite_registry, "demo-user-1", "underwriting", {"owner": "data-team"}
    )
    project = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert (
        project.tags[ns_meta_key("demo-user-1", "underwriting")]
        == '{"owner":"data-team"}'
    )


def test_set_namespace_properties_after_delete_does_not_undelete(sqlite_registry):
    ensure_catalog_project(sqlite_registry)
    create_namespace_meta(
        sqlite_registry, "demo-user-1", "underwriting", {"owner": "uw"}
    )
    delete_namespace_meta(sqlite_registry, "demo-user-1", "underwriting")
    with pytest.raises(NoSuchNamespaceException):
        set_namespace_properties(
            sqlite_registry, "demo-user-1", "underwriting", {"owner": "resurrect"}
        )
    assert not validate_namespace_exists(sqlite_registry, "demo-user-1", "underwriting")
    project = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert ns_meta_key("demo-user-1", "underwriting") not in project.tags
