from fastapi import APIRouter, Depends, HTTPException, Query, Request, Response, status

from feast.api.registry.rest.data_registry_sar import (
    extract_bearer_token,
    filter_projects_by_sar,
    is_data_registry_sar_enabled,
)
from feast.api.registry.rest.rest_utils import (
    get_pagination_params,
    get_sorting_params,
    grpc_call,
    list_all_projects,
)
from feast.protos.feast.registry import RegistryServer_pb2


def get_project_router(grpc_handler) -> APIRouter:
    router = APIRouter()

    @router.get("/projects/{name}")
    def get_project(
        name: str,
        allow_cache: bool = Query(True),
    ):
        req = RegistryServer_pb2.GetProjectRequest(
            name=name,
            allow_cache=allow_cache,
        )
        project = grpc_call(grpc_handler.GetProject, req)

        return project

    @router.get("/projects")
    def list_projects(
        request: Request,
        allow_cache: bool = Query(True),
        pagination_params: dict = Depends(get_pagination_params),
        sorting_params: dict = Depends(get_sorting_params),
    ):
        try:
            projects, pagination, err_msg = list_all_projects(
                grpc_handler=grpc_handler,
                allow_cache=allow_cache,
                pagination_params=pagination_params,
                sorting_params=sorting_params,
            )
        except Exception as e:
            return {"error": str(e)}

        if err_msg:
            return {"error": err_msg}

        if is_data_registry_sar_enabled():
            token = extract_bearer_token(request)
            if not token:
                raise HTTPException(status_code=401, detail="Bearer token required")
            projects = filter_projects_by_sar(projects, token)

        return {
            "projects": projects,
            "pagination": pagination,
        }

    return router


def get_registry_router(store) -> APIRouter:
    router = APIRouter()

    @router.post("/registry/refresh")
    def refresh_registry():
        from feast.permissions.action import AuthzedAction
        from feast.permissions.security_manager import assert_permissions

        project = store.registry.get_project(name=store.project, allow_cache=True)
        assert_permissions(resource=project, actions=[AuthzedAction.UPDATE])
        store.refresh_registry()
        return Response(status_code=status.HTTP_200_OK)

    return router
