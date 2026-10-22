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

"""Catalog translation-layer helpers.

Iceberg REST identity is a tuple (project, collection, table). Feast identity is
(project, name) and the catalog uses a single Feast project ``data-registry``,
so SavedDataset.name must carry namespace + collection + display name.

Callers (Iceberg REST, UI, engines) never see the scoped string: prefix on
write / get / delete, strip with unscoped_name on API responses.

The shared project constant, lazy Feast project creation, and
collection resolve/list/exists helpers are provided here.
This module does not mount Iceberg REST routes.

Empty collections persist as a scoped Project tag
``_ns_meta_{namespace}/{collection}`` on the shared ``data-registry`` project.
Unscoped ``_ns_meta_{collection}`` collides across namespaces — never write it.
"""

from __future__ import annotations

import json
import threading
from typing import Any, Callable

from fastapi import Request

from feast.api.data_catalog.errors import (
    BadRequestException,
    NamespaceAlreadyExistsException,
    NamespaceNotEmptyException,
    NoSuchNamespaceException,
    ServiceFailureException,
)
from feast.errors import ConcurrentVersionConflict, ProjectObjectNotFoundException
from feast.infra.registry.base_registry import BaseRegistry
from feast.project import Project
from feast.saved_dataset import SavedDataset

SCOPE_SEP = "/"
MAX_SCOPED_NAME = 255  # saved_datasets.saved_dataset_name VARCHAR(255)
CATALOG_PROJECT = "data-registry"
DEFAULT_COLLECTION = "default"
NAMESPACE_SEPARATOR = "\x1f"
NS_META_PREFIX = "_ns_meta_"
LABEL_META_PREFIX = "_label_"
CATALOG_MANAGED_TAG = "_catalog_managed"
CATALOG_MANAGED_VALUE = "true"
_CATALOG_MANAGED_TAGS = {CATALOG_MANAGED_TAG: CATALOG_MANAGED_VALUE}
# File / other registries have no Project row lock. Production catalog is SQL.
_FALLBACK_PROJECT_LOCK = threading.Lock()


def _require_part(label: str, value: str) -> str:
    if not isinstance(value, str):
        raise ValueError(f"{label} must be a string")
    if not value:
        raise ValueError(f"{label} must be a non-empty string")
    if value != value.strip():
        raise ValueError(
            f"{label} must not have leading/trailing whitespace (got {value!r})"
        )
    if any(c.isspace() for c in value):
        raise ValueError(
            f"{label} must not contain internal whitespace (got {value!r})"
        )
    if SCOPE_SEP in value:
        raise ValueError(f"{label} must not contain {SCOPE_SEP!r} (got {value!r})")
    return value


def _require_namespace(namespace: str) -> str:
    part = _require_part("namespace", namespace)
    if part != part.lower():
        raise ValueError(f"namespace must be lowercase (got {namespace!r})")
    return part


def scoped_name(namespace: str, collection: str, name: str) -> str:
    """Build a unique SavedDataset.name for the shared catalog Feast project.

    Format: ``{namespace}/{collection}/{display_name}``.
    """
    parts = (
        _require_namespace(namespace),
        _require_part("collection", collection),
        _require_part("name", name),
    )
    scoped = SCOPE_SEP.join(parts)
    if len(scoped) > MAX_SCOPED_NAME:
        raise ValueError(
            f"scoped name exceeds {MAX_SCOPED_NAME} characters ({len(scoped)})"
        )
    return scoped


def parse_scoped_name(scoped: str) -> tuple[str, str, str]:
    """Invert scoped_name. Raises ValueError if not exactly three parts."""
    if not isinstance(scoped, str) or not scoped:
        raise ValueError("scoped name must be a non-empty string")
    parts = scoped.split(SCOPE_SEP)
    if len(parts) != 3:
        raise ValueError(
            f"scoped name must be namespace/collection/name (got {scoped!r})"
        )
    return (
        _require_namespace(parts[0]),
        _require_part("collection", parts[1]),
        _require_part("name", parts[2]),
    )


def unscoped_name(scoped: str) -> str:
    """Display name for Iceberg / API JSON — never includes SCOPE_SEP."""
    return parse_scoped_name(scoped)[2]


def ensure_catalog_project(registry: BaseRegistry) -> Project:
    """Get-or-create the single catalog Feast project. Idempotent."""
    try:
        return registry.get_project(CATALOG_PROJECT, allow_cache=False)
    except ProjectObjectNotFoundException:
        project = Project(
            name=CATALOG_PROJECT,
            description="RHOAI Data Registry",
        )
        registry.apply_project(project)
        return project


