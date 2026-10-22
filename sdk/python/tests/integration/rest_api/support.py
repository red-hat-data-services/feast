import json
import shlex
import subprocess
import tempfile
import time

from kubernetes import client


def run_command(cmd, cwd=None, check=True):
    print(f"Running command: {' '.join(cmd)}")
    result = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, check=True)
    if check and result.returncode != 0:
        print(result.stdout)
        print(result.stderr)
        raise RuntimeError(f"Command failed: {' '.join(cmd)}")
    return result.stdout.strip()


def wait_for_namespace_gone(namespace_name, timeout_seconds=180, interval_seconds=5):
    """Block until kubectl no longer sees the namespace (Terminating finished)."""
    print(f"Waiting for namespace {namespace_name} to be fully deleted...")
    start_time = time.time()
    while time.time() - start_time < timeout_seconds:
        result = subprocess.run(
            ["kubectl", "get", "ns", namespace_name],
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            print(f"Namespace {namespace_name} is gone")
            return True
        time.sleep(interval_seconds)
    raise TimeoutError(
        f"Namespace {namespace_name} still exists after {timeout_seconds} seconds"
    )


def create_namespace(api_instance, namespace_name, timeout_seconds=180):
    namespace_body = client.V1Namespace(
        metadata=client.V1ObjectMeta(name=namespace_name)
    )
    try:
        api_instance.create_namespace(namespace_body)
        print(f"Created namespace {namespace_name}")
        return True
    except Exception as e:
        status = getattr(e, "status", None)
        print(f"Kubernetes API error during namespace creation: {e}")
        if status != 409:
            raise
        phase = run_kubectl_command(
            ["get", "ns", namespace_name, "-o", "jsonpath={.status.phase}"]
        )
        if phase == "Terminating":
            wait_for_namespace_gone(namespace_name, timeout_seconds=timeout_seconds)
            api_instance.create_namespace(namespace_body)
            print(f"Created namespace {namespace_name} after previous delete finished")
            return True
        print(f"Reusing existing namespace {namespace_name} (phase={phase})")
        return True


def delete_namespace(api_instance, namespace_name, timeout_seconds=180):
    try:
        api_instance.delete_namespace(namespace_name)
    except Exception as e:
        status = getattr(e, "status", None)
        print(f"Kubernetes API error during namespace deletion: {e}")
        if status == 404:
            return True
        raise
    wait_for_namespace_gone(namespace_name, timeout_seconds=timeout_seconds)
    return True


def dump_namespace_diagnostics(namespace, cr_name=None):
    """Print FeatureStore status, pods, and events so a timeout is diagnosable."""
    print(f"=== diagnostics for namespace {namespace} ===")
    feast_list = run_kubectl_command(["get", "feast", "-n", namespace, "-o", "wide"])
    print(f"  FeatureStores:\n{feast_list}")
    if cr_name:
        feast_yaml = run_kubectl_command(
            ["get", "feast", cr_name, "-n", namespace, "-o", "yaml"]
        )
        print(f"  FeatureStore {cr_name}:\n{feast_yaml}")
    pods = run_kubectl_command(["get", "pods", "-n", namespace, "-o", "wide"])
    print(f"  Pods:\n{pods}")
    events = run_kubectl_command(
        ["get", "events", "-n", namespace, "--sort-by=.lastTimestamp"]
    )
    print(f"  Events:\n{events}")


def run_kubectl_command(args):
    try:
        result = subprocess.run(
            ["kubectl"] + args, capture_output=True, text=True, check=True
        )
        return result.stdout.strip()
    except subprocess.CalledProcessError as e:
        print(f"Error executing kubectl (exit {e.returncode})")
        return None


def run_oc_command(args):
    try:
        result = subprocess.run(
            ["oc"] + args,
            capture_output=True,
            text=True,
            check=True,
        )
        return result.stdout.strip()
    except subprocess.CalledProcessError as e:
        print(f"Error running oc command: {' '.join(args)}")
        print(e.stderr)
        return None


def validate_feature_store_cr_status(
    namespace, cr_name, timeout_seconds=300, interval_seconds=5
):
    print(
        f"Waiting for FeatureStore CR {namespace}/{cr_name} to reach 'Ready' status..."
    )
    start_time = time.time()

    while time.time() - start_time < timeout_seconds:
        status = run_kubectl_command(
            ["get", "feast", cr_name, "-n", namespace, "-o", "jsonpath={.status.phase}"]
        )
        if status == "Ready":
            print(f"Feature Store CR {namespace}/{cr_name} is in Ready state")
            return True
        print(f"  Phase={status}")
        time.sleep(interval_seconds)

    dump_namespace_diagnostics(namespace, cr_name)
    raise TimeoutError(
        f" Feature Store CR {namespace}/{cr_name} did not reach 'Ready' state within {timeout_seconds} seconds."
    )


def create_feast_project(project_path, namespace, feast_instance_name):
    print(f"Applying Feast project from {project_path} in namespace {namespace}...")
    apply_result = run_kubectl_command(["apply", "-f", project_path, "-n", namespace])

    if apply_result is None:
        raise RuntimeError(f" Failed to apply Feast project: {project_path}")

    print("Feast CR applied successfully. Validating status...")
    return validate_feature_store_cr_status(namespace, feast_instance_name)


def wait_for_pods_ready(
    namespace, label_selector, timeout_seconds=180, interval_seconds=5
):
    print(
        f"Waiting for pods with label '{label_selector}' in namespace '{namespace}' to be Running..."
    )
    start_time = time.time()

    while time.time() - start_time < timeout_seconds:
        pods_output = run_kubectl_command(
            [
                "get",
                "pods",
                "-n",
                namespace,
                "-l",
                label_selector,
                "-o",
                "jsonpath={.items[*].status.phase}",
            ]
        )

        if pods_output:
            statuses = pods_output.split()
            if all(status == "Running" for status in statuses):
                print(
                    f"Pods with label '{label_selector}' are Running in namespace '{namespace}'."
                )
                return True
            else:
                print(f"Current pod statuses: {statuses}")

        time.sleep(interval_seconds)
    dump_namespace_diagnostics(namespace)
    raise TimeoutError(
        f" Pods did not reach Running state in {timeout_seconds} seconds."
    )


def deploy_and_validate_pod(namespace, deployment_yaml_path, label_selector):
    print(f"Applying deployment: {deployment_yaml_path} in namespace: {namespace}")
    apply_result = run_kubectl_command(
        ["apply", "-f", deployment_yaml_path, "-n", namespace]
    )

    if apply_result is None:
        raise RuntimeError(f"Failed to apply deployment YAML: {deployment_yaml_path}")

    return wait_for_pods_ready(namespace, label_selector)


def create_route(namespace, feast_project, service_name):
    create_args = [
        "create",
        "route",
        "passthrough",
        feast_project + "-registry-rest",
        "--service=" + service_name,
        "--port=https",
        "-n",
        namespace,
    ]
    create_output = run_oc_command(create_args)
    if create_output is None:
        return None
    print(f"Created route:\n{create_output}")

    get_args = [
        "get",
        "route",
        "-n",
        namespace,
        "-o",
        f"jsonpath={{.items[?(@.spec.to.name=='{service_name}')].spec.host}}",
    ]
    host = run_oc_command(get_args)
    if not host:
        print("Failed to get route host.")
        return None

    route_url = f"https://{host}"
    print(f"Route URL: {route_url}")
    return route_url


def run_kubectl_apply_with_sed(
    aws_access_key,
    aws_secret_key,
    aws_bucket_name,
    aws_feast_registry_path,
    template_path,
    namespace,
):
    if not aws_access_key or not aws_secret_key or not namespace:
        raise ValueError("Missing required values.")

    # Escape values to avoid shell issues
    escaped_access_key = shlex.quote(aws_access_key)
    escaped_secret_key = shlex.quote(aws_secret_key)
    escaped_namespace = shlex.quote(namespace)
    escaped_aws_bucket_name = shlex.quote(aws_bucket_name)
    escaped_aws_feast_registry_path = shlex.quote(aws_feast_registry_path)
    # Build sed command with pipeline
    cmd = (
        f"sed 's|AWS_ACCESS_KEY_PLACEHOLDER|{escaped_access_key}|g' {template_path} "
        f"| sed 's|AWS_SECRET_KEY_PLACEHOLDER|{escaped_secret_key}|g' "
        f"| sed 's|NAMESPACE_PLACEHOLDER|{escaped_namespace}|g' "
        f"| sed 's|AWS_BUCKET_NAME|{escaped_aws_bucket_name}|g' "
        f"| sed 's|REGISTRY_FILE_PATH|{escaped_aws_feast_registry_path}|g' "
        f"| kubectl apply -f -"
    )
    print("Running shell command:\n", cmd)
    try:
        result = subprocess.run(
            cmd, shell=True, check=True, capture_output=True, text=True
        )
        print(result.stdout.strip())
        return result.stdout.strip()
    except subprocess.CalledProcessError as e:
        print("Error applying manifest:")
        print(e.stderr)
        raise


def get_pod_name_by_prefix(namespace, prefix):
    args = [
        "get",
        "pods",
        "-n",
        namespace,
        "--no-headers",
        "-o",
        "custom-columns=:metadata.name",
    ]
    pods = run_kubectl_command(args)
    if pods:
        for line in pods.splitlines():
            if prefix in line:
                return line.strip()
    return None


def applyFeastProject(namespace, feast_project):
    pod_name = get_pod_name_by_prefix(namespace, feast_project)
    apply_output = execPodCommand(namespace, pod_name, ["feast", "apply"])
    return apply_output


def execPodCommand(namespace, podName, command_args):
    apply_args = ["exec", podName, "-n", namespace, "--"] + command_args
    apply_output = run_kubectl_command(apply_args)
    print("Output of args apply:\n", apply_output)
    return apply_output


def copy_secret(src_namespace, dest_namespace, secret_name):
    """Copy a Secret from src_namespace into dest_namespace, stripping cluster metadata."""
    raw = run_kubectl_command(
        ["get", "secret", secret_name, "-n", src_namespace, "-o", "json"]
    )
    if not raw:
        raise RuntimeError(
            f"Failed to get secret {secret_name} from namespace {src_namespace}"
        )
    data = json.loads(raw)
    data["metadata"] = {
        "name": secret_name,
        "namespace": dest_namespace,
    }
    data.pop("status", None)
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as handle:
        json.dump(data, handle)
        path = handle.name
    result = run_kubectl_command(["apply", "-f", path])
    if result is None:
        raise RuntimeError(
            f"Failed to apply secret {secret_name} into namespace {dest_namespace}"
        )
    print(f"Copied object from {src_namespace} to {dest_namespace}")
    return result


def label_namespace(namespace, label_key, label_value):
    """Add a label to a Kubernetes namespace."""
    args = [
        "label",
        "namespace",
        namespace,
        f"{label_key}={label_value}",
        "--overwrite",
    ]
    result = run_kubectl_command(args)
    if result is None:
        result = run_oc_command(
            [
                "label",
                "namespace",
                namespace,
                f"{label_key}={label_value}",
                "--overwrite",
            ]
        )
    print(f"Labeled namespace {namespace}: {label_key}={label_value}")
    return result


def create_sa_token(namespace, sa_name, duration="1h"):
    """Create a token for a ServiceAccount using kubectl/oc."""
    args = ["create", "token", sa_name, "-n", namespace, f"--duration={duration}"]
    token = run_kubectl_command(args)
    if not token:
        token = run_oc_command(
            ["create", "token", sa_name, "-n", namespace, f"--duration={duration}"]
        )
    if not token:
        raise RuntimeError(f"Failed to create token for SA {sa_name} in {namespace}")
    print(f"Created token for SA {sa_name} in {namespace} (length={len(token)})")
    return token


def create_data_registry_route(
    namespace, cr_name, timeout_seconds=120, interval_seconds=5
):
    """Return the URL of the operator-created Data Registry Route.

    The operator creates a ReEncrypt Route named feast-{cr}-data-registry
    (GetFeastServiceName(DataRegistryFeastType)) targeting kube-rbac-proxy.
    Tests must use that Route so they exercise the product ingress. Do not
    create a second passthrough Route: listing all Routes for the Service
    concatenates hosts and breaks HTTP clients.
    """
    route_name = f"feast-{cr_name}-data-registry"
    get_args = [
        "get",
        "route",
        route_name,
        "-n",
        namespace,
        "-o",
        "jsonpath={.spec.host}",
    ]
    print(
        f"Waiting for operator Data Registry Route {namespace}/{route_name} "
        f"(timeout={timeout_seconds}s)..."
    )
    start_time = time.time()
    host = None
    while time.time() - start_time < timeout_seconds:
        host = run_oc_command(get_args)
        if not host:
            host = run_kubectl_command(get_args)
        if host and " " not in host.strip():
            route_url = f"https://{host.strip()}"
            print(f"Data Registry Route URL: {route_url}")
            return route_url
        if host and " " in host.strip():
            raise RuntimeError(
                f"Operator Route {namespace}/{route_name} host contains a space "
                f"({host!r}). Refusing to use a concatenated multi-Route host."
            )
        print(f"  Route host not ready yet (got {host!r})")
        time.sleep(interval_seconds)

    dump_namespace_diagnostics(namespace, cr_name)
    raise RuntimeError(
        f"Operator Data Registry Route {namespace}/{route_name} did not get a "
        f"host within {timeout_seconds}s. The operator should create this "
        f"ReEncrypt Route; tests must not create a replacement."
    )


def wait_for_data_registry_ready(
    namespace, cr_name, timeout_seconds=600, interval_seconds=10
):
    """Wait for a Data Registry FeatureStore CR to reach Ready state.

    Data Registry CRs take longer because the operator must create additional
    resources (kube-rbac-proxy sidecar, auth ConfigMap, ClusterRoles, etc.)
    and wait for TLS cert provisioning by service-ca.
    """
    print(
        f"Waiting for Data Registry CR {namespace}/{cr_name} to reach Ready (timeout={timeout_seconds}s)..."
    )
    start_time = time.time()

    while time.time() - start_time < timeout_seconds:
        phase = run_kubectl_command(
            ["get", "feast", cr_name, "-n", namespace, "-o", "jsonpath={.status.phase}"]
        )
        if phase == "Ready":
            print(f"Data Registry CR {namespace}/{cr_name} is Ready")
            return True

        conditions = run_kubectl_command(
            [
                "get",
                "feast",
                cr_name,
                "-n",
                namespace,
                "-o",
                "jsonpath={.status.conditions[?(@.type=='DataRegistryReady')].status}",
            ]
        )
        print(f"  Phase={phase}, DataRegistryReady={conditions}")
        time.sleep(interval_seconds)

    dump_namespace_diagnostics(namespace, cr_name)
    raise TimeoutError(
        f"Data Registry CR {namespace}/{cr_name} did not reach Ready within {timeout_seconds}s"
    )
