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

"""Data Registry full-text search (RHAI-377).

``GET /v1/{project}/search`` searches catalog tables, volumes, and collections
within one RHOAI / Kubernetes namespace (``SavedDataset.namespace``). This is
not Feast feature-registry ``GET /search``.
"""

from __future__ import annotations

from fastapi import APIRouter, Query, Request

from feast.api.data_catalog.catalog_assets import (
    labels_from_tags,
    public_properties,
    storage_uri,
)
from feast.api.data_catalog.catalog_utils import (
    _catalog_saved_datasets,
    _registry,
    _require_namespace,
    get_namespace_properties,
    list_collections,
    unscoped_name,
)
from feast.api.data_catalog.errors import BadRequestException
from feast.api.data_catalog.models import SearchResponse, SearchResult
from feast.api.registry.rest.rest_utils import paginate_and_sort
from feast.saved_dataset import SavedDataset

_MAX_LIMIT = 500
_DEFAULT_LIMIT = 50

_SEARCHABLE_FIRST_CLASS = (
    "format",
    "owner",
    "purpose",
    "license",
    "maturity",
    "domain",
    "pii",
)

_RESULT_NOTE_KEYS = ("purpose", "license", "maturity", "domain", "pii")


def _rhai_ns(project: str) -> str:
    try:
        return _require_namespace(project)
    except ValueError as exc:
        raise BadRequestException(str(exc)) from exc


def _compute_match_score(
    query: str, name: str, description: str, values: list[str]
) -> int:
    q = query.strip().lower()
    if not q:
        return 0
    name_l = name.lower()
    if q == name_l:
        return 100
    if q in name_l:
        return 90
    if description and q in description.lower():
        return 80
    for val in values:
        if q in str(val).lower():
            return 60
    if len(q) >= 3:
        q_set = set(q)
        n_set = set(name_l)
        denom = max(len(q_set), len(n_set))
        if denom and (len(q_set & n_set) / denom) >= 0.75:
            return 40
    return 0


def _searchable_values(dataset: SavedDataset, tags: dict[str, str]) -> list[str]:
    values: list[str] = list(public_properties(tags).values())
    for key in _SEARCHABLE_FIRST_CLASS:
        val = tags.get(key)
        if val:
            values.append(val)
    values.extend(labels_from_tags(tags) or [])
    uri = storage_uri(dataset)
    if uri:
        values.append(uri)
    for column in dataset.columns or []:
        if column.name:
            values.append(column.name)
        if column.type:
            values.append(column.type)
        if column.description:
            values.append(column.description)
    return values


def _result_properties(tags: dict[str, str]) -> dict[str, str]:
    props = dict(public_properties(tags))
    for key in _RESULT_NOTE_KEYS:
        val = tags.get(key)
        if val:
            props[key] = val
    return props


def _filter_tag_map(tags: dict[str, str]) -> dict[str, str]:
    """Keys that ``properties=key:value`` may match, including ``format``."""
    out = dict(public_properties(tags))
    for key in _SEARCHABLE_FIRST_CLASS:
        val = tags.get(key)
        if val:
            out[key] = val
    return out


def _keep_by_type(
    *,
    effective_type: str | None,
    result_type: str,
    format_tag: str | None,
) -> bool:
    if not effective_type:
        return True
    if effective_type == "dataset":
        effective_type = "table"
    if effective_type == "iceberg_table":
        return result_type == "table" and (format_tag or "").casefold() == "iceberg"
    if effective_type in {"table", "volume", "collection"}:
        return result_type == effective_type
    return False


def _parse_property_filters(raw: list[str] | None) -> list[tuple[str, str]]:
    out: list[tuple[str, str]] = []
    if not raw:
        return out
    for item in raw:
        if ":" not in item:
            raise BadRequestException(
                f"Invalid properties filter: '{item}'. Expected key:value"
            )
        key, value = item.split(":", 1)
        key = key.strip()
        value = value.strip()
        if not key:
            raise BadRequestException(
                f"Invalid properties filter: '{item}'. Key must not be empty"
            )
        if not value:
            raise BadRequestException(
                f"Invalid properties filter: '{item}'. Value must not be empty"
            )
        out.append((key, value))
    return out


def _matches_properties(
    tag_map: dict[str, str], filters: list[tuple[str, str]]
) -> bool:
    folded = {key.casefold(): value for key, value in tag_map.items()}
    for key, value in filters:
        actual = folded.get(key.casefold())
        if actual is None or value.casefold() not in str(actual).casefold():
            return False
    return True


def _label_matches(tags: dict[str, str], label: str) -> bool:
    needle = label.casefold()
    labs = labels_from_tags(tags) or []
    return any(needle in item.casefold() for item in labs)


def _effective_limit(page_size: int | None, limit: int) -> int:
    return page_size if page_size is not None else limit


