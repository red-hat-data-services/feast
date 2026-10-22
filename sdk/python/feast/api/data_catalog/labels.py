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

"""RHOAI project-level label CRUD routes.

Labels are simple string identifiers for grouping and filtering assets.
They are managed at the project level (create/list/delete) and assigned
per asset via add_labels/remove_labels on volume and generic-table updates.

All labels are project-scoped. A label can be created explicitly via
POST /labels or implicitly by assigning it to an asset during registration.
listLabels returns both. deleteLabel removes the label everywhere.
"""

from __future__ import annotations

from fastapi import APIRouter, Request, Response

from feast.api.data_catalog.catalog_utils import (
    _registry,
    _require_namespace,
    create_label_meta,
    delete_label_meta,
    list_all_labels,
)
from feast.api.data_catalog.errors import BadRequestException
from feast.api.data_catalog.models import (
    CreateLabelRequest,
    LabelListResponse,
    LabelResponse,
)


def _rhai_ns(project: str) -> str:
    try:
        return _require_namespace(project)
    except ValueError as exc:
        raise BadRequestException(str(exc)) from exc


def get_label_router() -> APIRouter:
    router = APIRouter(tags=["labels"])

    @router.get(
        "/v1/{project}/labels",
        response_model=LabelListResponse,
    )
    def list_labels(project: str, request: Request) -> LabelListResponse:
        rhai_ns = _rhai_ns(project)
        registry = _registry(request)
        return LabelListResponse(labels=list_all_labels(registry, rhai_ns))

    @router.post(
        "/v1/{project}/labels",
        response_model=LabelResponse,
        status_code=201,
    )
    def create_label(
        project: str, body: CreateLabelRequest, request: Request
    ) -> LabelResponse:
        rhai_ns = _rhai_ns(project)
        registry = _registry(request)
        name = body.name.strip()
        if not name:
            raise BadRequestException("Label name must not be empty")
        if "/" in name:
            raise BadRequestException("Label name must not contain '/'")
        create_label_meta(registry, rhai_ns, name)
        return LabelResponse(name=name)

    @router.delete(
        "/v1/{project}/labels/{label}",
        status_code=204,
    )
    def delete_label(project: str, label: str, request: Request) -> Response:
        rhai_ns = _rhai_ns(project)
        registry = _registry(request)
        label_name = label.strip()
        if not label_name:
            raise BadRequestException("Label name must not be empty")
        delete_label_meta(registry, rhai_ns, label_name)
        return Response(status_code=204)

    return router
