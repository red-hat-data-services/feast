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

"""Catalog SavedDataset insert/get/delete for RHOAI extension routes.

Registry bookkeeping only. Does not open object storage or run engines.
"""

from __future__ import annotations

import json
import uuid
from datetime import datetime, timezone
from inspect import signature
from typing import Any

from pydantic import TypeAdapter

from feast.api.data_catalog.catalog_utils import (
    CATALOG_MANAGED_TAG,
    CATALOG_MANAGED_VALUE,
    CATALOG_PROJECT,
    ensure_catalog_project,
    scoped_name,
    unscoped_name,
)
from feast.api.data_catalog.errors import (
    AlreadyExistsException,
    BadRequestException,
    ServiceFailureException,
)
from feast.api.data_catalog.models import AssetResponse, ConnectionRef, SchemaField
from feast.errors import SavedDatasetAlreadyExists, SavedDatasetNotFound
from feast.infra.offline_stores.file_source import SavedDatasetFileStorage
from feast.infra.registry.base_registry import BaseRegistry
from feast.saved_dataset import SavedDataset, SavedDatasetColumn

_PLACEHOLDER_FEATURES = ["fv:feature"]
_PLACEHOLDER_JOIN_KEYS = ["entity_id"]

RESERVED_TAGS = {
    CATALOG_MANAGED_TAG,
    "asset_type",
    "format",
    "volume_type",
    "owner",
    "registered_by",
    "updated_by",
    "uuid",
    "_labels",
    "_connection_ref",
    "comment",
    "purpose",
    "license",
    "maturity",
    "domain",
    "pii",
    "created_at",
    "updated_at",
    "created-at",
    "updated-at",
}

GOVERNANCE_KEYS = frozenset({"purpose", "license", "maturity", "domain", "pii"})

_MAX_LABELS_JSON = 10_000
_MAX_LABEL_COUNT = 1_000
_MAX_LABEL_LEN = 255
_MAX_CONNECTION_REF_JSON = 4_096
_CONNECTION_REF_ADAPTER: TypeAdapter[ConnectionRef] = TypeAdapter(ConnectionRef)

STRUCTURED_FORMATS = frozenset(
    {"iceberg", "parquet", "csv", "delta", "postgresql", "milvus", "other"}
)
UNSTRUCTURED_FORMATS = frozenset(
    {"documents", "images", "audio", "video", "binary", "other"}
)


def isoformat_ts(value: datetime | None) -> str | None:
    if value is None:
        return None
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def epoch_ms(value: datetime | None) -> int:
    """Return epoch milliseconds, treating naive datetimes as UTC."""
    if value is None:
        return 0
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    return int(value.timestamp() * 1000)


def storage_uri(dataset: SavedDataset) -> str:
    storage = dataset.storage
    file_options = getattr(storage, "file_options", None)
    if file_options is not None:
        return file_options.uri or ""
    return ""


def labels_from_tags(tags: dict[str, str]) -> list[str] | None:
    raw = tags.get("_labels")
    if not raw or len(raw) > _MAX_LABELS_JSON:
        return None
    try:
        parsed = json.loads(raw)
    except (json.JSONDecodeError, ValueError, RecursionError):
        return None
    if not isinstance(parsed, list) or len(parsed) > _MAX_LABEL_COUNT:
        return None
    if not all(
        isinstance(item, str) and len(item) <= _MAX_LABEL_LEN for item in parsed
    ):
        return None
    return parsed


def labels_to_tag(labels: list[str] | None) -> dict[str, str]:
    if not labels:
        return {}
    if len(labels) > _MAX_LABEL_COUNT:
        raise ValueError(f"labels must have at most {_MAX_LABEL_COUNT} entries")
    for item in labels:
        if not isinstance(item, str) or len(item) > _MAX_LABEL_LEN:
            raise ValueError(
                f"each label must be a string of at most {_MAX_LABEL_LEN} characters"
            )
        if "/" in item:
            raise ValueError(f"label must not contain '/' (got {item!r})")
    encoded = json.dumps(list(labels))
    if len(encoded) > _MAX_LABELS_JSON:
        raise ValueError(f"labels JSON must be at most {_MAX_LABELS_JSON} characters")
    return {"_labels": encoded}


def connection_ref_from_tags(tags: dict[str, str]) -> ConnectionRef | None:
    raw = tags.get("_connection_ref")
    if not raw or len(raw) > _MAX_CONNECTION_REF_JSON:
        return None
    try:
        return _CONNECTION_REF_ADAPTER.validate_json(raw)
    except (ValueError, TypeError, json.JSONDecodeError):
        return None


def connection_ref_to_tag(ref: ConnectionRef | None) -> dict[str, str]:
    if ref is None:
        return {}
    stable_fields = {"type", "id"} if ref.type == "dch" else {"type", "secret_name"}
    encoded = ref.model_dump_json(include=stable_fields)
    if len(encoded) > _MAX_CONNECTION_REF_JSON:
        raise ValueError("connection_ref is too large")
    return {"_connection_ref": encoded}


