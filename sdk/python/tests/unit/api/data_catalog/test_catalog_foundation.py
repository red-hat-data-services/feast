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
from feast.api.data_catalog.errors import (
    NoSuchNamespaceException,
    ServiceFailureException,
)
from feast.api.registry.rest.data_catalog_integration import (
    sync_registry_project_protection,
)
from feast.constants import PROTECTED_PROJECT_TAG
from feast.errors import ProjectObjectNotFoundException
from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig
from feast.project import Project
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


def test_ensure_catalog_project_tags_protected(sqlite_registry):
    project = ensure_catalog_project(sqlite_registry)
    assert project.tags.get(PROTECTED_PROJECT_TAG) == "true"
    stored = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert stored.tags.get(PROTECTED_PROJECT_TAG) == "true"


def test_ensure_catalog_project_merges_protected_tag(sqlite_registry, monkeypatch):
    """Existing namespace tags survive, and the write goes through mutate_project."""
    meta_key = ns_meta_key("team", "billing")
    sqlite_registry.apply_project(
        Project(
            name=CATALOG_PROJECT,
            description="RHOAI Data Registry",
            tags={meta_key: "{}"},
        )
    )
    calls = {"apply": 0, "mutate": 0}
    real_apply = sqlite_registry.apply_project
    real_mutate = sqlite_registry.mutate_project

    def apply_wrapper(*args, **kwargs):
        calls["apply"] += 1
        return real_apply(*args, **kwargs)

    def mutate_wrapper(*args, **kwargs):
        calls["mutate"] += 1
        return real_mutate(*args, **kwargs)

    monkeypatch.setattr(sqlite_registry, "apply_project", apply_wrapper)
    monkeypatch.setattr(sqlite_registry, "mutate_project", mutate_wrapper)

    project = ensure_catalog_project(sqlite_registry)
    assert calls["mutate"] == 1
    assert calls["apply"] == 0
    assert project.tags[meta_key] == "{}"
    assert project.tags[PROTECTED_PROJECT_TAG] == "true"

    stored = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    ts = stored.last_updated_timestamp
    again = ensure_catalog_project(sqlite_registry)
    assert again.tags[meta_key] == "{}"
    assert again.tags[PROTECTED_PROJECT_TAG] == "true"
    assert calls["mutate"] == 1
    assert calls["apply"] == 0
    reread = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert reread.last_updated_timestamp == ts
    assert reread.tags[meta_key] == "{}"


def test_data_registry_startup_protects_catalog_project_only(
    sqlite_registry, monkeypatch
):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    # This env would hide spec.feastProject if startup still ran that sync.
    monkeypatch.setenv("FEAST_PROTECTED_PROJECT", "true")
    meta_key = ns_meta_key("team", "billing")
    sqlite_registry.apply_project(
        Project(
            name=CATALOG_PROJECT,
            description="RHOAI Data Registry",
            tags={meta_key: "{}"},
        )
    )

    class _Store:
        project = "catalog_mode"
        registry = sqlite_registry

    sync_registry_project_protection(_Store())
    stored = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert stored.tags[PROTECTED_PROJECT_TAG] == "true"
    assert stored.tags[meta_key] == "{}"
    with pytest.raises(ProjectObjectNotFoundException):
        sqlite_registry.get_project("catalog_mode", allow_cache=False)


