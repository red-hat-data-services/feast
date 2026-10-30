FROM registry.access.redhat.com/ubi9/python-312-minimal:latest

USER 0
RUN microdnf install -y git gcc libpq-devel python3.12-devel && microdnf clean all
RUN microdnf install -y crypto-policies-scripts && \
    update-crypto-policies --set DEFAULT:PQ && \
    microdnf clean all
USER 1001

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv
ENV UV_CACHE_DIR=/tmp/uv-cache
COPY sdk/python/feast/infra/feature_servers/multicloud/requirements.txt requirements.txt

# Power (ppc64le) only: pin pyarrow, re2, and grpcio via IBM wheels
RUN set -eux; \
    if [ "$(uname -m)" = "ppc64le" ]; then \
        echo "Power architecture detected. Installing pinned pyarrow and re2 version..."; \
        /prebuild-power-base.sh; \
    else \
        echo "Non-Power architecture detected; skipping Power-specific pyarrow installation."; \
    fi

# On ppc64le: feast[minimal] extras minus milvus (no faiss-cpu wheel).
# Keep the version pin from requirements.txt; only swap extras.
# On other arches: standard requirements.txt install.
RUN set -eux; \
    if [ "$(uname -m)" = "ppc64le" ]; then \
        test -s /power-constraints.txt; \
        sed -E 's/^feast\[minimal\]/feast[aws,gcp,snowflake,redis,go,mysql,postgres-c,opentelemetry,grpcio,k8s,duckdb,mcp,mcp-server]/' \
            requirements.txt > /tmp/requirements-ppc64le.txt; \
        grep -q '^feast\[aws,' /tmp/requirements-ppc64le.txt; \
        uv pip install --index-strategy unsafe-best-match \
            --extra-index-url https://wheels.developerfirst.ibm.com/ppc64le/linux \
            -c /power-constraints.txt \
            -r /tmp/requirements-ppc64le.txt; \
    else \
        uv pip install -r requirements.txt; \
    fi

# Replace PyPI feast with this git checkout so /healthz, catalog APIs, and
# Data Registry SAR match the commit Konflux is testing.
COPY sdk/python/feast /tmp/feast-overlay
RUN python3 -c "\
import feast, os, shutil; \
dst = os.path.dirname(feast.__file__); \
shutil.copytree('/tmp/feast-overlay', dst, dirs_exist_ok=True); \
print('overlaid feast package at', dst)"

# modify permissions to support running with a random uid
RUN chmod g+w $(python -c "import feast.ui as ui; print(ui.__path__)" | tr -d "[']")/build/projects-list.json
