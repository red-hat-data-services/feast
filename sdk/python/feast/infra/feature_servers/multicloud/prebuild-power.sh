#!/bin/bash
set -Eeuo pipefail
trap 'echo "[prebuild-power] failed at line $LINENO"; exit 1' ERR

IBM_PYPI_INDEX="https://wheels.developerfirst.ibm.com/ppc64le/linux/+simple"
REQUIREMENTS_FILE="/tmp/requirements.txt"
POWER_REQUIREMENTS_FILE="/tmp/power-requirements.txt"

# Packages that need Power-compatible prebuilt wheels.
# pyarrow is intentionally excluded because we pin an IBM-available version below.
PACKAGES="duckdb|grpcio|milvus-lite|pandas|numpy"

echo "Finding required package versions from ${REQUIREMENTS_FILE}..."

grep -Ei "^[[:space:]]*(${PACKAGES})([<>=!~].*)?[[:space:]]*$" \
    "${REQUIREMENTS_FILE}" \
    | sed 's/[[:space:]]*\\[[:space:]]*$//' \
    > "${POWER_REQUIREMENTS_FILE}"

echo "Power-specific requirements:"
cat "${POWER_REQUIREMENTS_FILE}"

echo "Installing Power packages from IBM index..."

python3.12 -m pip install \
    --extra-index-url "${IBM_PYPI_INDEX}" \
    -r "${POWER_REQUIREMENTS_FILE}"

echo "Installing pinned PyArrow version..."

python3.12 -m pip install \
    --extra-index-url "${IBM_PYPI_INDEX}" \
    pyarrow==22.0.0

echo "Installed packages:"
python3.12 -m pip list | grep -Ei \
    'duckdb|grpcio|pyarrow|milvus-lite|pandas|numpy'

echo "[prebuild-power] completed successfully."