@pytest.mark.parametrize("protected_env", ["true", None])
def test_data_registry_startup_clears_stale_feast_project_protection(
    sqlite_registry, monkeypatch, protected_env
):
    """Upgrade and protected-FeatureStore transition.

    GET /projects hides any project whose feast.dev/protected-project tag is
    "true". Data Registry mode must drop that tag from spec.feastProject even
    when FEAST_PROTECTED_PROJECT is still set, and must keep the catalog
    project protected.
    """
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    if protected_env is None:
        monkeypatch.delenv("FEAST_PROTECTED_PROJECT", raising=False)
    else:
        monkeypatch.setenv("FEAST_PROTECTED_PROJECT", protected_env)
    meta_key = ns_meta_key("team", "billing")
    sqlite_registry.apply_project(
        Project(
            name=CATALOG_PROJECT,
            description="RHOAI Data Registry",
            tags={meta_key: "{}"},
        )
    )
    sqlite_registry.apply_project(
        Project(
            name="credit_scoring",
            description="user project",
            owner="platform",
            tags={PROTECTED_PROJECT_TAG: "true", "owner": "team"},
        )
    )

    class _Store:
        project = "credit_scoring"
        registry = sqlite_registry

    sync_registry_project_protection(_Store())
    sync_registry_project_protection(_Store())

    feast_project = sqlite_registry.get_project("credit_scoring", allow_cache=False)
    assert PROTECTED_PROJECT_TAG not in feast_project.tags
    assert feast_project.tags == {"owner": "team"}
    assert feast_project.description == "user project"
    assert feast_project.owner == "platform"
    catalog = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert catalog.tags[PROTECTED_PROJECT_TAG] == "true"
    assert catalog.tags[meta_key] == "{}"
    visible = [
        project.name
        for project in (
            sqlite_registry.get_project("credit_scoring", allow_cache=False),
            catalog,
        )
        if project.tags.get(PROTECTED_PROJECT_TAG) != "true"
    ]
    assert visible == ["credit_scoring"]


def test_data_registry_startup_keeps_tags_written_after_protection_read(
    sqlite_registry, monkeypatch
):
    """A tag written after the guard read must survive the protection clear.

    mutate_project re-reads the row. apply_project of the guard snapshot would
    drop the tag added in between.
    """
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    sqlite_registry.apply_project(
        Project(
            name="credit_scoring",
            description="user project",
            owner="platform",
            tags={PROTECTED_PROJECT_TAG: "true", "owner": "team"},
        )
    )
    real_get = sqlite_registry.get_project
    injected = {"done": False}

    def get_project(name, allow_cache=False):
        project = real_get(name, allow_cache=allow_cache)
        if name != "credit_scoring" or injected["done"]:
            return project
        injected["done"] = True
        stale_tags = dict(project.tags)
        current = real_get(name, allow_cache=False)
        current.tags = {**current.tags, "fresh": "yes"}
        sqlite_registry.apply_project(current, commit=True)
        project.tags = stale_tags
        return project

    monkeypatch.setattr(sqlite_registry, "get_project", get_project)

    class _Store:
        project = "credit_scoring"
        registry = sqlite_registry

    sync_registry_project_protection(_Store())
    assert injected["done"]
    stored = real_get("credit_scoring", allow_cache=False)
    assert PROTECTED_PROJECT_TAG not in stored.tags
    assert stored.tags == {"owner": "team", "fresh": "yes"}
    assert stored.description == "user project"
    assert stored.owner == "platform"


class _RegistryWithoutProjectMutation:
    """Registry that can read and replace a project, but cannot merge a write."""

    def __init__(self, registry):
        self._registry = registry

    def get_project(self, name, allow_cache=False):
        return self._registry.get_project(name, allow_cache=allow_cache)

    def apply_project(self, project, commit=True):
        return self._registry.apply_project(project, commit=commit)


def test_data_registry_startup_refuses_snapshot_clear_without_compare_and_swap(
    sqlite_registry, monkeypatch
):
    """Do not apply the guard snapshot when concurrent tag writes cannot merge."""
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    monkeypatch.setenv("FEAST_PROTECTED_PROJECT", "true")
    sqlite_registry.apply_project(
        Project(
            name="credit_scoring",
            description="user project",
            tags={PROTECTED_PROJECT_TAG: "true", "owner": "team"},
        )
    )

    class _Store:
        project = "credit_scoring"
        registry = _RegistryWithoutProjectMutation(sqlite_registry)

    with pytest.raises(ServiceFailureException, match="atomic project mutation"):
        sync_registry_project_protection(_Store())

    stored = sqlite_registry.get_project("credit_scoring", allow_cache=False)
    assert stored.tags == {PROTECTED_PROJECT_TAG: "true", "owner": "team"}
    assert stored.description == "user project"
    catalog = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert catalog.tags[PROTECTED_PROJECT_TAG] == "true"


