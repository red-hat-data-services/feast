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

"""Tests for Data Registry search (GET /v1/{project}/search)."""

import tempfile

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from feast import FeatureStore
from feast.api.data_catalog.config import CATALOG_CONFIG_ENDPOINTS, get_config_router
from feast.api.data_catalog.errors import register_error_handlers
from feast.api.data_catalog.generic_tables import get_generic_table_router
from feast.api.data_catalog.labels import get_label_router
from feast.api.data_catalog.namespaces import get_namespace_router
from feast.api.data_catalog.search import _compute_match_score, get_search_router
from feast.api.data_catalog.tables import get_table_router
from feast.api.data_catalog.volumes import get_volume_router
from feast.api.registry.rest.rest_registry_server import RestRegistryServer
from feast.infra.registry.sql import SqlRegistry, SqlRegistryConfig
from feast.repo_config import RepoConfig

NS = "demo-user-1"
COL = "underwriting"
OTHER = "other-user"
EMPTY_NS = "empty-tenant-9"
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
    app.include_router(get_label_router())
    app.include_router(get_search_router())
    return TestClient(app, raise_server_exceptions=False)


def _ensure_collection(client: TestClient, project: str = NS, collection: str = COL):
    client.post(
        f"/v1/{project}/namespaces",
        json={"namespace": [collection], "properties": {}},
    )


def _search(client, project=NS, **params):
    return client.get(f"/v1/{project}/search", params=params)


def _seed_underwriting(client):
    client.post(
        f"/v1/{NS}/namespaces",
        json={"namespace": [COL], "properties": {"description": "UW collection"}},
    )
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={
                "name": "auto-claims",
                "format": "iceberg",
                "description": "underwriting claims table",
                "properties": {"team": "risk"},
                "purpose": "fraud",
                "labels": ["pii"],
            },
            headers=AUTH,
        ).status_code
        == 201
    )
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/volumes",
            json={
                "name": "claims-pdfs",
                "format": "documents",
                "storage_location": "s3://bucket/docs/",
                "description": "raw pdf dump",
            },
            headers=AUTH,
        ).status_code
        == 200
    )


def _assert_no_flat_pagination(body: dict) -> None:
    assert "total" not in body
    assert "page" not in body
    assert "limit" not in body
    assert "searched_projects" not in body
    assert "projects_searched" not in body
    assert "pagination" in body
    for row in body["results"]:
        assert "project" not in row


@pytest.mark.parametrize(
    "query,name,description,values,expected",
    [
        ("", "auto-claims", "hello", ["iceberg"], 0),
        ("  ", "auto-claims", "hello", [], 0),
        ("auto-claims", "auto-claims", "x", [], 100),
        ("AUTO-CLAIMS", "auto-claims", "x", [], 100),
        ("claims", "auto-claims", "x", [], 90),
        ("underwriting", "auto-claims", "underwriting claims table", [], 80),
        ("iceberg", "auto-claims", "nope", ["iceberg"], 60),
        ("format", "zzz", "nope", ["iceberg"], 0),
        ("acb", "abcx", "nope", [], 40),
        ("no-such", "auto-claims", "hello", ["iceberg"], 0),
    ],
)
def test_compute_match_score_tiers(query, name, description, values, expected):
    score = _compute_match_score(query, name, description, values)
    assert score == expected
    assert isinstance(score, int)


