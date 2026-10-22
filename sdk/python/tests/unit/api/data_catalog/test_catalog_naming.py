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

from feast.api.data_catalog import parse_scoped_name, scoped_name, unscoped_name
from feast.api.data_catalog.catalog_utils import MAX_SCOPED_NAME, SCOPE_SEP
from feast.errors import SavedDatasetNotFound
from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig
from feast.saved_dataset import SavedDataset

CATALOG_PROJECT = "data-registry"


@pytest.fixture
def sqlite_registry():
    fd, registry_path = tempfile.mkstemp()
    registry = SqlRegistry(
        SqlRegistryConfig(
            registry_type="sql",
            path=f"sqlite:///{registry_path}",
            purge_feast_metadata=False,
        ),
        CATALOG_PROJECT,
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


def test_scoped_name_round_trip():
    scoped = scoped_name("demo-user-1", "underwriting", "risk_scores")
    assert scoped == "demo-user-1/underwriting/risk_scores"
    assert parse_scoped_name(scoped) == (
        "demo-user-1",
        "underwriting",
        "risk_scores",
    )
    assert unscoped_name(scoped) == "risk_scores"


@pytest.mark.parametrize(
    "namespace,collection,name",
    [
        ("", "underwriting", "risk_scores"),
        ("demo-user-1", "", "risk_scores"),
        ("demo-user-1", "underwriting", ""),
        ("  ", "underwriting", "risk_scores"),
        ("demo-user-1", "under/writing", "risk_scores"),
        ("demo-user-1", "underwriting", "risk/scores"),
        ("a/b", "underwriting", "risk_scores"),
    ],
)
def test_scoped_name_rejects_empty_or_separator(namespace, collection, name):
    with pytest.raises(ValueError):
        scoped_name(namespace, collection, name)


def test_scoped_name_rejects_overlong():
    # 3 parts + 2 separators; each part sized so join exceeds VARCHAR(255).
    part = "a" * 85
    with pytest.raises(ValueError, match="255"):
        scoped_name(part, part, part)
    assert len(SCOPE_SEP.join((part, part, part))) > MAX_SCOPED_NAME


def test_unscoped_name_never_contains_separator():
    display = unscoped_name(scoped_name("ns1", "raw", "events"))
    assert SCOPE_SEP not in display
    assert display == "events"


def test_unscoped_apply_collides_across_namespaces(sqlite_registry):
    """Guard: CRUD must not apply the Iceberg display name as SavedDataset.name."""
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset("risk_scores", "demo-user-1", "underwriting"),
        CATALOG_PROJECT,
    )
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset("risk_scores", "demo-user-2", "underwriting"),
        CATALOG_PROJECT,
    )
    rows = sqlite_registry.list_saved_datasets(project=CATALOG_PROJECT)
    assert len(rows) == 1
    assert rows[0].namespace == "demo-user-2"


def test_scoped_apply_isolates_namespaces(sqlite_registry):
    name_a = scoped_name("demo-user-1", "underwriting", "risk_scores")
    name_b = scoped_name("demo-user-2", "underwriting", "risk_scores")
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(name_a, "demo-user-1", "underwriting"),
        CATALOG_PROJECT,
    )
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(name_b, "demo-user-2", "underwriting"),
        CATALOG_PROJECT,
    )

    all_rows = sqlite_registry.list_saved_datasets(project=CATALOG_PROJECT)
    assert sorted(d.name for d in all_rows) == [name_a, name_b]

    ns1 = sqlite_registry.list_saved_datasets(
        project=CATALOG_PROJECT, namespace="demo-user-1"
    )
    assert [d.name for d in ns1] == [name_a]
    assert unscoped_name(ns1[0].name) == "risk_scores"

    ns2 = sqlite_registry.list_saved_datasets(
        project=CATALOG_PROJECT, namespace="demo-user-2"
    )
    assert [d.name for d in ns2] == [name_b]

    with pytest.raises(SavedDatasetNotFound):
        sqlite_registry.get_saved_dataset("risk_scores", CATALOG_PROJECT)

    loaded = sqlite_registry.get_saved_dataset(name_a, CATALOG_PROJECT)
    assert loaded.namespace == "demo-user-1"
    assert loaded.collection == "underwriting"


def test_scoped_apply_isolates_collections_in_one_namespace(sqlite_registry):
    raw = scoped_name("demo-user-1", "raw", "risk_scores")
    curated = scoped_name("demo-user-1", "curated", "risk_scores")
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(raw, "demo-user-1", "raw"),
        CATALOG_PROJECT,
    )
    sqlite_registry.apply_saved_dataset(
        _make_saved_dataset(curated, "demo-user-1", "curated"),
        CATALOG_PROJECT,
    )
    rows = sqlite_registry.list_saved_datasets(
        project=CATALOG_PROJECT, namespace="demo-user-1"
    )
    assert sorted(d.name for d in rows) == [curated, raw]
    assert (
        sqlite_registry.list_saved_datasets(
            project=CATALOG_PROJECT, namespace="demo-user-1", collection="raw"
        )[0].name
        == raw
    )


@pytest.mark.parametrize(
    "namespace,collection,name",
    [
        ("demo-user-1", "underwriting", " auto-claims "),
        ("demo-user-1", " underwriting", "risk_scores"),
        (" demo-user-1", "underwriting", "risk_scores"),
        ("demo-user-1", "underwriting", "\trisk_scores"),
        ("demo-user-1", "under writing", "risk_scores"),
    ],
)
def test_scoped_name_rejects_whitespace(namespace, collection, name):
    with pytest.raises(ValueError, match="whitespace"):
        scoped_name(namespace, collection, name)
    with pytest.raises(ValueError, match="whitespace"):
        parse_scoped_name(f"{namespace}/{collection}/{name}")


def test_scoped_name_rejects_leading_trailing_vs_internal_whitespace():
    with pytest.raises(ValueError, match="leading/trailing whitespace"):
        scoped_name("demo-user-1", "underwriting", " auto-claims ")
    with pytest.raises(ValueError, match="internal whitespace"):
        scoped_name("demo-user-1", "under writing", "risk_scores")


def test_scoped_name_accepts_exact_display_name():
    assert (
        scoped_name("demo-user-1", "raw", "auto-claims")
        == "demo-user-1/raw/auto-claims"
    )


def test_scoped_name_rejects_mixed_case_namespace():
    with pytest.raises(ValueError, match="lowercase"):
        scoped_name("Demo-user", "raw", "t")
    with pytest.raises(ValueError, match="lowercase"):
        parse_scoped_name("Demo-user/raw/t")


def test_scoped_name_preserves_collection_and_table_case():
    scoped = scoped_name("demo-user", "Raw", "Risk_Scores")
    assert scoped == "demo-user/Raw/Risk_Scores"
    assert parse_scoped_name(scoped) == ("demo-user", "Raw", "Risk_Scores")
    assert unscoped_name(scoped) == "Risk_Scores"
