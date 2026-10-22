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

"""Iceberg REST table routes.

List, HEAD, and load are data-catalog **read** over Feast SavedDataset rows.
Load returns stored metadata with an empty config map (no credential
vending). Create, update, drop, and rename remain 501 stubs.

Do not mount this router on RestRegistryServer here.
"""

from __future__ import annotations

from fastapi import APIRouter, Request, Response

from feast.api.data_catalog.catalog_assets import epoch_ms, storage_uri
from feast.api.data_catalog.catalog_utils import (
    CATALOG_MANAGED_TAG,
    CATALOG_MANAGED_VALUE,
    CATALOG_PROJECT,
    _http_collection,
    _http_namespace,
    _http_part,
    _registry,
    scoped_name,
    unscoped_name,
    validate_namespace_exists,
)
from feast.api.data_catalog.errors import (
    NoSuchNamespaceException,
    NoSuchTableException,
    NotImplementedException,
)
from feast.api.data_catalog.models import (
    IcebergField,
    IcebergSchema,
    ListTablesResponse,
    LoadTableResponse,
    PartitionSpec,
    TableIdentifier,
    TableMetadata,
)
from feast.errors import SavedDatasetNotFound
from feast.infra.registry.base_registry import BaseRegistry
from feast.saved_dataset import SavedDataset

_TABLE_UNIMPLEMENTED = (
    "This Iceberg table operation is not implemented. "
    "This Data Registry deployment supports list and exists only; engines must use "
    "their own object-store credentials"
)


def _project_and_collection(project: str, collection: str) -> tuple[str, str]:
    return _http_namespace(project), _http_collection(collection)


def _display_name(table: str) -> str:
    return _http_part("name", table)


def _require_collection(registry: BaseRegistry, rhai_ns: str, collection: str) -> None:
    if not validate_namespace_exists(registry, rhai_ns, collection):
        raise NoSuchNamespaceException(f"Namespace does not exist: {collection}")


def _is_iceberg_table(dataset: SavedDataset) -> bool:
    tags = dataset.tags or {}
    return (
        tags.get(CATALOG_MANAGED_TAG) == CATALOG_MANAGED_VALUE
        and tags.get("format") == "iceberg"
        and tags.get("asset_type") == "table"
    )


def _get_iceberg_table(
    registry: BaseRegistry, rhai_ns: str, collection: str, table: str
) -> SavedDataset:
    display = _display_name(table)
    name = scoped_name(rhai_ns, collection, display)
    try:
        dataset = registry.get_saved_dataset(name, CATALOG_PROJECT, allow_cache=False)
    except SavedDatasetNotFound as exc:
        raise NoSuchTableException(
            f"Table does not exist: {collection}.{display}"
        ) from exc
    if dataset.namespace != rhai_ns or dataset.collection != collection:
        raise NoSuchTableException(f"Table does not exist: {collection}.{display}")
    if not _is_iceberg_table(dataset):
        raise NoSuchTableException(f"Table does not exist: {collection}.{display}")
    return dataset


def _load_table_response(dataset: SavedDataset) -> LoadTableResponse:
    """Build a LoadTableResponse from stored SavedDataset data.

    Returns Iceberg-spec-compliant metadata so that ``pyiceberg`` and other
    engines can parse the response without validation errors.  Fields like
    ``format_version``, ``current_schema_id``, ``partition_specs``, and
    ``last_sequence_number`` are hardcoded defaults — appropriate for
    catalog-only registered assets (no managed commits).
    ``config`` is always empty (no credential vending).
    """
    tags = dataset.tags or {}
    iceberg_fields = [
        IcebergField(
            id=idx + 1,
            name=col.name,
            required=not col.nullable,
            type=col.type or "string",
        )
        for idx, col in enumerate(dataset.columns or [])
    ]
    schema = IcebergSchema(fields=iceberg_fields)
    _INTERNAL_KEYS = {CATALOG_MANAGED_TAG, "_labels", "_connection_ref", "uuid"}
    props = {k: v for k, v in tags.items() if k not in _INTERNAL_KEYS}
    raw_ref = tags.get("_connection_ref")
    if raw_ref:
        props["connection_ref"] = raw_ref
    last_updated_ms = epoch_ms(dataset.last_updated_timestamp)
    location = storage_uri(dataset)
    table_uuid = tags.get("uuid") or dataset.name
    metadata = TableMetadata(
        format_version=2,
        table_uuid=table_uuid,
        location=location,
        last_updated_ms=last_updated_ms,
        properties=props,
        schemas=[schema],
        current_schema_id=0,
        partition_specs=[PartitionSpec(spec_id=0, fields=[])],
        last_column_id=len(iceberg_fields),
        last_sequence_number=0,
    )
    return LoadTableResponse(
        metadata_location=location,
        metadata=metadata,
        config={},
    )


def get_table_router() -> APIRouter:
    router = APIRouter(tags=["tables"])

    @router.get(
        "/v1/{project}/namespaces/{collection}/tables",
        response_model=ListTablesResponse,
        response_model_by_alias=True,
    )
    def list_tables(
        project: str, collection: str, request: Request
    ) -> ListTablesResponse:
        rhai_ns, col = _project_and_collection(project, collection)
        registry = _registry(request)
        _require_collection(registry, rhai_ns, col)
        identifiers: list[TableIdentifier] = []
        for dataset in registry.list_saved_datasets(
            CATALOG_PROJECT, namespace=rhai_ns, collection=col
        ):
            if not _is_iceberg_table(dataset):
                continue
            try:
                display = unscoped_name(dataset.name)
            except ValueError:
                continue
            identifiers.append(TableIdentifier(namespace=[col], name=display))
        return ListTablesResponse(identifiers=identifiers)

    @router.post("/v1/{project}/namespaces/{collection}/tables")
    def create_table(project: str, collection: str) -> Response:
        _project_and_collection(project, collection)
        raise NotImplementedException(_TABLE_UNIMPLEMENTED)

    @router.get(
        "/v1/{project}/namespaces/{collection}/tables/{table}",
        response_model=LoadTableResponse,
        response_model_by_alias=True,
    )
    def load_table(
        project: str, collection: str, table: str, request: Request
    ) -> LoadTableResponse:
        rhai_ns, col = _project_and_collection(project, collection)
        registry = _registry(request)
        _require_collection(registry, rhai_ns, col)
        dataset = _get_iceberg_table(registry, rhai_ns, col, table)
        return _load_table_response(dataset)

    @router.head(
        "/v1/{project}/namespaces/{collection}/tables/{table}",
        status_code=204,
    )
    def table_exists(
        project: str, collection: str, table: str, request: Request
    ) -> Response:
        rhai_ns, col = _project_and_collection(project, collection)
        registry = _registry(request)
        _require_collection(registry, rhai_ns, col)
        _get_iceberg_table(registry, rhai_ns, col, table)
        return Response(status_code=204)

    @router.post("/v1/{project}/namespaces/{collection}/tables/{table}")
    def update_table(project: str, collection: str, table: str) -> Response:
        _project_and_collection(project, collection)
        _display_name(table)
        raise NotImplementedException(_TABLE_UNIMPLEMENTED)

    @router.delete("/v1/{project}/namespaces/{collection}/tables/{table}")
    def drop_table(project: str, collection: str, table: str) -> Response:
        _project_and_collection(project, collection)
        _display_name(table)
        raise NotImplementedException(_TABLE_UNIMPLEMENTED)

    @router.post("/v1/{project}/tables/rename")
    def rename_table(project: str) -> Response:
        _http_namespace(project)
        raise NotImplementedException(_TABLE_UNIMPLEMENTED)

    return router