def test_empty_query_includes_seeded_rows(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    _assert_no_flat_pagination(body)
    names = {r["name"] for r in body["results"]}
    assert {"auto-claims", "claims-pdfs", "underwriting", "default"}.issubset(names)
    assert body["pagination"]["totalCount"] == len(body["results"])
    assert all(r["score"] == 0 for r in body["results"])
    assert all(isinstance(r["score"], int) for r in body["results"])


def test_does_not_leak_other_rhoai_project(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    _ensure_collection(client, OTHER, COL)
    assert (
        client.post(
            f"/v1/{OTHER}/namespaces/{COL}/generic-tables",
            json={"name": "secret-table", "format": "parquet"},
            headers=AUTH,
        ).status_code
        == 201
    )
    ns_body = _search(client).json()
    assert "secret-table" not in {r["name"] for r in ns_body["results"]}
    other_body = _search(client, project=OTHER).json()
    other_names = {r["name"] for r in other_body["results"]}
    assert "secret-table" in other_names
    assert "auto-claims" not in other_names


def test_search_hit_properties_purpose_not_format(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="auto-claims")
    assert resp.status_code == 200, resp.text
    row = next(r for r in resp.json()["results"] if r["name"] == "auto-claims")
    assert row["properties"]["team"] == "risk"
    assert row["properties"]["purpose"] == "fraud"
    assert "format" not in row["properties"]


def test_empty_rhoai_project_returns_default_collection_only(sqlite_registry):
    """list_collections always includes ``default`` (plan-locked)."""
    client = _client(sqlite_registry)
    resp = _search(client, project=EMPTY_NS)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["results"] == [
        {
            "type": "collection",
            "namespace": ["default"],
            "name": "default",
            "description": None,
            "properties": {},
            "score": 0,
        }
    ]


def test_score_40_fuzzy_over_http(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={"name": "abcx", "format": "parquet", "description": "nope"},
            headers=AUTH,
        ).status_code
        == 201
    )
    resp = _search(client, query="acb", asset_type="table")
    assert resp.status_code == 200, resp.text
    row = next(r for r in resp.json()["results"] if r["name"] == "abcx")
    assert row["score"] == 40


def test_namespaces_filters_multiple_collections(sqlite_registry):
    client = _client(sqlite_registry)
    other_col = "claims"
    _ensure_collection(client, collection=COL)
    _ensure_collection(client, collection=other_col)
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={"name": "in-underwriting", "format": "parquet"},
            headers=AUTH,
        ).status_code
        == 201
    )
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{other_col}/generic-tables",
            json={"name": "in-claims", "format": "parquet"},
            headers=AUTH,
        ).status_code
        == 201
    )
    resp = _search(client, namespaces=[COL, other_col])
    assert resp.status_code == 200, resp.text
    names = {r["name"] for r in resp.json()["results"]}
    assert names == {"in-underwriting", "in-claims"}
    assert not any(r["type"] == "collection" for r in resp.json()["results"])


def test_mixed_400_bodies_sort_vs_pagination(sqlite_registry):
    """All 400s are Iceberg ``{error:…}`` (unified, locked 2026-09-24)."""
    client = _client(sqlite_registry)
    sort_resp = _search(client, sort_by="match_score")
    assert sort_resp.status_code == 400
    assert sort_resp.json()["error"]["type"] == "BadRequestException"
    assert "detail" not in sort_resp.json()

    page_resp = _search(client, limit=501)
    assert page_resp.status_code == 400
    assert page_resp.json()["error"]["type"] == "BadRequestException"
    assert "detail" not in page_resp.json()


def test_score_100_exact_name(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="auto-claims")
    assert resp.status_code == 200, resp.text
    row = next(r for r in resp.json()["results"] if r["name"] == "auto-claims")
    assert row["score"] == 100
    assert isinstance(row["score"], int)


def test_score_90_name_substring(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="claims")
    assert resp.status_code == 200, resp.text
    row = next(r for r in resp.json()["results"] if r["name"] == "auto-claims")
    assert row["score"] == 90


def test_score_80_description(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="underwriting claims table")
    assert resp.status_code == 200, resp.text
    row = next(r for r in resp.json()["results"] if r["name"] == "auto-claims")
    assert row["score"] == 80


def test_score_60_value_not_key(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    for q in ("iceberg", "risk", "pii"):
        resp = _search(client, query=q)
        assert resp.status_code == 200, resp.text
        row = next(r for r in resp.json()["results"] if r["name"] == "auto-claims")
        assert row["score"] == 60
    resp = _search(client, query="format")
    assert resp.status_code == 200, resp.text
    assert not any(r["score"] == 60 for r in resp.json()["results"])


def test_no_match_empty_results(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="zzz-no-such-asset")
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["results"] == []
    assert "totalCount" not in body["pagination"]
    assert body["pagination"]["page"] == 1
    assert body["pagination"]["limit"] == 50


def test_internal_tag_true_not_universal_hit(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="true")
    assert resp.status_code == 200, resp.text
    assert resp.json()["results"] == []


def test_namespace_filter_hides_collections(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, namespace=COL)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    names = {r["name"] for r in body["results"]}
    assert "auto-claims" in names
    assert "claims-pdfs" in names
    assert not any(r["type"] == "collection" for r in body["results"])
    assert "default" not in names


def test_asset_type_filters(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={"name": "plain-parquet", "format": "parquet"},
            headers=AUTH,
        ).status_code
        == 201
    )

    vol = _search(client, asset_type="volume").json()
    assert all(r["type"] == "volume" for r in vol["results"])
    assert any(r["name"] == "claims-pdfs" for r in vol["results"])
    assert not any(r["type"] == "collection" for r in vol["results"])

    tables = _search(client, asset_type="table").json()
    assert all(r["type"] == "table" for r in tables["results"])

    dataset = _search(client, asset_type="dataset").json()
    table_names = {r["name"] for r in tables["results"]}
    assert {r["name"] for r in dataset["results"]} == table_names

    iceberg = _search(client, asset_type="iceberg_table").json()
    iceberg_names = {r["name"] for r in iceberg["results"]}
    assert "auto-claims" in iceberg_names
    assert "plain-parquet" not in iceberg_names

    coll = _search(client, asset_type="collection").json()
    assert all(r["type"] == "collection" for r in coll["results"])

    for unknown in ("vector_index", "document_collection"):
        empty = _search(client, asset_type=unknown)
        assert empty.status_code == 200, empty.text
        assert empty.json()["results"] == []


