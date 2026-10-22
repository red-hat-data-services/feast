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

"""Pydantic models for Iceberg REST JSON (OpenAPI DataRegistryConfig / ErrorResponse)."""

from __future__ import annotations

from typing import Annotated, Any, Literal
from uuid import UUID

from pydantic import BaseModel, ConfigDict, Field


class IcebergError(BaseModel):
    message: str
    type: str
    code: int


class ErrorResponse(BaseModel):
    error: IcebergError


class DataRegistryConfig(BaseModel):
    defaults: dict[str, str] = Field(default_factory=dict)
    overrides: dict[str, str] = Field(default_factory=dict)
    endpoints: list[str] = Field(default_factory=list)


class CreateNamespaceRequest(BaseModel):
    namespace: list[str]
    properties: dict[str, str] = Field(default_factory=dict)


class NamespaceResponse(BaseModel):
    namespace: list[str]
    properties: dict[str, str] = Field(default_factory=dict)


class ListNamespacesResponse(BaseModel):
    namespaces: list[list[str]]


class UpdateNamespacePropertiesRequest(BaseModel):
    updates: dict[str, str] = Field(default_factory=dict)
    removals: list[str] = Field(default_factory=list)


class UpdateNamespacePropertiesResponse(BaseModel):
    updated: list[str]
    removed: list[str]
    missing: list[str]


class TableIdentifier(BaseModel):
    namespace: list[str]
    name: str


class ListTablesResponse(BaseModel):
    identifiers: list[TableIdentifier]


class SchemaField(BaseModel):
    name: str
    type: str
    description: str = ""
    nullable: bool = True


class DchConnectionRef(BaseModel):
    """OpenAPI DchConnectionRef.

    Data Registry stores only the stable reference; it does not call DCH.
    """

    type: Literal["dch"]
    id: UUID
    name: str | None = Field(
        default=None,
        description="Current display name, resolved at lookup time; never persisted with assets.",
        json_schema_extra={"readOnly": True},
    )
    connection_type: str | None = Field(
        default=None,
        alias="connectionType",
        description="Connection provider label, such as s3 or postgres; never persisted with assets.",
        json_schema_extra={"readOnly": True},
    )


class RhaiConnectionRef(BaseModel):
    """OpenAPI RhaiConnectionRef.

    Data Registry stores only secret_name; it does not read the Secret.
    """

    type: Literal["secret"]
    secret_name: str
    name: str | None = Field(
        default=None,
        description="Current display name, resolved at lookup time; never persisted with assets.",
        json_schema_extra={"readOnly": True},
    )
    connection_type: str | None = Field(
        default=None,
        alias="connectionType",
        description="Connection provider label, such as s3 or postgres; never persisted with assets.",
        json_schema_extra={"readOnly": True},
    )


ConnectionRef = Annotated[
    DchConnectionRef | RhaiConnectionRef,
    Field(discriminator="type"),
]

StructuredFormat = Literal[
    "iceberg", "parquet", "csv", "delta", "postgresql", "milvus", "other"
]
UnstructuredFormat = Literal["documents", "images", "audio", "video", "binary", "other"]

LicenseType = Literal[
    "internal-use", "cc-by-4.0", "apache-2.0", "proprietary", "restricted"
]
MaturityType = Literal["experimental", "staging", "production", "deprecated"]
PiiStatus = Literal["none", "contains-pii", "contains-sensitive", "anonymized"]


class CreateVolumeRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    name: str
    format: UnstructuredFormat
    storage_location: str | None = None
    connection_ref: ConnectionRef | None = None
    description: str | None = None
    purpose: str | None = None
    license: LicenseType | None = None
    maturity: MaturityType | None = None
    domain: str | None = None
    pii: PiiStatus | None = None
    labels: list[str] | None = None
    properties: dict[str, str] = Field(default_factory=dict)


class UpdateVolumeRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    description: str | None = None
    format: UnstructuredFormat | None = None
    storage_location: str | None = None
    connection_ref: ConnectionRef | None = None
    purpose: str | None = None
    license: LicenseType | None = None
    maturity: MaturityType | None = None
    domain: str | None = None
    pii: PiiStatus | None = None
    properties: dict[str, str] | None = None
    add_labels: list[str] | None = None
    remove_labels: list[str] | None = None
    remove_properties: list[str] | None = None


class CreateGenericTableRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    name: str
    format: StructuredFormat
    storage_location: str | None = None
    connection_ref: ConnectionRef | None = None
    description: str | None = None
    purpose: str | None = None
    license: LicenseType | None = None
    maturity: MaturityType | None = None
    domain: str | None = None
    pii: PiiStatus | None = None
    labels: list[str] | None = None
    schema_fields: list[SchemaField] | None = None
    properties: dict[str, str] = Field(default_factory=dict)


class UpdateGenericTableRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    description: str | None = None
    format: StructuredFormat | None = None
    storage_location: str | None = None
    connection_ref: ConnectionRef | None = None
    purpose: str | None = None
    license: LicenseType | None = None
    maturity: MaturityType | None = None
    domain: str | None = None
    pii: PiiStatus | None = None
    add_labels: list[str] | None = None
    remove_labels: list[str] | None = None
    schema_fields: list[SchemaField] | None = None
    properties: dict[str, str] | None = None
    remove_properties: list[str] | None = None


class AssetResponse(BaseModel):
    name: str
    asset_type: str
    uuid: str
    format: str
    collection: str
    owner: str
    created_at: str
    updated_at: str
    storage_location: str | None = None
    columns: list[SchemaField] | None = Field(
        default=None,
        description="Table schema on GET/PATCH responses. Create/update requests use schema_fields.",
    )
    connection_ref: ConnectionRef | None = None
    description: str | None = None
    labels: list[str] | None = None
    properties: dict[str, str] | None = None


class AssetListResponse(BaseModel):
    assets: list[AssetResponse]


class ListVolumesResponse(BaseModel):
    volumes: list[AssetResponse]


# ----- Labels -----


class LabelListResponse(BaseModel):
    labels: list[str]


class CreateLabelRequest(BaseModel):
    name: str


class LabelResponse(BaseModel):
    name: str


class SearchResult(BaseModel):
    type: str
    namespace: list[str] = Field(min_length=1, max_length=1)
    name: str
    description: str | None = None
    properties: dict[str, str] = Field(default_factory=dict)
    score: int = 0


class SearchResponse(BaseModel):
    query: str
    results: list[SearchResult]
    pagination: dict[str, Any] = Field(default_factory=dict)


# ---------------------------------------------------------------------------
# Iceberg LoadTableResponse (GET .../tables/{table})
# ---------------------------------------------------------------------------


class IcebergField(BaseModel):
    id: int
    name: str
    required: bool
    type: str


class IcebergSchema(BaseModel):
    model_config = ConfigDict(populate_by_name=True, serialize_by_alias=True)

    type: str = "struct"
    schema_id: int = Field(default=0, serialization_alias="schema-id")
    fields: list[IcebergField] = Field(default_factory=list)


class PartitionSpec(BaseModel):
    model_config = ConfigDict(populate_by_name=True, serialize_by_alias=True)

    spec_id: int = Field(default=0, serialization_alias="spec-id")
    fields: list = Field(default_factory=list)


class SortOrder(BaseModel):
    model_config = ConfigDict(populate_by_name=True, serialize_by_alias=True)

    order_id: int = Field(default=0, serialization_alias="order-id")
    fields: list = Field(default_factory=list)


class TableMetadata(BaseModel):
    model_config = ConfigDict(populate_by_name=True, serialize_by_alias=True)

    format_version: int = Field(default=2, serialization_alias="format-version")
    table_uuid: str = Field(serialization_alias="table-uuid")
    location: str
    last_updated_ms: int = Field(serialization_alias="last-updated-ms")
    properties: dict[str, str] = Field(default_factory=dict)
    schemas: list[IcebergSchema] = Field(default_factory=list)
    current_schema_id: int = Field(default=0, serialization_alias="current-schema-id")
    partition_specs: list[PartitionSpec] = Field(
        default_factory=list, serialization_alias="partition-specs"
    )
    default_spec_id: int = Field(default=0, serialization_alias="default-spec-id")
    sort_orders: list[SortOrder] = Field(
        default_factory=list, serialization_alias="sort-orders"
    )
    default_sort_order_id: int = Field(
        default=0, serialization_alias="default-sort-order-id"
    )
    last_column_id: int = Field(default=0, serialization_alias="last-column-id")
    last_sequence_number: int = Field(
        default=0, serialization_alias="last-sequence-number"
    )
    last_partition_id: int = Field(default=999, serialization_alias="last-partition-id")
    snapshots: list = Field(default_factory=list)
    current_snapshot_id: int = Field(
        default=-1, serialization_alias="current-snapshot-id"
    )


class LoadTableResponse(BaseModel):
    model_config = ConfigDict(populate_by_name=True, serialize_by_alias=True)

    metadata_location: str = Field(serialization_alias="metadata-location")
    metadata: TableMetadata
    config: dict[str, str] = Field(default_factory=dict)
