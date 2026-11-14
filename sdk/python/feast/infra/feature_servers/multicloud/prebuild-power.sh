#!/bin/bash
set -Eeuo pipefail
trap 'echo "[prebuild-power] failed at line $LINENO"; exit 1' ERR

IBM_PYPI_INDEX="https://wheels.developerfirst.ibm.com/ppc64le/linux/+simple"
REQUIREMENTS_FILE="/tmp/requirements.txt"
POWER_REQUIREMENTS_FILE="/tmp/power-requirements.txt"

# Packages that need Power-compatible prebuilt wheels.
# pyarrow is intentionally excluded because we pin an IBM-available version below.
PACKAGES="duckdb|pandas|numpy"

echo "Finding required package versions from ${REQUIREMENTS_FILE}..."

grep -Ei "^[[:space:]]*(${PACKAGES})([<>=!~].*)?[[:space:]]*$" \
    "${REQUIREMENTS_FILE}" \
    | sed 's/[[:space:]]*\\[[:space:]]*$//' \
    > "${POWER_REQUIREMENTS_FILE}"

# milvus-lite: pinned to the IBM-built 2.4.12 wheel (linked against system
# OpenSSL - passes the FIPS scan). The 3.2.1 pin used on x86_64/arm64 is a
# pure wheel whose faiss-cpu dependency has no ppc64le wheels, so it cannot
# be dependency-resolved here.
echo "milvus-lite==2.4.12" >> "${POWER_REQUIREMENTS_FILE}"

echo "Power-specific requirements:"
cat "${POWER_REQUIREMENTS_FILE}"

echo "Installing Power packages from IBM index..."

python3.12 -m pip install \
    --extra-index-url "${IBM_PYPI_INDEX}" \
    -r "${POWER_REQUIREMENTS_FILE}"

echo "Building grpcio from source with system OpenSSL..."
GRPC_PYTHON_BUILD_SYSTEM_OPENSSL=1 python3.12 -m pip install --no-binary=:all: grpcio==1.84.1

echo "Installing pinned PyArrow version..."

python3.12 -m pip install \
    --extra-index-url "${IBM_PYPI_INDEX}" \
    pyarrow==22.0.0

echo "Installed packages:"
python3.12 -m pip list | grep -Ei \
    'duckdb|grpcio|pyarrow|milvus-lite|pandas|numpy'

echo "[prebuild-power] completed successfully."
