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

"""Mount Data Registry Iceberg ``/v1`` routes on RestRegistryServer.

Kept out of ``feast.api.data_catalog`` so that package stays free of
RestRegistryServer / DATA_REGISTRY_ENABLED identifiers.
"""

from __future__ import annotations

import logging
import os
from typing import Any

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse

from feast.api.data_catalog.config import get_config_router
from feast.api.data_catalog.errors import (
    BadRequestException,
    IcebergRESTException,
    validation_message,
)
from feast.api.data_catalog.generic_tables import get_generic_table_router
from feast.api.data_catalog.labels import get_label_router
from feast.api.data_catalog.namespaces import get_namespace_router
from feast.api.data_catalog.search import get_search_router
from feast.api.data_catalog.tables import get_table_router
from feast.api.data_catalog.volumes import get_volume_router

logger = logging.getLogger(__name__)


def is_data_registry_enabled() -> bool:
    return os.environ.get("DATA_REGISTRY_ENABLED", "").lower() in (
        "1",
        "true",
        "yes",
    )


def sync_registry_project_protection(store: Any) -> None:
    """Protect the right project before the REST registry server listens.

    Data Registry mode tags only the internal ``data-registry`` project, through
    ``ensure_catalog_project``. It does not run the FEAST_PROTECTED_PROJECT sync:
    that sync tags ``store.project``, which is ``spec.feastProject``, and the
    Python server does not read ``FEAST_PROJECT``.

    A project already tagged protected is cleared here. That tag is left behind
    when a protected FeatureStore becomes the data registry, or on upgrade from
    a process that ran with ``FEAST_PROTECTED_PROJECT=true``. ``GET /projects``
    hides every project that still carries it. The catalog project stays
    protected.
    """
    if is_data_registry_enabled():
        from feast.api.data_catalog.catalog_utils import ensure_catalog_project

        ensure_catalog_project(store.registry)
        _clear_stale_feast_project_protection(store)
        return

    from feast.registry_server import _sync_protected_project_tag

    _sync_protected_project_tag(store)


def _clear_stale_feast_project_protection(store: Any) -> None:
    """Remove ``feast.dev/protected-project`` from ``store.project``.

    No-op when that project is missing, is the catalog project, or is not
    tagged protected. Other tags are kept. The catalog project is updated
    separately with compare-and-swap and is not rewritten here.

    The tag is removed with ``mutate_project``, which re-reads the row and
    compare-and-swaps. A registry without that update fails startup.
    ``apply_project`` of the project read above would replace tags written
    after that read.
    """
    from feast.api.data_catalog.catalog_utils import CATALOG_PROJECT
    from feast.constants import PROTECTED_PROJECT_TAG
    from feast.errors import ProjectObjectNotFoundException

    project_name = store.project
    if (
        not isinstance(project_name, str)
        or project_name == ""
        or project_name == CATALOG_PROJECT
    ):
        return

    try:
        existing = store.registry.get_project(name=project_name, allow_cache=False)
    except ProjectObjectNotFoundException:
        return

    if existing.tags.get(PROTECTED_PROJECT_TAG) != "true":
        return

    # Re-read inside compare-and-swap. The guard snapshot in ``existing`` is
    # only used to decide whether a write is needed.
    def mutator(project: Any, _conn: Any) -> None:
        if project.tags.get(PROTECTED_PROJECT_TAG) == "true":
            del project.tags[PROTECTED_PROJECT_TAG]

    from feast.api.data_catalog.errors import ServiceFailureException
    from feast.errors import ConcurrentVersionConflict

    mutate = getattr(store.registry, "mutate_project", None)
    if not callable(mutate):
        raise ServiceFailureException(
            "Registry does not support atomic project mutation required to clear stale protection"
        )
    try:
        mutate(project_name, mutator, create_if_missing=False)
    except ProjectObjectNotFoundException:
        return
    except ConcurrentVersionConflict as exc:
        raise ServiceFailureException(str(exc)) from exc
    logger.info("Removed protected tag from project '%s'", project_name)


def add_data_catalog_routes(app: FastAPI, registry) -> None:
    """Include Iceberg routers when ``DATA_REGISTRY_ENABLED`` is truthy.

    Iceberg JSON for ``IcebergRESTException`` and for ``/v1`` validation
    errors only. Feast ``/entities`` validation stays 422 ``detail``.
    """
    if not is_data_registry_enabled():
        return
    app.state.registry = registry
    app.include_router(get_config_router())
    app.include_router(get_namespace_router())
    app.include_router(get_table_router())
    app.include_router(get_volume_router())
    app.include_router(get_generic_table_router())
    app.include_router(get_label_router())
    app.include_router(get_search_router())

    @app.exception_handler(IcebergRESTException)
    async def iceberg_rest_exception_handler(
        request: Request, exc: IcebergRESTException
    ) -> JSONResponse:
        return JSONResponse(status_code=exc.http_status, content=exc.to_payload())

    @app.exception_handler(RequestValidationError)
    async def request_validation_error_handler(
        request: Request, exc: RequestValidationError
    ) -> JSONResponse:
        if request.url.path.startswith("/v1"):
            payload = BadRequestException(validation_message(exc)).to_payload()
            return JSONResponse(status_code=400, content=payload)
        return JSONResponse(
            status_code=422,
            content={
                "status_code": 422,
                "detail": str(exc),
                "error_type": "RequestValidationError",
            },
        )