def resolve_namespace(raw: str | list[str]) -> str:
    """Iceberg namespace → collection display name. Phase 1: one segment."""
    parts = list(raw) if isinstance(raw, list) else [raw]
    if len(parts) != 1:
        raise ValueError("namespace must be a single collection name")
    value = parts[0]
    if not isinstance(value, str) or NAMESPACE_SEPARATOR in value:
        raise ValueError("nested namespaces are not supported")
    return _require_part("collection", value)


def _as_bad_request(exc: ValueError) -> BadRequestException:
    return BadRequestException(str(exc))


def _registry(request: Request) -> BaseRegistry:
    registry = getattr(request.app.state, "registry", None)
    if registry is None:
        raise ServiceFailureException("Data Registry is not configured")
    return registry


def _http_namespace(project: str) -> str:
    """K8s / RHOAI namespace from Iceberg path ``{project}``."""
    try:
        return _require_namespace(project)
    except ValueError as exc:
        raise _as_bad_request(exc) from exc


def _http_collection(collection: str | list[str]) -> str:
    """Iceberg collection from a path segment or CreateNamespace ``namespace`` list."""
    try:
        return resolve_namespace(collection)
    except ValueError as exc:
        raise _as_bad_request(exc) from exc


def _http_part(label: str, value: str) -> str:
    try:
        return _require_part(label, value)
    except ValueError as exc:
        raise _as_bad_request(exc) from exc


def _catalog_saved_datasets(
    registry: BaseRegistry,
    rhai_ns: str,
    collection: str | None = None,
) -> list[SavedDataset]:
    """SavedDatasets stamped ``_catalog_managed=true`` for this tenant."""
    kwargs: dict[str, Any] = {
        "namespace": rhai_ns,
        "tags": _CATALOG_MANAGED_TAGS,
    }
    if collection is not None:
        kwargs["collection"] = collection
    return registry.list_saved_datasets(CATALOG_PROJECT, **kwargs)


def list_collections(registry: BaseRegistry, rhai_ns: str) -> list[str]:
    """Distinct collection display names for one tenant, plus ``default``.

    Unions catalog-managed SavedDataset.collection values with scoped
    ``_ns_meta_{ns}/{collection}`` Project tags so empty POST-created
    collections appear. Unmanaged rows in ``data-registry`` are ignored.
    """
    ns = _require_namespace(rhai_ns)
    found = {
        ds.collection for ds in _catalog_saved_datasets(registry, ns) if ds.collection
    }
    found.update(_ns_meta_collections(registry, ns))
    found.add(DEFAULT_COLLECTION)
    return sorted(found)


def validate_namespace_exists(
    registry: BaseRegistry, rhai_ns: str, collection: str
) -> bool:
    """True if the collection exists for this tenant. ``default`` is always True."""
    ns = _require_namespace(rhai_ns)
    col = _require_part("collection", collection)
    if col == DEFAULT_COLLECTION:
        return True
    if _has_namespace_meta(registry, ns, col):
        return True
    return any(True for _ in _catalog_saved_datasets(registry, ns, col))


def ns_meta_key(rhai_ns: str, collection: str) -> str:
    """Scoped Project tag key. Never ``_ns_meta_{collection}`` alone."""
    ns = _require_namespace(rhai_ns)
    col = _require_part("collection", collection)
    return f"{NS_META_PREFIX}{ns}/{col}"


def parse_ns_meta_key(key: str) -> tuple[str, str] | None:
    """Return ``(rhai_ns, collection)`` or None if the key is not a scoped meta tag."""
    if not isinstance(key, str) or not key.startswith(NS_META_PREFIX):
        return None
    rest = key[len(NS_META_PREFIX) :]
    if rest.count(SCOPE_SEP) != 1:
        return None
    ns, col = rest.split(SCOPE_SEP, 1)
    try:
        return _require_namespace(ns), _require_part("collection", col)
    except ValueError:
        return None


def collection_has_assets(
    registry: BaseRegistry, rhai_ns: str, collection: str
) -> bool:
    """True if any catalog-managed SavedDataset exists in this tenant collection."""
    ns = _require_namespace(rhai_ns)
    col = _require_part("collection", collection)
    return any(True for _ in _catalog_saved_datasets(registry, ns, col))


