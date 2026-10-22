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

"""Iceberg REST error types and FastAPI handlers.

Handlers are registered on the data-catalog FastAPI app only. Do not call
``register_error_handlers`` on RestRegistryServer without a ``/v1`` path
check — that would rewrite Feast REST bodies.
"""

from __future__ import annotations

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse


class IcebergRESTException(Exception):
    """Base for catalog errors. ``type`` in JSON is ``error_type``."""

    http_status: int = 500
    error_type: str = "ServiceFailureException"

    def __init__(self, message: str) -> None:
        super().__init__(message)
        self.message = message

    def to_payload(self) -> dict:
        return {
            "error": {
                "message": self.message,
                "type": self.error_type,
                "code": self.http_status,
            }
        }


class NoSuchNamespaceException(IcebergRESTException):
    http_status = 404
    error_type = "NoSuchNamespaceException"


class NoSuchTableException(IcebergRESTException):
    http_status = 404
    error_type = "NoSuchTableException"


class AlreadyExistsException(IcebergRESTException):
    http_status = 409
    error_type = "AlreadyExistsException"


class NamespaceAlreadyExistsException(AlreadyExistsException):
    pass


class NoSuchVolumeException(IcebergRESTException):
    http_status = 404
    error_type = "NoSuchVolumeException"


class NoSuchLabelException(IcebergRESTException):
    http_status = 404
    error_type = "NoSuchLabelException"


class NamespaceNotEmptyException(IcebergRESTException):
    http_status = 409
    error_type = "NamespaceNotEmptyException"


class BadRequestException(IcebergRESTException):
    http_status = 400
    error_type = "BadRequestException"


class NotImplementedException(IcebergRESTException):
    http_status = 501
    error_type = "NotImplementedException"


class ServiceFailureException(IcebergRESTException):
    http_status = 500
    error_type = "ServiceFailureException"


def missing_required_fields(*fields: str) -> BadRequestException:
    return BadRequestException("Missing required fields: " + ", ".join(fields))


def _field_from_loc(loc: tuple) -> str:
    parts = [str(p) for p in loc if p not in ("body", "query", "path")]
    return ".".join(parts) if parts else "request"


def validation_message(exc: RequestValidationError) -> str:
    missing: list[str] = []
    other: list[str] = []
    for err in exc.errors():
        name = _field_from_loc(tuple(err.get("loc", ())))
        if err.get("type") == "missing":
            missing.append(name)
        else:
            other.append(f"{name}: {err.get('msg', 'invalid')}")
    parts: list[str] = []
    if missing:
        parts.append("Missing required fields: " + ", ".join(missing))
    if other:
        parts.append("; ".join(other))
    return ". ".join(parts) or "Invalid request"


def register_error_handlers(app: FastAPI) -> None:
    """Iceberg JSON for catalog exceptions and request validation.

    Does not install a catch-all ``Exception`` handler (would hide Feast
    ``/api/v1`` errors if mounted on the same app).
    """

    @app.exception_handler(IcebergRESTException)
    async def iceberg_rest_exception_handler(
        request: Request, exc: IcebergRESTException
    ) -> JSONResponse:
        return JSONResponse(status_code=exc.http_status, content=exc.to_payload())

    @app.exception_handler(RequestValidationError)
    async def request_validation_error_handler(
        request: Request, exc: RequestValidationError
    ) -> JSONResponse:
        payload = BadRequestException(validation_message(exc)).to_payload()
        return JSONResponse(status_code=400, content=payload)