def merge_labels(
    tags: dict[str, str],
    *,
    add: list[str] | None = None,
    remove: list[str] | None = None,
) -> dict[str, str]:
    current = list(labels_from_tags(tags) or [])
    if add:
        for item in add:
            if item not in current:
                current.append(item)
    if remove:
        drop = set(remove)
        current = [item for item in current if item not in drop]
    updated = dict(tags)
    updated.pop("_labels", None)
    updated.update(labels_to_tag(current or None))
    return updated


def public_properties(tags: dict[str, str]) -> dict[str, str]:
    return {key: value for key, value in tags.items() if key not in RESERVED_TAGS}


def notes_from_properties(properties: dict[str, str] | None) -> dict[str, str]:
    """User ``properties`` with catalog reserved keys stripped."""
    if not properties:
        return {}
    return {key: value for key, value in properties.items() if key not in RESERVED_TAGS}


def merge_public_properties(
    tags: dict[str, str], properties: dict[str, str]
) -> dict[str, str]:
    """Merge user properties into tags. Omitted keys remain unchanged."""
    updated = dict(tags)
    for key, value in notes_from_properties(properties).items():
        updated[key] = value
    return updated


def remove_custom_properties(
    tags: dict[str, str],
    keys_to_remove: list[str],
) -> dict[str, str]:
    """Remove user-specified keys from tags. Rejects governance keys."""
    updated = dict(tags)
    for key in keys_to_remove:
        if key in GOVERNANCE_KEYS:
            raise BadRequestException(
                f"Cannot remove governance property '{key}'. "
                "Use the top-level field to update it."
            )
        if key in RESERVED_TAGS:
            continue
        updated.pop(key, None)
    return updated


def validate_property_overlap(
    properties: dict[str, str] | None,
    remove_properties: list[str] | None,
) -> None:
    """400 if the same key appears in both properties and remove_properties."""
    if not properties or not remove_properties:
        return
    overlap = sorted(set(properties) & set(remove_properties))
    if overlap:
        raise BadRequestException(
            "Cannot set and remove the same property: " + ", ".join(overlap)
        )


def insert_catalog_dataset(
    registry: BaseRegistry,
    *,
    rhai_ns: str,
    collection: str,
    display_name: str,
    location: str,
    tags: dict[str, str],
    description: str = "",
    columns: list[SavedDatasetColumn] | None = None,
) -> SavedDataset:
    """Insert a catalog-managed SavedDataset. Duplicate → 409.

    Sequential: exists-check. Concurrent: SqlRegistry ``on_conflict='raise'``.
    Does not change Feast's default IntegrityError skip on ``feast apply``.
    """
    ensure_catalog_project(registry)
    name = scoped_name(rhai_ns, collection, display_name)
    try:
        registry.get_saved_dataset(name, CATALOG_PROJECT, allow_cache=False)
    except SavedDatasetNotFound:
        pass
    else:
        raise AlreadyExistsException(
            f"Asset already exists: {collection}.{display_name}"
        )
    stamped = {
        **tags,
        CATALOG_MANAGED_TAG: CATALOG_MANAGED_VALUE,
        "uuid": str(uuid.uuid4()),
    }
    dataset = SavedDataset(
        name=name,
        features=list(_PLACEHOLDER_FEATURES),
        join_keys=list(_PLACEHOLDER_JOIN_KEYS),
        storage=SavedDatasetFileStorage(path=location),
        namespace=rhai_ns,
        collection=collection,
        description=description,
        columns=columns or [],
        tags=stamped,
    )
    apply_kwargs: dict[str, Any] = {}
    try:
        if "on_conflict" in signature(registry.apply_saved_dataset).parameters:
            apply_kwargs["on_conflict"] = "raise"
    except (TypeError, ValueError):
        pass
    try:
        registry.apply_saved_dataset(dataset, CATALOG_PROJECT, **apply_kwargs)
    except SavedDatasetAlreadyExists as exc:
        raise AlreadyExistsException(
            f"Asset already exists: {collection}.{display_name}"
        ) from exc
    return registry.get_saved_dataset(name, CATALOG_PROJECT, allow_cache=False)


def get_catalog_dataset(
    registry: BaseRegistry, rhai_ns: str, collection: str, display_name: str
) -> SavedDataset | None:
    name = scoped_name(rhai_ns, collection, display_name)
    try:
        dataset = registry.get_saved_dataset(name, CATALOG_PROJECT, allow_cache=False)
    except SavedDatasetNotFound:
        return None
    if dataset.namespace != rhai_ns or dataset.collection != collection:
        return None
    tags = dataset.tags or {}
    if tags.get(CATALOG_MANAGED_TAG) != CATALOG_MANAGED_VALUE:
        return None
    return dataset