def get_namespace_properties(
    registry: BaseRegistry, rhai_ns: str, collection: str
) -> dict[str, str]:
    key = ns_meta_key(rhai_ns, collection)
    project = _get_catalog_project(registry)
    if project is None:
        return {}
    raw = project.tags.get(key)
    if not raw:
        return {}
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return {}
    if not isinstance(data, dict):
        return {}
    return {str(k): str(v) for k, v in data.items()}


def _saved_datasets_in_collection(
    registry: BaseRegistry,
    conn: Any,
    rhai_ns: str,
    collection: str,
) -> bool:
    """True if a catalog-managed SavedDataset exists in this tenant collection.

    When ``conn`` is the mutate transaction, SELECT proto on that connection
    (tags are not a SQL column). File-registry fallback uses list + tag filter.
    """
    if conn is None:
        return any(True for _ in _catalog_saved_datasets(registry, rhai_ns, collection))
    from sqlalchemy import select as sa_select

    from feast.infra.registry.sql import saved_datasets
    from feast.protos.feast.core.SavedDataset_pb2 import (
        SavedDataset as SavedDatasetProto,
    )

    rows = conn.execute(
        sa_select(saved_datasets.c.saved_dataset_proto).where(
            saved_datasets.c.project_id == CATALOG_PROJECT,
            saved_datasets.c.namespace == rhai_ns,
            saved_datasets.c.collection == collection,
        )
    )
    for row in rows:
        proto = SavedDatasetProto.FromString(bytes(row._mapping["saved_dataset_proto"]))
        tags = dict(proto.spec.tags)
        if tags.get(CATALOG_MANAGED_TAG) == CATALOG_MANAGED_VALUE:
            return True
    return False


def _collection_present_in_mutate(
    project: Project,
    registry: BaseRegistry,
    conn: Any,
    rhai_ns: str,
    collection: str,
    key: str,
) -> bool:
    """True if this collection exists as tag, ``default``, or catalog-managed assets."""
    if collection == DEFAULT_COLLECTION or key in project.tags:
        return True
    return _saved_datasets_in_collection(registry, conn, rhai_ns, collection)


def _mutate_catalog_project(
    registry: BaseRegistry,
    mutator: Callable[[Project, Any], None],
    *,
    create_if_missing: bool = True,
) -> None:
    """Run ``mutator(project, conn)`` with SQL compare-and-swap when available."""

    def catalog_mutator(project: Project, conn: Any) -> None:
        if create_if_missing and not project.description:
            project.description = "RHOAI Data Registry"
        mutator(project, conn)

    mutate = getattr(registry, "mutate_project", None)
    if callable(mutate):
        try:
            mutate(
                CATALOG_PROJECT,
                catalog_mutator,
                create_if_missing=create_if_missing,
            )
        except ConcurrentVersionConflict as exc:
            raise ServiceFailureException(str(exc)) from exc
        return
    with _FALLBACK_PROJECT_LOCK:
        try:
            project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
        except ProjectObjectNotFoundException:
            if not create_if_missing:
                raise
            project = Project(
                name=CATALOG_PROJECT,
                description="RHOAI Data Registry",
            )
            registry.apply_project(project)
            project = registry.get_project(CATALOG_PROJECT, allow_cache=False)
        catalog_mutator(project, None)
        registry.apply_project(project)


def create_namespace_meta(
    registry: BaseRegistry,
    rhai_ns: str,
    collection: str,
    properties: dict[str, str],
) -> None:
    """Persist an empty collection tag. Raises if it already exists.

    Existence is decided inside mutate: tag map, ``default``, and SavedDatasets
    on the same write connection.
    """
    ns = _require_namespace(rhai_ns)
    col = _require_part("collection", collection)
    key = ns_meta_key(ns, col)
    payload = json.dumps(properties, separators=(",", ":"), sort_keys=True)

    def mutator(project: Project, conn: Any) -> None:
        if col == DEFAULT_COLLECTION or key in project.tags:
            raise NamespaceAlreadyExistsException(f"Namespace already exists: {col}")
        if _saved_datasets_in_collection(registry, conn, ns, col):
            raise NamespaceAlreadyExistsException(f"Namespace already exists: {col}")
        tags = dict(project.tags)
        tags[key] = payload
        project.tags = tags

    _mutate_catalog_project(registry, mutator)


