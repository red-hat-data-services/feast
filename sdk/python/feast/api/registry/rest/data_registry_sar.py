"""SubjectAccessReview (SAR) filtering for Data Registry endpoints.

When ``DATA_REGISTRY_SAR_API_GROUP`` is set, Feast feature-registry
``GET /projects`` filters the project list with per-namespace
SubjectAccessReview. Data Registry routes use path ``{project}`` (Kubernetes
namespace) and are not listed here; OpenAPI 0.8 removed Data Registry
``GET /v1/projects``.

Environment variables (set by the Feast operator):
    DATA_REGISTRY_SAR_API_GROUP: API group for SAR checks.
    DATA_REGISTRY_SAR_RESOURCES: Comma-separated resources to check.
    DATA_REGISTRY_SAR_CACHE_TTL_SECONDS: Authorization cache lifetime.
"""

import logging
import os
import time
from typing import Dict, List, Optional, Tuple

from fastapi import Request

logger = logging.getLogger(__name__)

_SAR_API_GROUP = os.getenv("DATA_REGISTRY_SAR_API_GROUP", "")
_SAR_RESOURCES = os.getenv("DATA_REGISTRY_SAR_RESOURCES", "namespaces").split(",")
_SAR_CACHE_TTL = int(os.getenv("DATA_REGISTRY_SAR_CACHE_TTL_SECONDS", "30"))

_access_cache: Dict[Tuple[str, str], Tuple[bool, float]] = {}


def is_data_registry_sar_enabled() -> bool:
    return bool(_SAR_API_GROUP)


def extract_bearer_token(request: Request) -> Optional[str]:
    auth_header = request.headers.get("authorization", "")
    if auth_header.lower().startswith("bearer "):
        return auth_header[7:]
    return None


def filter_projects_by_sar(
    projects: List[Dict],
    bearer_token: str,
    resource: str = "namespaces",
    verb: str = "list",
) -> List[Dict]:
    """
    Filter a list of projects (namespaces) by performing SubjectAccessReview
    for each one. Returns only projects where the user is authorized.
    """
    if not is_data_registry_sar_enabled():
        return projects

    try:
        from kubernetes import client, config

        config.load_incluster_config()
        authz_api = client.AuthorizationV1Api()
    except Exception as e:
        logger.error(f"Failed to initialize K8s client for SAR: {e}")
        return []

    permitted = []
    for project in projects:
        project_name = project.get("spec", {}).get("name", "")
        if not project_name:
            continue

        if _check_access_cached(authz_api, bearer_token, project_name, resource, verb):
            permitted.append(project)

    logger.debug(f"SAR filter: {len(permitted)}/{len(projects)} projects permitted")
    return permitted


def _check_access_cached(
    authz_api,
    bearer_token: str,
    namespace: str,
    resource: str,
    verb: str,
) -> bool:
    cache_key = (_token_fingerprint(bearer_token), f"{namespace}/{resource}/{verb}")
    now = time.time()

    cached = _access_cache.get(cache_key)
    if cached and (now - cached[1]) < _SAR_CACHE_TTL:
        return cached[0]

    result = _do_sar_check(authz_api, bearer_token, namespace, resource, verb)
    _access_cache[cache_key] = (result, now)

    if len(_access_cache) > 10000:
        _evict_cache(now)

    return result


def _do_sar_check(
    authz_api,
    bearer_token: str,
    namespace: str,
    resource: str,
    verb: str,
) -> bool:
    """Perform a SubjectAccessReview impersonating the caller via their token."""
    from kubernetes import client

    try:
        token_review = client.V1TokenReview(
            spec=client.V1TokenReviewSpec(token=bearer_token)
        )
        auth_api = client.AuthenticationV1Api()
        tr_response = auth_api.create_token_review(token_review)

        token_status = tr_response.status
        if token_status is None or not token_status.authenticated:
            return False

        user = token_status.user
        if user is None or not user.username:
            return False

        username = user.username
        groups = user.groups or []

        sar = client.V1SubjectAccessReview(
            spec=client.V1SubjectAccessReviewSpec(
                user=username,
                groups=groups,
                resource_attributes=client.V1ResourceAttributes(
                    namespace=namespace,
                    verb=verb,
                    group=_SAR_API_GROUP,
                    resource=resource,
                ),
            )
        )

        response = authz_api.create_subject_access_review(sar)
        return response.status.allowed

    except Exception as e:
        logger.error(f"SAR check failed for {namespace}/{resource}/{verb}: {e}")
        return False


def _token_fingerprint(token: str) -> str:
    """Short hash of token for cache keying (avoids storing full tokens in memory)."""
    import hashlib

    return hashlib.sha256(token.encode()).hexdigest()[:16]


_CACHE_MAX = 10000
_CACHE_TARGET = 7500  # trim to 75 % capacity after eviction


def _evict_cache(now: float) -> None:
    """Remove expired entries first; if the cache is still above _CACHE_TARGET,
    evict the oldest entries by insertion/refresh timestamp to enforce the cap."""
    expired = [
        k for k, (_, ts) in _access_cache.items() if (now - ts) >= _SAR_CACHE_TTL
    ]
    for k in expired:
        del _access_cache[k]

    if len(_access_cache) > _CACHE_TARGET:
        overflow = len(_access_cache) - _CACHE_TARGET
        oldest = sorted(_access_cache, key=lambda k: _access_cache[k][1])[:overflow]
        for k in oldest:
            del _access_cache[k]