def test_properties_format_iceberg(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={"name": "plain-parquet", "format": "parquet"},
            headers=AUTH,
        ).status_code
        == 201
    )
    resp = _search(client, properties=["format:iceberg"])
    assert resp.status_code == 200, resp.text
    names = {r["name"] for r in resp.json()["results"]}
    assert names == {"auto-claims"}


def test_properties_empty_value_is_400_not_match_all(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, properties=["format:"])
    assert resp.status_code == 400
    assert resp.json()["error"]["type"] == "BadRequestException"


def test_properties_without_colon_is_400_not_silent(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    before = _search(client, asset_type="table").json()["results"]
    resp = _search(client, properties=["iceberg"], asset_type="table")
    assert resp.status_code == 400
    assert resp.json()["error"]["type"] == "BadRequestException"
    assert len(before) > 0


def test_label_filter_is_case_insensitive_substring(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    assert _search(client, label="pii").json()["results"]
    assert _search(client, label="PII").json()["results"]
    names = {r["name"] for r in _search(client, label="ii").json()["results"]}
    assert "auto-claims" in names


def test_iceberg_table_type_matches_format_case_insensitive(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={"name": "upper-fmt", "format": "ICEBERG"},
            headers=AUTH,
        ).status_code
        == 400
    )
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={"name": "lower-fmt", "format": "iceberg"},
            headers=AUTH,
        ).status_code
        == 201
    )
    names = {
        r["name"] for r in _search(client, asset_type="iceberg_table").json()["results"]
    }
    assert "lower-fmt" in names
    assert "upper-fmt" not in names


def test_page_past_end_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    for name in ("aaa-one", "bbb-two", "ccc-three"):
        assert (
            client.post(
                f"/v1/{NS}/namespaces/{COL}/generic-tables",
                json={"name": name, "format": "parquet"},
                headers=AUTH,
            ).status_code
            == 201
        )
    resp = _search(
        client,
        asset_type="table",
        sort_by="name",
        sort_order="asc",
        limit=1,
        page=99,
    )
    assert resp.status_code == 400
    assert resp.json()["error"]["type"] == "BadRequestException"
    assert "detail" not in resp.json()


def test_label_filter(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, label="pii")
    assert resp.status_code == 200, resp.text
    body = resp.json()
    names = {r["name"] for r in body["results"]}
    assert "auto-claims" in names
    assert "claims-pdfs" not in names
    assert not any(r["type"] == "collection" for r in body["results"])