def _reject_invalid_page(page: int, pagination: dict) -> None:
    total = pagination.get("totalCount", 0) or 0
    total_pages = pagination.get("totalPages", 0) or 0
    if total == 0:
        if page > 1:
            raise BadRequestException(
                f"Invalid page parameter: '{page}'. No results to paginate"
            )
        return
    if page > total_pages:
        raise BadRequestException(
            f"Invalid page parameter: '{page}'. Must be less than or equal to {total_pages}"
        )


def get_search_router() -> APIRouter:
    router = APIRouter(tags=["search"])

    @router.get(
        "/v1/{project}/search",
        response_model=SearchResponse,
        operation_id="searchDataRegistry",
    )
    def search_catalog(
        project: str,
        request: Request,
        query: str = Query(default=""),
        namespace: str | None = Query(default=None),
        namespaces: list[str] | None = Query(default=None),
        asset_type: str | None = Query(default=None),
        properties: list[str] | None = Query(default=None),
        label: str | None = Query(default=None),
        sort_by: str = Query(default="score"),
        sort_order: str | None = Query(default=None),
        page: int = Query(default=1, ge=1),
        page_size: int | None = Query(default=None, ge=1, le=_MAX_LIMIT),
        limit: int = Query(default=_DEFAULT_LIMIT, ge=1, le=_MAX_LIMIT),
    ) -> SearchResponse:
        rhai_ns = _rhai_ns(project)
        registry = _registry(request)
        effective_type = (asset_type or "").strip().lower() or None
        effective_collections: list[str] = [
            item.strip() for item in (namespaces or []) if item and item.strip()
        ]
        if namespace and namespace.strip():
            ns = namespace.strip()
            if ns.casefold() not in {c.casefold() for c in effective_collections}:
                effective_collections.append(ns)
        collection_fold = {c.casefold() for c in effective_collections}
        include_collections = len(effective_collections) == 0
        prop_filters = _parse_property_filters(properties)
        q_empty = query.strip() == ""
        hits: list[dict] = []

        if include_collections:
            for col in list_collections(registry, rhai_ns):
                if label:
                    continue
                props = get_namespace_properties(registry, rhai_ns, col)
                desc = props.get("description") or ""
                result_props = {k: v for k, v in props.items() if k != "description"}
                if prop_filters and not _matches_properties(result_props, prop_filters):
                    continue
                if not _keep_by_type(
                    effective_type=effective_type,
                    result_type="collection",
                    format_tag=None,
                ):
                    continue
                score = _compute_match_score(
                    query, col, desc, list(result_props.values())
                )
                if not q_empty and score == 0:
                    continue
                hits.append(
                    {
                        "type": "collection",
                        "namespace": [col],
                        "name": col,
                        "description": desc or None,
                        "properties": result_props,
                        "score": score,
                    }
                )

        for dataset in _catalog_saved_datasets(registry, rhai_ns):
            collection = dataset.collection or "default"
            if collection_fold and collection.casefold() not in collection_fold:
                continue
            try:
                display = unscoped_name(dataset.name)
            except ValueError:
                continue
            tags = dataset.tags or {}
            result_type = tags.get("asset_type") or "table"
            if result_type not in {"table", "volume"}:
                continue
            if not _keep_by_type(
                effective_type=effective_type,
                result_type=result_type,
                format_tag=tags.get("format"),
            ):
                continue
            if label and not _label_matches(tags, label):
                continue
            if prop_filters and not _matches_properties(
                _filter_tag_map(tags), prop_filters
            ):
                continue
            score = _compute_match_score(
                query,
                display,
                dataset.description or "",
                _searchable_values(dataset, tags),
            )
            if not q_empty and score == 0:
                continue
            hits.append(
                {
                    "type": result_type,
                    "namespace": [collection],
                    "name": display,
                    "description": dataset.description or None,
                    "properties": _result_properties(tags),
                    "score": score,
                }
            )

        sort_key = (sort_by or "score").strip().lower()
        if sort_key not in {"score", "name"}:
            raise BadRequestException(
                f"Invalid sort_by parameter: '{sort_by}'. Valid options are: ['score', 'name']"
            )
        if sort_order is None or not str(sort_order).strip():
            effective_order = "desc" if sort_key == "score" else "asc"
        else:
            effective_order = str(sort_order).strip().lower()
            if effective_order not in {"asc", "desc"}:
                raise BadRequestException(
                    f"Invalid sort_order parameter: '{sort_order}'. Valid options are: ['asc', 'desc']"
                )
        effective_limit = _effective_limit(page_size, limit)
        for hit in hits:
            hit["_sort_name"] = hit["name"].casefold()
        helper_sort = "_sort_name" if sort_key == "name" else "score"
        paged, pagination = paginate_and_sort(
            items=hits,
            page=page,
            limit=effective_limit,
            sort_by=helper_sort,
            sort_order=effective_order,
        )
        _reject_invalid_page(page, pagination)
        results = []
        for row in paged:
            row.pop("_sort_name", None)
            results.append(SearchResult(**row))
        return SearchResponse(query=query, results=results, pagination=pagination)

    return router
