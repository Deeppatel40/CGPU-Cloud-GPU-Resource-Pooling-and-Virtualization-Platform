#!/usr/bin/env bash
# Regenerates cgpu_burn_ptx.h from cgpu_burn_kernel.cu.
#
# Run this only when the kernel changes. The generated header is committed, so neither the
# probe's build nor the test suite needs a CUDA toolkit - see the comment at the top of
# cgpu_burn_kernel.cu for why that matters.
#
# nvcc comes from the CUDA devel image, used throw-away via containerd. PTX is compiled for
# compute_75, the lowest capability this project may meet; the driver JITs it up to the
# actual device (the RTX 4050 worker is 8.9). Targeting a virtual arch rather than a real
# one is what keeps the artefact portable across the cluster's GPUs.
#
# Usage: sudo ./gen_ptx.sh        (needs k3s ctr, i.e. run on a cluster node as root)
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
IMG="${CGPU_DEVEL_IMAGE:-docker.io/nvidia/cuda:12.6.2-devel-ubuntu24.04}"
ARCH="${CGPU_PTX_ARCH:-compute_75}"

command -v k3s >/dev/null || { echo "FAIL: k3s ctr not found; run this on a cluster node" >&2; exit 1; }

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
cp cgpu_burn_kernel.cu "$WORK/"

echo "compiling cgpu_burn_kernel.cu to PTX ($ARCH) using $IMG"
k3s ctr image pull "$IMG" >/dev/null
k3s ctr run --rm \
    --mount "type=bind,src=$WORK,dst=/work,options=rbind:rw" \
    "$IMG" "cgpu-ptxgen-$$" \
    nvcc -ptx -arch="$ARCH" -o /work/cgpu_burn.ptx /work/cgpu_burn_kernel.cu

[ -s "$WORK/cgpu_burn.ptx" ] || { echo "FAIL: nvcc produced no PTX" >&2; exit 1; }
grep -q "\.visible \.entry cgpu_burn" "$WORK/cgpu_burn.ptx" \
    || { echo "FAIL: cgpu_burn entry point missing from the PTX" >&2; exit 1; }

NVCC_VER=$(k3s ctr run --rm "$IMG" "cgpu-ptxver-$$" nvcc --version 2>/dev/null \
    | grep -oE "release [0-9]+\.[0-9]+" | head -n1)

{
    echo "/*"
    echo " * cgpu_burn_ptx.h - GENERATED FILE, do not edit."
    echo " *"
    echo " * Produced by gen_ptx.sh from cgpu_burn_kernel.cu."
    echo " *   nvcc:   ${NVCC_VER:-unknown} (image $IMG)"
    echo " *   target: $ARCH (virtual arch; the driver JITs to the real device)"
    echo " *"
    echo " * Committed so the probe builds with nothing but gcc and libcuda."
    echo " */"
    echo
    echo "#ifndef CGPU_BURN_PTX_H"
    echo "#define CGPU_BURN_PTX_H"
    echo
    echo "static const char cgpu_burn_ptx[] ="
    # Each PTX line becomes one C string literal. Backslashes and quotes are escaped;
    # PTX contains neither in practice, but a generator should not assume that.
    sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/^/    "/' -e 's/$/\\n"/' "$WORK/cgpu_burn.ptx"
    echo "    ;"
    echo
    echo "#endif /* CGPU_BURN_PTX_H */"
} > cgpu_burn_ptx.h

echo "wrote cgpu_burn_ptx.h ($(wc -l < cgpu_burn_ptx.h) lines)"