def set_namespace_properties(
    registry: BaseRegistry,
    rhai_ns: str,
    collection: str,
    properties: dict[str, str],
) -> None:
    """Replace one collection's JSON properties. Collection must already exist.

    Existence (tag, ``default``, or catalog-managed SavedDatasets) is decided on
    the same write connection as merge. Missing collection is 404 and does not
    write a tag.
    """
    ns = _require_namespace(rhai_ns)
    col = _require_part("collection", collection)
    key = ns_meta_key(ns, col)
    payload = json.dumps(properties, separators=(",", ":"), sort_keys=True)

    def mutator(project: Project, conn: Any) -> None:
        if not _collection_present_in_mutate(project, registry, conn, ns, col, key):
            raise NoSuchNamespaceException(f"Namespace does not exist: {col}")
        tags = dict(project.tags)
        tags[key] = payload
        project.tags = tags

    _mutate_catalog_project(registry, mutator)


def merge_namespace_properties(
    registry: BaseRegistry,
    rhai_ns: str,
    collection: str,
    updates: dict[str, str],
    removals: list[str],
) -> tuple[list[str], list[str], list[str]]:
    """Apply updates/removals to one collection's JSON inside compare-and-swap.

    Existence (tag, ``default``, or catalog-managed SavedDatasets) is decided on
    the same write connection. Missing collection is 404 and does not write a
    tag (so a concurrent DELETE cannot be undone by properties).

    Returns ``(updated, removed, missing)`` from the snapshot that was written.
    """
    ns = _require_namespace(rhai_ns)
    col = _require_part("collection", collection)
    key = ns_meta_key(ns, col)
    result: dict[str, list[str]] = {"updated": [], "removed": [], "missing": []}

    def mutator(project: Project, conn: Any) -> None:
        if not _collection_present_in_mutate(project, registry, conn, ns, col, key):
            raise NoSuchNamespaceException(f"Namespace does not exist: {col}")
        current: dict[str, str] = {}
        raw = project.tags.get(key)
        if raw:
            try:
                data = json.loads(raw)
            except json.JSONDecodeError:
                data = None
            if isinstance(data, dict):
                current = {str(k): str(v) for k, v in data.items()}
        for prop_key, value in updates.items():
            current[prop_key] = value
        removed: list[str] = []
        missing: list[str] = []
        for prop_key in removals:
            if prop_key in current:
                current.pop(prop_key)
                removed.append(prop_key)
            else:
                missing.append(prop_key)
        tags = dict(project.tags)
        tags[key] = json.dumps(current, separators=(",", ":"), sort_keys=True)
        project.tags = tags
        result["updated"] = sorted(updates)
        result["removed"] = removed
        result["missing"] = missing

    _mutate_catalog_project(registry, mutator)
    return result["updated"], result["removed"], result["missing"]


def delete_namespace_meta(
    registry: BaseRegistry, rhai_ns: str, collection: str
) -> None:
    """Drop an empty collection tag. Raises if missing or not empty.

    Empty/exists are decided inside mutate: catalog-managed SavedDatasets on the
    same write connection, then the tag map. Table-only collections are 409.
    """
    ns = _require_namespace(rhai_ns)
    col = _require_part("collection", collection)
    key = ns_meta_key(ns, col)

    def mutator(project: Project, conn: Any) -> None:
        if _saved_datasets_in_collection(registry, conn, ns, col):
            raise NamespaceNotEmptyException(f"Namespace not empty: {col}")
        if key not in project.tags:
            raise NoSuchNamespaceException(f"Namespace does not exist: {col}")
        tags = dict(project.tags)
        tags.pop(key, None)
        project.tags = tags

    try:
        _mutate_catalog_project(registry, mutator, create_if_missing=False)
    except ProjectObjectNotFoundException as exc:
        raise NoSuchNamespaceException(f"Namespace does not exist: {col}") from exc


def _get_catalog_project(registry: BaseRegistry) -> Project | None:
    try:
        return registry.get_project(CATALOG_PROJECT, allow_cache=False)
    except ProjectObjectNotFoundException:
        return None


def _has_namespace_meta(registry: BaseRegistry, rhai_ns: str, collection: str) -> bool:
    project = _get_catalog_project(registry)
    if project is None:
        return False
    return ns_meta_key(rhai_ns, collection) in project.tags


def _ns_meta_collections(registry: BaseRegistry, rhai_ns: str) -> set[str]:
    project = _get_catalog_project(registry)
    if project is None:
        return set()
    found: set[str] = set()
    for key in project.tags:
        parsed = parse_ns_meta_key(key)
        if parsed and parsed[0] == rhai_ns:
            found.add(parsed[1])
    return found


