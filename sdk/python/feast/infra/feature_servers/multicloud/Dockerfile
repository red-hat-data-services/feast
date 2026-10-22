FROM registry.access.redhat.com/ubi9/python-312-minimal:latest

USER 0
RUN microdnf install -y git gcc libpq-devel python3.12-devel && microdnf clean all
USER 1001

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv
ENV UV_CACHE_DIR=/tmp/uv-cache
COPY sdk/python/feast/infra/feature_servers/multicloud/requirements.txt requirements.txt
RUN uv pip install -r requirements.txt

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
