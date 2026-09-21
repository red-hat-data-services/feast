#!/bin/bash
set -Eeuo pipefail
trap 'echo "[prebuild-power] failed at line $LINENO"; exit 1' ERR

IBM_PYPI_INDEX="https://wheels.developerfirst.ibm.com/ppc64le/linux/+simple"
REQUIREMENTS_FILE="/tmp/requirements.txt"
POWER_REQUIREMENTS_FILE="/tmp/power-requirements.txt"

# Packages that have pre-built ppc64le wheels on the IBM index.
# duckdb (1.1.3) and pyarrow (17.0.0) are excluded — their pinned
# versions are not on the IBM index and are built from source below.
PACKAGES="grpcio|milvus-lite|pandas|numpy"

echo "Finding required package versions from ${REQUIREMENTS_FILE}..."

grep -Ei "^[[:space:]]*(${PACKAGES})([<>=!~].*)?[[:space:]]*$" \
    "${REQUIREMENTS_FILE}" \
    | sed 's/[[:space:]]*\\[[:space:]]*$//' \
    > "${POWER_REQUIREMENTS_FILE}"

echo "Power-specific requirements:"
cat "${POWER_REQUIREMENTS_FILE}"

echo "Installing Power packages from IBM index..."

python3.11 -m pip install \
    --extra-index-url "${IBM_PYPI_INDEX}" \
    -r "${POWER_REQUIREMENTS_FILE}"

#######################################################
# Source builds — versions not available on IBM index
#######################################################

echo "Installing build tools for source builds..."
dnf install -y gcc-toolset-13 make cmake ninja-build libomp-devel \
               git python3.11-devel openssl openssl-devel zlib-devel libuuid-devel
source /opt/rh/gcc-toolset-13/enable
export CXX=/opt/rh/gcc-toolset-13/root/usr/bin/g++

# Pin Cython < 3.1 — PyArrow 17.0.0 has a nogil/GIL incompatibility
# with Cython ≥ 3.1 (ARROW-43552: table.pxi:6105).
python3.11 -m pip install \
    build wheel 'setuptools<78' ninja pybind11 setuptools_scm 'Cython<3.1'

WORKDIR=$(pwd)

# DuckDB 1.1.3 (IBM index oldest is 1.4.3)
echo "Building DuckDB 1.1.3 from source..."
git clone --depth 1 -b v1.1.3 https://github.com/duckdb/duckdb.git
cd duckdb/tools/pythonpkg
SETUPTOOLS_SCM_PRETEND_VERSION=1.1.3 python3.11 -m build --wheel --no-isolation
pip install dist/*.whl
cd "$WORKDIR"

# PyArrow 17.0.0 (IBM index has 19.0.1+, not 17.0.0)
echo "Building PyArrow 17.0.0 from source..."
git clone --depth 1 -b apache-arrow-17.0.0 https://github.com/apache/arrow.git
cd arrow && git submodule update --init --recursive
cd cpp && mkdir -p release && cd release
cmake -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_INSTALL_PREFIX=/usr/local \
      -DARROW_PYTHON=ON \
      -DARROW_PARQUET=ON \
      -DARROW_ORC=ON \
      -DARROW_FILESYSTEM=ON \
      -DARROW_WITH_LZ4=ON \
      -DARROW_WITH_ZSTD=ON \
      -DARROW_WITH_SNAPPY=ON \
      -DARROW_JSON=ON \
      -DARROW_CSV=ON \
      -DARROW_DATASET=ON \
      -DARROW_S3=ON \
      -DARROW_BUILD_TESTS=OFF \
      -DARROW_SUBSTRAIT=ON \
      -DProtobuf_SOURCE=BUNDLED \
      -DARROW_DEPENDENCY_SOURCE=BUNDLED \
    ..
make -j$(nproc) && make install
cd ../../python
BUILD_TYPE=release python3.11 setup.py build_ext --build-type=release --bundle-arrow-cpp bdist_wheel
pip install dist/*.whl
cd "$WORKDIR"

echo "Installed packages:"
python3.11 -m pip list | grep -Ei \
    'duckdb|grpcio|pyarrow|milvus-lite|pandas|numpy'

echo "[prebuild-power] completed successfully."