# ---------------------------------------------------------------------------
# Label meta helpers (project-level label CRUD)
# ---------------------------------------------------------------------------


def label_meta_key(rhai_ns: str, label_name: str) -> str:
    """Scoped Project tag key for a project-level label."""
    ns = _require_namespace(rhai_ns)
    if not label_name or not label_name.strip():
        raise ValueError("label name must not be empty")
    name = label_name.strip()
    if "/" in name:
        raise ValueError(f"label name must not contain '/' (got {name!r})")
    return f"{LABEL_META_PREFIX}{ns}/{name}"


def parse_label_meta_key(key: str) -> tuple[str, str] | None:
    """Return ``(rhai_ns, label_name)`` or None if the key is not a label tag."""
    if not isinstance(key, str) or not key.startswith(LABEL_META_PREFIX):
        return None
    rest = key[len(LABEL_META_PREFIX) :]
    if SCOPE_SEP not in rest:
        return None
    ns, label = rest.split(SCOPE_SEP, 1)
    if not ns or not label:
        return None
    return ns, label


def list_explicit_labels(registry: BaseRegistry, rhai_ns: str) -> set[str]:
    """Labels explicitly created via POST /labels (stored as Project tags)."""
    ns = _require_namespace(rhai_ns)
    project = _get_catalog_project(registry)
    if project is None:
        return set()
    found: set[str] = set()
    for key in project.tags or {}:
        parsed = parse_label_meta_key(key)
        if parsed and parsed[0] == ns:
            found.add(parsed[1])
    return found


def list_discovered_labels(registry: BaseRegistry, rhai_ns: str) -> set[str]:
    """Labels found on assets (from ``_labels`` tags on SavedDatasets)."""
    from feast.api.data_catalog.catalog_assets import labels_from_tags

    ns = _require_namespace(rhai_ns)
    found: set[str] = set()
    for dataset in _catalog_saved_datasets(registry, ns):
        labels = labels_from_tags(dataset.tags or {})
        if labels:
            found.update(labels)
    return found


def list_all_labels(registry: BaseRegistry, rhai_ns: str) -> list[str]:
    """Merge explicit + discovered labels, sorted."""
    return sorted(
        list_explicit_labels(registry, rhai_ns)
        | list_discovered_labels(registry, rhai_ns)
    )


def create_label_meta(registry: BaseRegistry, rhai_ns: str, label_name: str) -> None:
    """Create a project-level label. Raises AlreadyExistsException if present anywhere."""
    from feast.api.data_catalog.errors import AlreadyExistsException

    ns = _require_namespace(rhai_ns)
    key = label_meta_key(ns, label_name)

    discovered = list_discovered_labels(registry, ns)

    def mutator(project: Project, conn: Any) -> None:
        if key in project.tags:
            raise AlreadyExistsException(f"Label already exists: {label_name}")
        if label_name in discovered:
            raise AlreadyExistsException(f"Label already exists: {label_name}")
        tags = dict(project.tags)
        tags[key] = "true"
        project.tags = tags

    _mutate_catalog_project(registry, mutator)


def delete_label_meta(registry: BaseRegistry, rhai_ns: str, label_name: str) -> None:
    """Delete a project-level label and cascade-remove from all assets.

    Raises NoSuchLabelException if the label does not exist anywhere
    (neither as an explicit project tag nor on any asset).
    """
    from feast.api.data_catalog.catalog_assets import (
        labels_from_tags,
        merge_labels,
        replace_catalog_dataset,
    )
    from feast.api.data_catalog.errors import NoSuchLabelException

    ns = _require_namespace(rhai_ns)
    key = label_meta_key(ns, label_name)

    explicit_exists = False
    project = _get_catalog_project(registry)
    if project is not None and key in (project.tags or {}):
        explicit_exists = True

    assets_with_label: list = []
    for dataset in _catalog_saved_datasets(registry, ns):
        labels = labels_from_tags(dataset.tags or {})
        if labels and label_name in labels:
            assets_with_label.append(dataset)

    if not explicit_exists and not assets_with_label:
        raise NoSuchLabelException(f"Label does not exist: {label_name}")

    if explicit_exists:

        def mutator(project: Project, conn: Any) -> None:
            tags = dict(project.tags)
            tags.pop(key, None)
            project.tags = tags

        _mutate_catalog_project(registry, mutator, create_if_missing=False)

    for dataset in assets_with_label:
        dataset.tags = merge_labels(dataset.tags or {}, remove=[label_name])
        replace_catalog_dataset(registry, dataset)
