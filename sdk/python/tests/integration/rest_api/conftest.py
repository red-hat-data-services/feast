import os
import time
from pathlib import Path

import pytest
import requests
from kubernetes import client, config

from tests.integration.rest_api.support import (
    applyFeastProject,
    copy_secret,
    create_data_registry_route,
    create_feast_project,
    create_namespace,
    create_route,
    create_sa_token,
    delete_namespace,
    deploy_and_validate_pod,
    execPodCommand,
    get_pod_name_by_prefix,
    label_namespace,
    run_kubectl_command,
    validate_feature_store_cr_status,
    wait_for_data_registry_ready,
)


class FeastRestClient:
    """HTTP client for Feast REST API endpoints."""

    def __init__(self, base_url, token=None):
        self.base_url = base_url.rstrip("/")
        self.api_prefix = "/api/v1"
        self.token = token

    def _build_url(self, endpoint):
        if not endpoint.startswith("/"):
            endpoint = "/" + endpoint
        return f"{self.base_url}{self.api_prefix}{endpoint}"

    def _headers(self):
        if self.token:
            return {"Authorization": f"Bearer {self.token}"}
        return {}

    def get(self, endpoint, params=None):
        params = params or {}
        params.setdefault("allow_cache", "false")
        url = self._build_url(endpoint)
        return requests.get(url, params=params, headers=self._headers(), verify=False)

    def post(self, endpoint, json=None, params=None):
        url = self._build_url(endpoint)
        return requests.post(
            url, json=json, params=params, headers=self._headers(), verify=False
        )

    def delete(self, endpoint, params=None):
        url = self._build_url(endpoint)
        return requests.delete(
            url, params=params, headers=self._headers(), verify=False
        )


class DataRegistryClient:
    """HTTPS client for Data Registry API endpoints (behind kube-rbac-proxy)."""

    def __init__(self, base_url, token=None):
        self.base_url = base_url.rstrip("/")
        self.token = token

    def _headers(self):
        if self.token:
            return {"Authorization": f"Bearer {self.token}"}
        return {}

    def get(self, endpoint, params=None):
        url = f"{self.base_url}{endpoint}"
        return requests.get(url, params=params, headers=self._headers(), verify=False)

    def post(self, endpoint, json=None, params=None):
        url = f"{self.base_url}{endpoint}"
        return requests.post(
            url, json=json, params=params, headers=self._headers(), verify=False
        )

    def put(self, endpoint, json=None, params=None):
        url = f"{self.base_url}{endpoint}"
        return requests.put(
            url, json=json, params=params, headers=self._headers(), verify=False
        )

    def patch(self, endpoint, json=None, params=None):
        url = f"{self.base_url}{endpoint}"
        return requests.patch(
            url, json=json, params=params, headers=self._headers(), verify=False
        )

    def delete(self, endpoint, params=None):
        url = f"{self.base_url}{endpoint}"
        return requests.delete(
            url, params=params, headers=self._headers(), verify=False
        )

    def head(self, endpoint, params=None):
        url = f"{self.base_url}{endpoint}"
        return requests.head(url, params=params, headers=self._headers(), verify=False)


def _wait_for_http_ready(
    route_url: str,
    health_path: str = "/api/v1/projects",
    timeout: int = 300,
    interval: int = 5,
    initial_delay: int = 30,
    verify_ssl: bool = False,
    headers: dict = None,
) -> None:
    """Poll an HTTP endpoint until it returns a non-502 response."""
    health_url = f"{route_url}{health_path}"
    last_status = None

    if initial_delay > 0:
        print(f"\n Waiting {initial_delay}s for backend to start...")
        time.sleep(initial_delay)

    deadline = time.time() + timeout
    print(
        f"\n Waiting for HTTP endpoint to become ready (timeout={timeout}s): {health_url}"
    )

    while time.time() < deadline:
        try:
            resp = requests.get(
                health_url, timeout=10, verify=verify_ssl, headers=headers or {}
            )
            last_status = resp.status_code
            if resp.status_code != 502:
                print(f" HTTP endpoint is ready (status={resp.status_code})")
                return
            print(
                f" HTTP endpoint returned {resp.status_code}, retrying in {interval}s..."
            )
        except requests.exceptions.RequestException as exc:
            last_status = str(exc)
            print(f" HTTP request failed ({exc}), retrying in {interval}s...")

        time.sleep(interval)

    raise RuntimeError(
        f"HTTP endpoint {health_url} did not become ready within {timeout}s "
        f"(last status: {last_status})"
    )


# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
NAMESPACE = "test-ns-feast-rest"
DATA_REGISTRY_NAMESPACE = "rhoai-data-registry"
CREDIT_SCORING = "credit-scoring"
DRIVER_RANKING = "driver-ranking"
FEAST_AUTH = "feast-auth"
DATA_REGISTRY = "data-registry"

CREDIT_SCORING_REST_SERVICE = "feast-credit-scoring-registry-rest"
FEAST_AUTH_REST_SERVICE = "feast-feast-auth-registry-rest"