def test_data_registry_startup_without_compare_and_swap_leaves_unprotected_project(
    sqlite_registry, monkeypatch
):
    """Missing mutate_project is an error only when a stale tag must be cleared."""
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    sqlite_registry.apply_project(
        Project(
            name="credit_scoring",
            description="kept",
            tags={"owner": "team"},
        )
    )

    class _Store:
        project = "credit_scoring"
        registry = _RegistryWithoutProjectMutation(sqlite_registry)

    sync_registry_project_protection(_Store())
    stored = sqlite_registry.get_project("credit_scoring", allow_cache=False)
    assert stored.tags == {"owner": "team"}
    assert stored.description == "kept"


def test_data_registry_startup_does_not_unprotect_catalog_project(
    sqlite_registry, monkeypatch
):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    monkeypatch.setenv("FEAST_PROTECTED_PROJECT", "true")
    meta_key = ns_meta_key("team", "billing")
    sqlite_registry.apply_project(
        Project(
            name=CATALOG_PROJECT,
            description="RHOAI Data Registry",
            tags={PROTECTED_PROJECT_TAG: "true", meta_key: "{}"},
        )
    )

    class _Store:
        project = CATALOG_PROJECT
        registry = sqlite_registry

    sync_registry_project_protection(_Store())
    stored = sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)
    assert stored.tags[PROTECTED_PROJECT_TAG] == "true"
    assert stored.tags[meta_key] == "{}"
    assert stored.description == "RHOAI Data Registry"


def test_data_registry_startup_leaves_unprotected_feast_project_unchanged(
    sqlite_registry, monkeypatch
):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    monkeypatch.setenv("FEAST_PROTECTED_PROJECT", "true")
    sqlite_registry.apply_project(
        Project(
            name="credit_scoring",
            description="kept",
            tags={"owner": "team"},
        )
    )
    before = sqlite_registry.get_project("credit_scoring", allow_cache=False)

    class _Store:
        project = "credit_scoring"
        registry = sqlite_registry

    sync_registry_project_protection(_Store())
    after = sqlite_registry.get_project("credit_scoring", allow_cache=False)
    assert after.tags == {"owner": "team"}
    assert after.description == "kept"
    assert after.last_updated_timestamp == before.last_updated_timestamp


def test_registry_startup_still_protects_feast_project_outside_data_registry(
    sqlite_registry, monkeypatch
):
    monkeypatch.delenv("DATA_REGISTRY_ENABLED", raising=False)
    monkeypatch.setenv("FEAST_PROTECTED_PROJECT", "true")

    class _Store:
        project = "credit_scoring"
        registry = sqlite_registry

    sync_registry_project_protection(_Store())
    stored = sqlite_registry.get_project("credit_scoring", allow_cache=False)
    assert stored.tags[PROTECTED_PROJECT_TAG] == "true"
    with pytest.raises(ProjectObjectNotFoundException):
        sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)


def test_registry_startup_skips_catalog_project_when_disabled(
    sqlite_registry, monkeypatch
):
    monkeypatch.delenv("DATA_REGISTRY_ENABLED", raising=False)
    monkeypatch.delenv("FEAST_PROTECTED_PROJECT", raising=False)

    class _Store:
        project = "catalog_mode"
        registry = sqlite_registry

    sync_registry_project_protection(_Store())
    with pytest.raises(ProjectObjectNotFoundException):
        sqlite_registry.get_project(CATALOG_PROJECT, allow_cache=False)


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
