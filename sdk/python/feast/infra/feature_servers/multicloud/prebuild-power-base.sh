#!/bin/bash
# ppc64le pins matching downstream:
# https://github.com/red-hat-data-services/feast/blob/rhoai-3.5/Dockerfiles/Dockerfile.feature-server.konflux
set -Eeuo pipefail
trap 'echo "[prebuild-power] failed at line $LINENO"; exit 1' ERR

IBM_INDEX="https://wheels.developerfirst.ibm.com/ppc64le/linux"
PYARROW_VER=22.0.0
RE2_VER=20220401
# IBM has no grpcio 1.83.x for ppc64le; 1.82.1 is manylinux_2_34 and satisfies feast's grpcio>=1.56.2.
GRPCIO_VER=1.82.1

echo "[prebuild-power] Installing pinned pyarrow==${PYARROW_VER} re2==${RE2_VER} grpcio==${GRPCIO_VER} (rhoai-3.5 Power path + IBM grpcio)"
# IBM publishes pyarrow 22.0.0 as manylinux_2_34_ppc64le (UBI9 glibc 2.34).
uv pip install --extra-index-url "$IBM_INDEX" \
    "pyarrow==${PYARROW_VER}" "re2==${RE2_VER}" "grpcio==${GRPCIO_VER}"

uv pip freeze | grep -E '^(pyarrow|re2|grpcio)==' > /power-constraints.txt
if [ ! -s /power-constraints.txt ]; then
    echo "[prebuild-power] ERROR: pyarrow/re2/grpcio missing after Power pin" >&2
    exit 1
fi
echo "[prebuild-power] Power constraints:"
cat /power-constraints.txt