# ---------------------------------------------------------------------------
# Base fixture: feast_rest_client
# Deploys ONLY credit-scoring + driver-ranking (the original behavior).
# Existing test_registry_rest_api.py depends only on this.
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
def feast_rest_client():
    """Deploy credit-scoring + driver-ranking CRs and yield a FeastRestClient."""
    config.load_kube_config()
    api_instance = client.CoreV1Api()

    test_dir = Path(__file__).parent
    resource_dir = test_dir / "resource"

    run_on_openshift = os.getenv("RUN_ON_OPENSHIFT_CI", "false").lower() == "true"

    create_namespace(api_instance, NAMESPACE)

    try:
        # Deploy shared infrastructure
        deploy_and_validate_pod(
            NAMESPACE, str(resource_dir / "redis.yaml"), "app=redis"
        )
        deploy_and_validate_pod(
            NAMESPACE, str(resource_dir / "postgres.yaml"), "app=postgres"
        )

        # Deploy the two noAuth FeatureStore CRs
        create_feast_project(
            str(resource_dir / "feast_config_credit_scoring.yaml"),
            NAMESPACE,
            CREDIT_SCORING,
        )
        validate_feature_store_cr_status(NAMESPACE, CREDIT_SCORING)

        create_feast_project(
            str(resource_dir / "feast_config_driver_ranking.yaml"),
            NAMESPACE,
            DRIVER_RANKING,
        )
        validate_feature_store_cr_status(NAMESPACE, DRIVER_RANKING)

        if run_on_openshift:
            route_url = create_route(
                NAMESPACE, CREDIT_SCORING, CREDIT_SCORING_REST_SERVICE
            )
        else:
            run_kubectl_command(
                [
                    "apply",
                    "-f",
                    str(resource_dir / "feast-registry-nginx.yaml"),
                    "-n",
                    NAMESPACE,
                ]
            )
            ingress_host = run_kubectl_command(
                [
                    "get",
                    "ingress",
                    "feast-registry-ingress",
                    "-n",
                    NAMESPACE,
                    "-o",
                    "jsonpath={.spec.rules[0].host}",
                ]
            )
            route_url = f"http://{ingress_host}"

        # Apply feast projects
        applyFeastProject(NAMESPACE, CREDIT_SCORING)
        applyFeastProject(NAMESPACE, DRIVER_RANKING)

        # Create saved datasets and permissions on credit-scoring
        pod_name = get_pod_name_by_prefix(NAMESPACE, CREDIT_SCORING)
        execPodCommand(NAMESPACE, pod_name, ["python", "create_ui_visible_datasets.py"])
        execPodCommand(NAMESPACE, pod_name, ["python", "permissions_apply.py"])

        if not route_url:
            raise RuntimeError("Route URL could not be fetched.")

        _wait_for_http_ready(route_url)

        print(f"\n Connected to Feast REST at: {route_url}")
        yield FeastRestClient(route_url)

    finally:
        print(f"\n Deleting namespace: {NAMESPACE}")
        delete_namespace(api_instance, NAMESPACE)


# ---------------------------------------------------------------------------
# Extended fixture: feast_extended_setup
# Deploys feast-auth + data-registry + RBAC on top of the base setup.
# New test modules (auth, DR, isolation) depend on this.
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
def feast_extended_setup(feast_rest_client):
    """Deploy feast-auth CR, data-registry CR, and RBAC resources.

    Depends on feast_rest_client so that the namespace, shared infra
    (redis, postgres), and base CRs (credit-scoring, driver-ranking) are
    already deployed.  Returns the base feast_rest_client for convenience.
    """
    resource_dir = Path(__file__).parent / "resource"
    api_instance = client.CoreV1Api()

    # Apply RBAC ServiceAccounts + RoleBindings
    run_kubectl_command(
        ["apply", "-f", str(resource_dir / "rbac_setup.yaml"), "-n", NAMESPACE]
    )

    # Deploy feast-auth CR
    create_feast_project(
        str(resource_dir / "feast_config_feast_auth.yaml"), NAMESPACE, FEAST_AUTH
    )
    validate_feature_store_cr_status(NAMESPACE, FEAST_AUTH)

    # Apply feast project inside feast-auth pod
    applyFeastProject(NAMESPACE, FEAST_AUTH)

    # Create saved datasets and permissions on feast-auth
    auth_pod_name = get_pod_name_by_prefix(NAMESPACE, FEAST_AUTH)
    execPodCommand(
        NAMESPACE, auth_pod_name, ["python", "create_ui_visible_datasets.py"]
    )
    execPodCommand(NAMESPACE, auth_pod_name, ["python", "permissions_apply.py"])

    # Data Registry CRs are gated to rhoai-data-registry. Create that
    # namespace, copy the shared Postgres secrets, then apply the CR there.
    # Catalog URL paths still use NAMESPACE (test-ns-feast-rest) because
    # kube-rbac-proxy SAR treats {project} as the tenant Kubernetes namespace.
    create_namespace(api_instance, DATA_REGISTRY_NAMESPACE)
    label_namespace(DATA_REGISTRY_NAMESPACE, "opendatahub.io/data-registry", "true")
    copy_secret(NAMESPACE, DATA_REGISTRY_NAMESPACE, "postgres-secret")
    copy_secret(NAMESPACE, DATA_REGISTRY_NAMESPACE, "feast-data-stores")

    run_kubectl_command(
        [
            "apply",
            "-f",
            str(resource_dir / "feast_config_data_registry.yaml"),
            "-n",
            DATA_REGISTRY_NAMESPACE,
        ]
    )
    try:
        wait_for_data_registry_ready(
            DATA_REGISTRY_NAMESPACE, DATA_REGISTRY, timeout_seconds=900
        )
        yield feast_rest_client
    finally:
        print(
            f"\n Deleting Data Registry CR {DATA_REGISTRY} from {DATA_REGISTRY_NAMESPACE}"
        )
        run_kubectl_command(
            [
                "delete",
                "feast",
                DATA_REGISTRY,
                "-n",
                DATA_REGISTRY_NAMESPACE,
                "--ignore-not-found",
            ]
        )


