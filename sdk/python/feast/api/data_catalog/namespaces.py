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

"""Iceberg REST namespace (collection) lifecycle.

Empty collections persist as scoped Project tags ``_ns_meta_{ns}/{collection}``
on Feast project ``data-registry``. Do not mount this router on
RestRegistryServer here.
"""

from __future__ import annotations

from fastapi import APIRouter, Request, Response

from feast.api.data_catalog.catalog_utils import (
    DEFAULT_COLLECTION,
    _http_collection,
    _http_namespace,
    _registry,
    create_namespace_meta,
    delete_namespace_meta,
    get_namespace_properties,
    list_collections,
    merge_namespace_properties,
    validate_namespace_exists,
)
from feast.api.data_catalog.errors import (
    BadRequestException,
    NoSuchNamespaceException,
)
from feast.api.data_catalog.models import (
    CreateNamespaceRequest,
    ListNamespacesResponse,
    NamespaceResponse,
    UpdateNamespacePropertiesRequest,
    UpdateNamespacePropertiesResponse,
)


def _project_and_collection(project: str, collection: str) -> tuple[str, str]:
    return _http_namespace(project), _http_collection(collection)


def get_namespace_router() -> APIRouter:
    router = APIRouter(tags=["namespaces"])

    @router.get("/v1/{project}/namespaces", response_model=ListNamespacesResponse)
    def list_namespaces_http(project: str, request: Request) -> ListNamespacesResponse:
        rhai_ns = _http_namespace(project)
        names = list_collections(_registry(request), rhai_ns)
        return ListNamespacesResponse(namespaces=[[name] for name in names])

    @router.post("/v1/{project}/namespaces", response_model=NamespaceResponse)
    def create_namespace(
        project: str, body: CreateNamespaceRequest, request: Request
    ) -> NamespaceResponse:
        rhai_ns = _http_namespace(project)
        collection = _http_collection(body.namespace)
        registry = _registry(request)
        properties = dict(body.properties)
        create_namespace_meta(registry, rhai_ns, collection, properties)
        return NamespaceResponse(namespace=[collection], properties=properties)

    @router.get(
        "/v1/{project}/namespaces/{collection}",
        response_model=NamespaceResponse,
    )
    def get_namespace(
        project: str, collection: str, request: Request
    ) -> NamespaceResponse:
        rhai_ns, col = _project_and_collection(project, collection)
        registry = _registry(request)
        if not validate_namespace_exists(registry, rhai_ns, col):
            raise NoSuchNamespaceException(f"Namespace does not exist: {col}")
        return NamespaceResponse(
            namespace=[col],
            properties=get_namespace_properties(registry, rhai_ns, col),
        )

    @router.head("/v1/{project}/namespaces/{collection}", status_code=204)
    def namespace_exists(project: str, collection: str, request: Request) -> Response:
        rhai_ns, col = _project_and_collection(project, collection)
        if not validate_namespace_exists(_registry(request), rhai_ns, col):
            raise NoSuchNamespaceException(f"Namespace does not exist: {col}")
        return Response(status_code=204)

    @router.delete("/v1/{project}/namespaces/{collection}", status_code=204)
    def drop_namespace(project: str, collection: str, request: Request) -> Response:
        rhai_ns, col = _project_and_collection(project, collection)
        if col == DEFAULT_COLLECTION:
            raise BadRequestException("Cannot drop the default collection")
        delete_namespace_meta(_registry(request), rhai_ns, col)
        return Response(status_code=204)

    @router.post(
        "/v1/{project}/namespaces/{collection}/properties",
        response_model=UpdateNamespacePropertiesResponse,
    )
    def update_namespace_properties(
        project: str,
        collection: str,
        body: UpdateNamespacePropertiesRequest,
        request: Request,
    ) -> UpdateNamespacePropertiesResponse:
        rhai_ns, col = _project_and_collection(project, collection)
        overlap = sorted(set(body.updates) & set(body.removals))
        if overlap:
            raise BadRequestException(
                "Cannot update and remove the same property: " + ", ".join(overlap)
            )
        updated, removed, missing = merge_namespace_properties(
            _registry(request),
            rhai_ns,
            col,
            dict(body.updates),
            list(body.removals),
        )
        return UpdateNamespacePropertiesResponse(
            updated=updated,
            removed=removed,
            missing=missing,
        )

    return router