def list_catalog_datasets(
    registry: BaseRegistry,
    rhai_ns: str,
    collection: str,
    *,
    asset_type: str,
) -> list[SavedDataset]:
    rows: list[SavedDataset] = []
    for dataset in registry.list_saved_datasets(
        CATALOG_PROJECT,
        namespace=rhai_ns,
        collection=collection,
        tags={CATALOG_MANAGED_TAG: CATALOG_MANAGED_VALUE},
    ):
        if (dataset.tags or {}).get("asset_type") != asset_type:
            continue
        try:
            unscoped_name(dataset.name)
        except ValueError:
            continue
        rows.append(dataset)
    return rows


def replace_catalog_dataset(
    registry: BaseRegistry, dataset: SavedDataset
) -> SavedDataset:
    registry.apply_saved_dataset(dataset, CATALOG_PROJECT)
    return registry.get_saved_dataset(dataset.name, CATALOG_PROJECT, allow_cache=False)


def delete_catalog_dataset(
    registry: BaseRegistry, rhai_ns: str, collection: str, display_name: str
) -> bool:
    dataset = get_catalog_dataset(registry, rhai_ns, collection, display_name)
    if dataset is None:
        return False
    registry.delete_saved_dataset(dataset.name, CATALOG_PROJECT)
    return True


def owner_from_identity(
    x_remote_user: str | None,
    x_user: str | None,
    kubeflow_userid: str | None,
) -> str:
    ident = (
        (x_remote_user or "").strip()
        or (x_user or "").strip()
        or (kubeflow_userid or "").strip()
    )
    if not ident:
        raise BadRequestException(
            "Owner cannot be determined from authenticated identity"
        )
    return ident


def structured_format_from_tags(tags: dict[str, str]) -> str:
    fmt = tags.get("format")
    if not fmt or fmt not in STRUCTURED_FORMATS:
        raise BadRequestException("table asset has invalid or missing format")
    return fmt


def unstructured_format_from_tags(tags: dict[str, str]) -> str:
    fmt = tags.get("format")
    if not fmt or fmt not in UNSTRUCTURED_FORMATS:
        raise BadRequestException("volume asset has invalid or missing format")
    return fmt


def catalog_asset_response(dataset: SavedDataset, collection: str) -> AssetResponse:
    tags = dataset.tags or {}
    asset_type = tags.get("asset_type") or "table"
    if asset_type == "volume":
        format_value = unstructured_format_from_tags(tags)
    else:
        format_value = structured_format_from_tags(tags)
    owner = (tags.get("owner") or "").strip()
    if not owner:
        raise BadRequestException("asset missing owner")
    uuid = tags.get("uuid")
    if not uuid:
        raise ServiceFailureException("asset missing uuid")
    columns: list[SchemaField] | None
    if asset_type == "table":
        columns = [
            SchemaField(
                name=column.name,
                type=column.type,
                description=column.description or "",
                nullable=column.nullable,
            )
            for column in (dataset.columns or [])
        ]
        if not columns:
            columns = None
    else:
        columns = None
    props = public_properties(tags)
    for key in ("purpose", "license", "maturity", "domain", "pii"):
        if tags.get(key):
            props[key] = tags[key]
    created_at = isoformat_ts(dataset.created_timestamp)
    updated_at = isoformat_ts(dataset.last_updated_timestamp)
    if not created_at and not updated_at:
        raise ServiceFailureException("asset missing timestamps")
    return AssetResponse(
        name=unscoped_name(dataset.name),
        asset_type=asset_type,
        uuid=uuid,
        format=format_value,
        storage_location=storage_uri(dataset) or None,
        columns=columns,
        collection=collection,
        connection_ref=connection_ref_from_tags(tags),
        owner=owner,
        description=dataset.description or None,
        labels=labels_from_tags(tags),
        properties=props or None,
        created_at=created_at or updated_at or "",
        updated_at=updated_at or created_at or "",
    )


def schema_fields_to_columns(fields: list[Any] | None) -> list[SavedDatasetColumn]:
    if not fields:
        return []
    columns: list[SavedDatasetColumn] = []
    for field in fields:
        if hasattr(field, "name"):
            name = field.name
            type_name = getattr(field, "type", "") or ""
            description = getattr(field, "description", "") or ""
            nullable = getattr(field, "nullable", True)
            if nullable is None:
                nullable = True
        elif isinstance(field, dict):
            name = str(field.get("name") or "")
            type_name = str(field.get("type") or "")
            description = str(field.get("description") or "")
            nullable = field.get("nullable", True)
            if nullable is None:
                nullable = True
        else:
            raise ValueError("schema_fields entries must be objects with name and type")
        if not name:
            raise ValueError("schema_fields[].name is required")
        if not type_name:
            raise ValueError("schema_fields[].type is required")
        columns.append(
            SavedDatasetColumn(
                name=name,
                type=type_name,
                description=description,
                nullable=bool(nullable),
            )
        )
    return columns
