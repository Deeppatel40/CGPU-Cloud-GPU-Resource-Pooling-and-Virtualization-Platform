#!/usr/bin/env bash
# Builds the CGPU matmul workload.
#
#   ./build.sh            compile the binary into ./build
#   ./build.sh --ptx      regenerate cgpu_matmul_ptx.h from the .cu first (needs nvcc via
#                         containerd; only necessary when the kernel changes)
#
# The binary links libcuda dynamically on purpose - that is what makes its allocations
# interposable by the CGPU guard - and needs no CUDA toolkit, because the kernel travels as
# committed PTX. It must also stay loadable in the Ubuntu 22.04 (glibc 2.35) CUDA images, so
# the build fails if a newer glibc symbol creeps in.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
OUT="build"
GEN_PTX=0
for a in "$@"; do
    case "$a" in
        --ptx) GEN_PTX=1 ;;
        *) OUT="$a" ;;
    esac
done
mkdir -p "$OUT"

if [ "$GEN_PTX" = 1 ]; then
    IMG="${CGPU_DEVEL_IMAGE:-docker.io/nvidia/cuda:12.6.2-devel-ubuntu24.04}"
    ARCH="${CGPU_PTX_ARCH:-compute_75}"
    command -v k3s >/dev/null || { echo "FAIL: need k3s ctr to reach nvcc" >&2; exit 1; }
    WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
    cp cgpu_matmul_kernel.cu "$WORK/"
    echo "compiling kernel to PTX ($ARCH)"
    k3s ctr image pull "$IMG" >/dev/null
    k3s ctr run --rm \
        --mount "type=bind,src=$WORK,dst=/work,options=rbind:rw" \
        "$IMG" "cgpu-matmul-ptxgen-$$" \
        nvcc -ptx -arch="$ARCH" -o /work/cgpu_matmul.ptx /work/cgpu_matmul_kernel.cu
    grep -q "\.visible \.entry cgpu_sgemm" "$WORK/cgpu_matmul.ptx" \
        || { echo "FAIL: cgpu_sgemm missing from the generated PTX" >&2; exit 1; }
    {
        echo "/*"
        echo " * cgpu_matmul_ptx.h - GENERATED FILE, do not edit."
        echo " * Produced by build.sh --ptx from cgpu_matmul_kernel.cu ($ARCH)."
        echo " * Committed so the workload builds with nothing but gcc."
        echo " */"
        echo
        echo "#ifndef CGPU_MATMUL_PTX_H"
        echo "#define CGPU_MATMUL_PTX_H"
        echo
        echo "static const char cgpu_matmul_ptx[] ="
        sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/^/    "/' -e 's/$/\\n"/' "$WORK/cgpu_matmul.ptx"
        echo "    ;"
        echo
        echo "#endif /* CGPU_MATMUL_PTX_H */"
    } > cgpu_matmul_ptx.h
    echo "wrote cgpu_matmul_ptx.h"
fi

[ -f cgpu_matmul_ptx.h ] || { echo "FAIL: cgpu_matmul_ptx.h missing - run ./build.sh --ptx" >&2; exit 1; }

# Find a libcuda to link against. On WSL the driver lives outside the normal search path;
# in the CUDA devel image only the stub exists, which is fine - it carries the right SONAME
# (libcuda.so.1) so the real driver satisfies it at run time.
CUDA_LIBDIR=""
for d in /usr/lib/wsl/lib /usr/lib/x86_64-linux-gnu /usr/local/cuda/lib64/stubs; do
    if ls "$d"/libcuda.so* >/dev/null 2>&1; then CUDA_LIBDIR="$d"; break; fi
done
[ -n "$CUDA_LIBDIR" ] || { echo "FAIL: no libcuda found to link against" >&2; exit 1; }
echo "linking against libcuda in $CUDA_LIBDIR"

gcc -std=c11 -O2 -g -Wall -Wextra -Wno-unused-parameter \
    -o "$OUT/cgpu_matmul" cgpu_matmul.c \
    -L"$CUDA_LIBDIR" -Wl,--no-as-needed -lcuda \
    -Wl,-rpath,/usr/lib/wsl/lib

# Explicit, because the output often lands on a Windows drive where gcc's mode
# does not stick: a non-executable binary in the build context produced an image
# that failed at run time with "Permission denied". The Dockerfile chmods it too;
# this keeps the local copy runnable as well.
chmod 0755 "$OUT/cgpu_matmul"

echo "built $OUT/cgpu_matmul"
echo "dynamic CUDA symbols (what the guard interposes):"
nm -D -u "$OUT/cgpu_matmul" | grep -oE "cu[A-Za-z_]+(_v2)?" | sort -u | sed 's/^/  /'

TARGET_GLIBC="${CGPU_TARGET_GLIBC:-2.35}"
MAX_GLIBC=$(objdump -T "$OUT/cgpu_matmul" 2>/dev/null \
    | grep -oE "GLIBC_[0-9]+\.[0-9]+" | sed 's/GLIBC_//' | sort -u -V | tail -n 1)
echo "glibc required: ${MAX_GLIBC:-none} (ceiling $TARGET_GLIBC)"
if [ -n "$MAX_GLIBC" ] && [ "$(printf '%s\n%s\n' "$MAX_GLIBC" "$TARGET_GLIBC" | sort -V | tail -n1)" != "$TARGET_GLIBC" ]; then
    echo "FAIL: needs glibc $MAX_GLIBC, workload images have $TARGET_GLIBC" >&2
    exit 1
fi
echo "OK: loadable in the target workload images"
