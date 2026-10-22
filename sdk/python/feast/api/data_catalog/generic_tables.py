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

"""RHOAI generic-table catalog routes (RHAI-376).

Register tables of any format (including iceberg) as SavedDataset rows.
Iceberg REST POST /tables stays 501 (warehouse create). This route is
catalog metadata only.
"""

from __future__ import annotations

from fastapi import APIRouter, Header, Query, Request, Response

from feast.api.data_catalog.catalog_assets import (
    catalog_asset_response,
    connection_ref_to_tag,
    delete_catalog_dataset,
    get_catalog_dataset,
    insert_catalog_dataset,
    labels_to_tag,
    list_catalog_datasets,
    merge_labels,
    merge_public_properties,
    notes_from_properties,
    owner_from_identity,
    remove_custom_properties,
    replace_catalog_dataset,
    schema_fields_to_columns,
    validate_property_overlap,
)
from feast.api.data_catalog.catalog_utils import (
    _registry,
    _require_namespace,
    _require_part,
    resolve_namespace,
    validate_namespace_exists,
)
from feast.api.data_catalog.errors import (
    BadRequestException,
    NoSuchNamespaceException,
    NoSuchTableException,
)
from feast.api.data_catalog.models import (
    AssetListResponse,
    AssetResponse,
    CreateGenericTableRequest,
    UpdateGenericTableRequest,
)
from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
from feast.infra.registry.base_registry import BaseRegistry
from feast.saved_dataset import SavedDataset


def _as_bad_request(exc: ValueError) -> BadRequestException:
    return BadRequestException(str(exc))


def _rhai_ns(project: str) -> str:
    try:
        return _require_namespace(project)
    except ValueError as exc:
        raise _as_bad_request(exc) from exc


def _collection_name(collection: str) -> str:
    try:
        return resolve_namespace(collection)
    except ValueError as exc:
        raise _as_bad_request(exc) from exc


def _display_name(table: str) -> str:
    try:
        return _require_part("name", table)
    except ValueError as exc:
        raise _as_bad_request(exc) from exc


def _require_collection(registry: BaseRegistry, rhai_ns: str, collection: str) -> None:
    if not validate_namespace_exists(registry, rhai_ns, collection):
        raise NoSuchNamespaceException(f"Namespace does not exist: {collection}")


def _get_table(
    registry: BaseRegistry, rhai_ns: str, collection: str, table: str
) -> SavedDataset:
    dataset = get_catalog_dataset(registry, rhai_ns, collection, table)
    if dataset is None or (dataset.tags or {}).get("asset_type") != "table":
        raise NoSuchTableException(f"Table does not exist: {collection}.{table}")
    return dataset