# ---------------------------------------------------------------------------
# Token fixtures
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
def admin_token(feast_extended_setup):
    """Bearer token for the feast-test-admin ServiceAccount."""
    return create_sa_token(NAMESPACE, "feast-test-admin")


@pytest.fixture(scope="session")
def viewer_token(feast_extended_setup):
    """Bearer token for the feast-test-viewer ServiceAccount."""
    return create_sa_token(NAMESPACE, "feast-test-viewer")


@pytest.fixture(scope="session")
def unauthorized_token(feast_extended_setup):
    """Bearer token for the feast-test-unauthorized ServiceAccount (no RBAC bindings)."""
    return create_sa_token(NAMESPACE, "feast-test-unauthorized")


# ---------------------------------------------------------------------------
# Auth-enabled Feast fixtures
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
def feast_auth_route_url(feast_extended_setup):
    """Route URL for the feast-auth CR's registry-rest service."""
    run_on_openshift = os.getenv("RUN_ON_OPENSHIFT_CI", "false").lower() == "true"
    if not run_on_openshift:
        pytest.skip("feast-auth route requires OpenShift")

    url = create_route(NAMESPACE, FEAST_AUTH, FEAST_AUTH_REST_SERVICE)
    if not url:
        raise RuntimeError("Could not create route for feast-auth")
    return url


@pytest.fixture(scope="session")
def feast_auth_client(feast_auth_route_url, admin_token):
    """FeastRestClient for the auth-enabled Feast CR, using the admin SA token."""
    _wait_for_http_ready(
        feast_auth_route_url,
        headers={"Authorization": f"Bearer {admin_token}"},
    )
    return FeastRestClient(feast_auth_route_url, token=admin_token)


@pytest.fixture(scope="session")
def feast_auth_viewer_client(feast_auth_route_url, viewer_token):
    """FeastRestClient for the auth-enabled Feast CR, using the viewer SA token."""
    return FeastRestClient(feast_auth_route_url, token=viewer_token)


@pytest.fixture(scope="session")
def feast_auth_unauthorized_client(feast_auth_route_url, unauthorized_token):
    """FeastRestClient for the auth-enabled Feast CR, using an unauthorized SA token."""
    return FeastRestClient(feast_auth_route_url, token=unauthorized_token)


@pytest.fixture(scope="session")
def feast_auth_no_token_client(feast_auth_route_url):
    """FeastRestClient for the auth-enabled Feast CR, with no token (expect 401)."""
    return FeastRestClient(feast_auth_route_url, token=None)


# ---------------------------------------------------------------------------
# Data Registry fixtures
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
def data_registry_route_url(feast_extended_setup):
    """Route URL for the Data Registry CR's HTTPS service."""
    run_on_openshift = os.getenv("RUN_ON_OPENSHIFT_CI", "false").lower() == "true"
    if not run_on_openshift:
        pytest.skip("Data Registry route requires OpenShift")

    url = create_data_registry_route(DATA_REGISTRY_NAMESPACE, DATA_REGISTRY)
    return url


@pytest.fixture(scope="session")
def data_registry_client(data_registry_route_url, admin_token):
    """DataRegistryClient for the Data Registry, using the admin SA token."""
    _wait_for_http_ready(
        data_registry_route_url,
        health_path="/projects",
        headers={"Authorization": f"Bearer {admin_token}"},
        initial_delay=10,
    )
    return DataRegistryClient(data_registry_route_url, token=admin_token)


@pytest.fixture(scope="session")
def data_registry_no_token_client(data_registry_route_url):
    """DataRegistryClient with no token (expect 401 from kube-rbac-proxy)."""
    return DataRegistryClient(data_registry_route_url, token=None)


@pytest.fixture(scope="session")
def data_registry_unauthorized_client(data_registry_route_url, unauthorized_token):
    """DataRegistryClient with an unauthorized SA token (expect 403)."""
    return DataRegistryClient(data_registry_route_url, token=unauthorized_token)
