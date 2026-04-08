#!/bin/bash
set -Eeuo pipefail
trap 'echo "[prebuild-power] failed at line $LINENO"; exit 1' ERR
shopt -s dotglob nullglob

# Must match the Konflux base image Python version; wheel tags must match `pip` in Dockerfile.
PYTHON_VERSION=3.12
WORKDIR=$(pwd)
CMAKE_VERSION=3.30.5
CMAKE_REQUIRED_VERSION=3.30.5

dnf install -y make cmake ninja-build libomp-devel \
               git openssl openssl-devel zlib-devel libuuid-devel \
               libcurl-devel && dnf clean all

export CC=gcc
export CXX=g++
# -mlongcall: generates indirect calls to avoid R_PPC64_REL24 overflow (24-bit branch limit)
#   on large ppc64le binaries like DuckDB. -mcmodel=medium alone only fixes TOC/data access.
export CFLAGS="-mcmodel=medium -mlongcall"
export CXXFLAGS="-std=c++17 -mcmodel=medium -mlongcall -Wno-attributes"
export LDFLAGS="-Wl,--no-toc-optimize"
# DuckDB's setup.py reads this to override its default compile flags
export DUCKDB_COMPILE_FLAGS="-mcmodel=medium -mlongcall -Wno-attributes -g0"

: "${CMAKE_ARGS:=""}"
: "${LINKFLAGS:=""}"

# Installing Python build dependencies
python${PYTHON_VERSION} -m pip install build wheel 'setuptools<82' ninja pybind11 numpy==2.3.3 setuptools_scm Cython

# Directory to collect built wheels
mkdir -p /wheelhouse

#######################################################
# Build DuckDB (Python package)
#######################################################
echo "Entering DuckDB source directory..."
git clone https://github.com/duckdb/duckdb.git
cd duckdb
git checkout v1.1.3
cd tools/pythonpkg
export SETUPTOOLS_SCM_PRETEND_VERSION=1.1.3
python${PYTHON_VERSION} -m build --wheel --no-isolation
# Cleanup
unset SETUPTOOLS_SCM_PRETEND_VERSION
ls dist/*.whl >/dev/null
cp -v dist/*.whl /wheelhouse/
cd $WORKDIR

#######################################################
# Build gRPC  (Python package)
#######################################################
echo "Building grpcio..."
export GRPC_PYTHON_BUILD_SYSTEM_OPENSSL=1
python${PYTHON_VERSION} -m pip install grpcio==1.62.3

#######################################################
# Build Pyarrow  (Python package)
#######################################################
echo "Entering Pyarrow source directory..."
git clone https://github.com/apache/arrow.git
cd arrow
git checkout apache-arrow-22.0.0
git submodule update --init --recursive
cd cpp
mkdir -p release && cd release
cmake -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_INSTALL_PREFIX=/usr/local \
      -DARROW_PYTHON=ON \
      -DARROW_PARQUET=ON \
      -DARROW_ORC=ON \
      -DARROW_FILESYSTEM=ON \
      -DARROW_FLIGHT=ON \
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
make -j$(nproc)
make install
cd ../../python
export BUILD_TYPE=release
python${PYTHON_VERSION} setup.py build_ext --build-type=$BUILD_TYPE --bundle-arrow-cpp bdist_wheel
ls dist/*.whl >/dev/null
cp -v dist/*.whl /wheelhouse/
cd ../../..

#######################################################
# Build Milvus-Lite  (Python package)
#######################################################
echo "Building milvus-lite..."
dnf install -y perl ncurses-devel wget openblas-devel cargo libstdc++-static which libaio \
               libtool m4 autoconf automake libffi-devel scl-utils xz && dnf clean all

python${PYTHON_VERSION} -m pip install conan==1.64.1 setuptools==70.0.0

git clone https://github.com/milvus-io/milvus-lite
cd milvus-lite/python
git checkout v2.4.12
git submodule update --init --recursive
# --no-build-isolation: milvus-lite's setup.py shells out to `conan install`
# which must find the conan binary + profile from the main environment.
python${PYTHON_VERSION} -m pip install -v --no-build-isolation .