def get_generic_table_router() -> APIRouter:
    router = APIRouter(tags=["generic-tables"])

    @router.get(
        "/v1/{project}/namespaces/{collection}/generic-tables",
        response_model=AssetListResponse,
        response_model_exclude_unset=True,
    )
    def list_generic_tables(
        project: str,
        collection: str,
        request: Request,
        label: str | None = Query(default=None),
    ) -> AssetListResponse:
        rhai_ns = _rhai_ns(project)
        col = _collection_name(collection)
        registry = _registry(request)
        _require_collection(registry, rhai_ns, col)
        assets = [
            catalog_asset_response(dataset, col)
            for dataset in list_catalog_datasets(
                registry, rhai_ns, col, asset_type="table"
            )
        ]
        if label:
            assets = [
                asset for asset in assets if asset.labels and label in asset.labels
            ]
        return AssetListResponse(assets=assets)

    @router.post(
        "/v1/{project}/namespaces/{collection}/generic-tables",
        response_model=AssetResponse,
        response_model_exclude_unset=True,
        status_code=201,
    )
    def create_generic_table(
        project: str,
        collection: str,
        body: CreateGenericTableRequest,
        request: Request,
        x_remote_user: str | None = Header(default=None, alias="X-Remote-User"),
        x_user: str | None = Header(default=None, alias="X-User"),
        kubeflow_userid: str | None = Header(default=None, alias="kubeflow-userid"),
    ) -> AssetResponse:
        rhai_ns = _rhai_ns(project)
        col = _collection_name(collection)
        registry = _registry(request)
        _require_collection(registry, rhai_ns, col)
        display = _display_name(body.name)
        try:
            label_tags = labels_to_tag(body.labels)
            ref_tags = connection_ref_to_tag(body.connection_ref)
            columns = schema_fields_to_columns(body.schema_fields)
        except ValueError as exc:
            raise _as_bad_request(exc) from exc
        tags = {
            **notes_from_properties(body.properties),
            **label_tags,
            **ref_tags,
            "asset_type": "table",
            "format": body.format,
            "owner": owner_from_identity(x_remote_user, x_user, kubeflow_userid),
        }
        for key in ("purpose", "license", "maturity", "domain", "pii"):
            value = getattr(body, key)
            if value:
                tags[key] = value
        dataset = insert_catalog_dataset(
            registry,
            rhai_ns=rhai_ns,
            collection=col,
            display_name=display,
            location=(body.storage_location or "").strip(),
            tags=tags,
            description=body.description or "",
            columns=columns,
        )
        return catalog_asset_response(dataset, col)

    @router.get(
        "/v1/{project}/namespaces/{collection}/generic-tables/{table}",
        response_model=AssetResponse,
        response_model_exclude_unset=True,
    )
    def get_generic_table(
        project: str, collection: str, table: str, request: Request
    ) -> AssetResponse:
        rhai_ns = _rhai_ns(project)
        col = _collection_name(collection)
        registry = _registry(request)
        _require_collection(registry, rhai_ns, col)
        dataset = _get_table(registry, rhai_ns, col, _display_name(table))
        return catalog_asset_response(dataset, col)

    @router.patch(
        "/v1/{project}/namespaces/{collection}/generic-tables/{table}",
        response_model=AssetResponse,
        response_model_exclude_unset=True,
    )
    def update_generic_table(
        project: str,
        collection: str,
        table: str,
        body: UpdateGenericTableRequest,
        request: Request,
    ) -> AssetResponse:
        rhai_ns = _rhai_ns(project)
        col = _collection_name(collection)
        registry = _registry(request)
        _require_collection(registry, rhai_ns, col)
        dataset = _get_table(registry, rhai_ns, col, _display_name(table))
        tags = dict(dataset.tags or {})
        if body.format is not None:
            tags["format"] = body.format
        if "connection_ref" in body.model_fields_set:
            tags.pop("_connection_ref", None)
            try:
                tags.update(connection_ref_to_tag(body.connection_ref))
            except ValueError as exc:
                raise _as_bad_request(exc) from exc
        if body.add_labels or body.remove_labels:
            try:
                tags = merge_labels(
                    tags, add=body.add_labels, remove=body.remove_labels
                )
            except ValueError as exc:
                raise _as_bad_request(exc) from exc
        if body.properties is not None or body.remove_properties:
            validate_property_overlap(body.properties, body.remove_properties)
        if body.properties is not None:
            tags = merge_public_properties(tags, body.properties)
        if body.remove_properties:
            tags = remove_custom_properties(tags, body.remove_properties)
        for key in ("purpose", "license", "maturity", "domain", "pii"):
            if key in body.model_fields_set:
                value = getattr(body, key)
                if value is None:
                    tags.pop(key, None)
                else:
                    tags[key] = value
        if body.description is not None:
            dataset.description = body.description
        if "storage_location" in body.model_fields_set:
            path = body.storage_location or ""
            dataset.storage = SavedDatasetFileStorage(path=path)
        if body.schema_fields is not None:
            try:
                dataset.columns = schema_fields_to_columns(body.schema_fields)
            except ValueError as exc:
                raise _as_bad_request(exc) from exc
        dataset.tags = tags
        updated = replace_catalog_dataset(registry, dataset)
        return catalog_asset_response(updated, col)

    @router.delete(
        "/v1/{project}/namespaces/{collection}/generic-tables/{table}",
        status_code=204,
    )
    def delete_generic_table(
        project: str, collection: str, table: str, request: Request
    ) -> Response:
        rhai_ns = _rhai_ns(project)
        col = _collection_name(collection)
        registry = _registry(request)
        _require_collection(registry, rhai_ns, col)
        display = _display_name(table)
        _get_table(registry, rhai_ns, col, display)
        delete_catalog_dataset(registry, rhai_ns, col, display)
        return Response(status_code=204)

    return router
