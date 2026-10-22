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

"""Iceberg REST config endpoints (RHAI-387) plus RHOAI extensions (RHAI-376).

``GET /v1/config`` is the engine bootstrap call. ``warehouse`` (or the
prefixed path) becomes ``overrides.prefix`` so later calls use
``/v1/{prefix}/...``. ``endpoints`` lists operations that actually serve.
Iceberg table create/update/drop/rename and LoadTable exist as routes but
return 501 and are not advertised. Namespace CRUD and RHOAI volume /
generic-table catalog registration (including iceberg-format rows) are
advertised. Auth and rate limits are kube-rbac-proxy / ingress, not these
handlers. Mounting on RestRegistryServer is RHAI-390.
"""

from __future__ import annotations

from fastapi import APIRouter, Query

from feast.api.data_catalog.catalog_utils import _http_namespace
from feast.api.data_catalog.models import DataRegistryConfig

# Iceberg clients match ``{prefix}`` in these strings, not OpenAPI ``{project}``.
CATALOG_CONFIG_ENDPOINTS = [
    "GET /v1/config",
    "GET /v1/{prefix}/config",
    "GET /v1/{prefix}/namespaces",
    "POST /v1/{prefix}/namespaces",
    "GET /v1/{prefix}/namespaces/{namespace}",
    "HEAD /v1/{prefix}/namespaces/{namespace}",
    "DELETE /v1/{prefix}/namespaces/{namespace}",
    "POST /v1/{prefix}/namespaces/{namespace}/properties",
    "GET /v1/{prefix}/namespaces/{namespace}/tables",
    "GET /v1/{prefix}/namespaces/{namespace}/tables/{table}",
    "HEAD /v1/{prefix}/namespaces/{namespace}/tables/{table}",
    "GET /v1/{prefix}/namespaces/{namespace}/volumes",
    "POST /v1/{prefix}/namespaces/{namespace}/volumes",
    "GET /v1/{prefix}/namespaces/{namespace}/volumes/{volume}",
    "HEAD /v1/{prefix}/namespaces/{namespace}/volumes/{volume}",
    "PATCH /v1/{prefix}/namespaces/{namespace}/volumes/{volume}",
    "DELETE /v1/{prefix}/namespaces/{namespace}/volumes/{volume}",
    "GET /v1/{prefix}/namespaces/{namespace}/generic-tables",
    "POST /v1/{prefix}/namespaces/{namespace}/generic-tables",
    "GET /v1/{prefix}/namespaces/{namespace}/generic-tables/{table}",
    "PATCH /v1/{prefix}/namespaces/{namespace}/generic-tables/{table}",
    "DELETE /v1/{prefix}/namespaces/{namespace}/generic-tables/{table}",
    "GET /v1/{prefix}/labels",
    "POST /v1/{prefix}/labels",
    "DELETE /v1/{prefix}/labels/{label}",
    "GET /v1/{prefix}/search",
]


def _config_prefix(raw: str | None) -> str | None:
    """Validate Iceberg ``{prefix}`` (K8s / RHOAI namespace). Empty stays unset."""
    if not raw:
        return None
    return _http_namespace(raw)


def data_catalog_config(*, prefix: str | None = None) -> DataRegistryConfig:
    overrides: dict[str, str] = {}
    if prefix:
        overrides["prefix"] = prefix
    return DataRegistryConfig(
        defaults={},
        overrides=overrides,
        endpoints=list(CATALOG_CONFIG_ENDPOINTS),
    )


def get_config_router() -> APIRouter:
    router = APIRouter(tags=["discovery"])

    @router.get(
        "/v1/config",
        response_model=DataRegistryConfig,
        operation_id="getBootstrapConfig",
    )
    def get_data_catalog_config(
        warehouse: str | None = Query(default=None),
    ) -> DataRegistryConfig:
        """Return bootstrap configuration for Iceberg engine clients.

        Auth: ``kube-rbac-proxy`` · Rate limit: ingress/gateway.
        """
        return data_catalog_config(prefix=_config_prefix(warehouse))

    @router.get(
        "/v1/{project}/config",
        response_model=DataRegistryConfig,
        operation_id="getProjectConfig",
    )
    def get_data_catalog_prefixed_config(project: str) -> DataRegistryConfig:
        """Return project-scoped configuration.

        Auth: ``kube-rbac-proxy`` · Rate limit: ingress/gateway.
        """
        return data_catalog_config(prefix=_config_prefix(project))

    return router