def test_pagination_feast_shape(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    for name in ("aaa-one", "bbb-two", "ccc-three"):
        assert (
            client.post(
                f"/v1/{NS}/namespaces/{COL}/generic-tables",
                json={"name": name, "format": "parquet"},
                headers=AUTH,
            ).status_code
            == 201
        )

    params = {
        "asset_type": "table",
        "sort_by": "name",
        "sort_order": "asc",
        "limit": 1,
        "page": 1,
    }
    resp = _search(client, **params)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert len(body["results"]) == 1
    assert body["results"][0]["name"] == "aaa-one"
    pag = body["pagination"]
    assert pag["page"] == 1
    assert pag["limit"] == 1
    assert pag["totalCount"] == 3
    assert pag["totalPages"] == 3
    assert pag["hasNext"] is True
    assert "hasPrevious" not in pag

    params["page"] = 2
    resp2 = _search(client, **params)
    assert resp2.json()["results"][0]["name"] == "bbb-two"
    pag2 = resp2.json()["pagination"]
    assert pag2["hasPrevious"] is True
    assert pag2["hasNext"] is True

    params["page"] = 3
    resp3 = _search(client, **params)
    assert resp3.json()["results"][0]["name"] == "ccc-three"
    pag3 = resp3.json()["pagination"]
    assert "hasNext" not in pag3
    assert pag3["hasPrevious"] is True

    wide = _search(client, page_size=1, limit=50, asset_type="table")
    assert len(wide.json()["results"]) == 1
    assert wide.json()["pagination"]["limit"] == 1


def test_sort_by_name_and_score(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, sort_by="name", sort_order="asc", asset_type="table")
    assert resp.status_code == 200, resp.text
    names = [r["name"] for r in resp.json()["results"]]
    assert names == sorted(names)

    scored = _search(client, query="auto-claims")
    assert scored.status_code == 200, scored.text
    results = scored.json()["results"]
    assert results[0]["name"] == "auto-claims"
    assert results[0]["score"] == 100


def test_config_lists_search(sqlite_registry):
    client = _client(sqlite_registry)
    resp = client.get("/v1/config")
    assert resp.status_code == 200
    assert "GET /v1/{prefix}/search" in resp.json()["endpoints"]
    assert "GET /v1/{prefix}/search" in CATALOG_CONFIG_ENDPOINTS


def test_search_via_rest_registry_server(tmp_path, monkeypatch):
    monkeypatch.setenv("DATA_REGISTRY_ENABLED", "true")
    registry_path = tmp_path / "registry.db"
    config = RepoConfig.model_validate(
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
    store = FeatureStore(config=config)
    client = TestClient(RestRegistryServer(store).app, raise_server_exceptions=False)
    created = client.post(
        "/v1/demo-user-1/namespaces/default/volumes",
        json={
            "name": "docs",
            "format": "documents",
            "storage_location": "s3://bucket/docs/",
        },
        headers=AUTH,
    )
    assert created.status_code == 200, created.text
    resp = client.get("/v1/demo-user-1/search")
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert "pagination" in body
    names = {r["name"] for r in body["results"]}
    assert "docs" in names
    assert "default" in names


def test_legacy_type_query_is_ignored(sqlite_registry):
    """OpenAPI keeps ``asset_type`` only; leftover ``type=`` is not a filter."""
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, type="volume")
    assert resp.status_code == 200, resp.text
    names = {r["name"] for r in resp.json()["results"]}
    assert "auto-claims" in names
    assert "claims-pdfs" in names


def test_properties_format_filter_is_case_insensitive(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, properties=["Format:ICEBERG"])
    assert resp.status_code == 200, resp.text
    assert {r["name"] for r in resp.json()["results"]} == {"auto-claims"}


def test_collection_filter_is_case_insensitive(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, namespace=COL.upper())
    assert resp.status_code == 200, resp.text
    names = {r["name"] for r in resp.json()["results"]}
    assert "auto-claims" in names
    assert not any(r["type"] == "collection" for r in resp.json()["results"])


def test_sort_by_is_case_insensitive(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="auto-claims", sort_by="SCORE")
    assert resp.status_code == 200, resp.text
    assert resp.json()["results"][0]["name"] == "auto-claims"


def test_sort_by_name_is_case_insensitive(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={"name": "Zebra", "format": "parquet"},
            headers=AUTH,
        ).status_code
        == 201
    )
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={"name": "apple", "format": "parquet"},
            headers=AUTH,
        ).status_code
        == 201
    )
    names = [
        r["name"]
        for r in _search(
            client, asset_type="table", sort_by="name", sort_order="asc"
        ).json()["results"]
    ]
    assert names == ["apple", "Zebra"]


def test_schema_and_storage_uri_are_searchable(sqlite_registry):
    client = _client(sqlite_registry)
    _ensure_collection(client)
    assert (
        client.post(
            f"/v1/{NS}/namespaces/{COL}/generic-tables",
            json={
                "name": "schema-tbl",
                "format": "parquet",
                "description": "nope",
                "storage_location": "s3://warehouse/schema-tbl/",
                "schema_fields": [
                    {
                        "name": "claim_amount",
                        "type": "double",
                        "description": "payout cents",
                    }
                ],
            },
            headers=AUTH,
        ).status_code
        == 201
    )
    for q in ("claim_amount", "double", "payout cents", "s3://warehouse/schema-tbl/"):
        resp = _search(client, query=q, asset_type="table")
        assert resp.status_code == 200, resp.text
        row = next(r for r in resp.json()["results"] if r["name"] == "schema-tbl")
        assert row["score"] == 60


def test_volume_storage_uri_is_searchable(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="s3://bucket/docs/", asset_type="volume")
    assert resp.status_code == 200, resp.text
    row = next(r for r in resp.json()["results"] if r["name"] == "claims-pdfs")
    assert row["score"] == 60


def test_limit_200_is_ok(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, limit=200)
    assert resp.status_code == 200, resp.text
    assert resp.json()["pagination"]["limit"] == 200


def test_empty_results_page_two_is_400(sqlite_registry):
    client = _client(sqlite_registry)
    _seed_underwriting(client)
    resp = _search(client, query="zzz-no-such-asset", page=2)
    assert resp.status_code == 400
    assert resp.json()["error"]["type"] == "BadRequestException"
