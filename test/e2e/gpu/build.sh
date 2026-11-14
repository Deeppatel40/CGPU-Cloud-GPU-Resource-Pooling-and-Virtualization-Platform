#!/usr/bin/env bash
# Builds the CGPU controlled GPU test workload (cgpu_alloc_probe).
#
# Links libcuda dynamically on purpose: that is what makes the workload's allocation calls
# interposable by the CGPU guard, and it mirrors how framework workloads link the driver.
# No CUDA toolkit is needed - the driver API prototypes are declared in the source and the
# driver stub is taken from the WSL driver directory (or the standard path off WSL).
#
# The binary must stay loadable inside the CUDA workload images (Ubuntu 22.04, glibc 2.35).
#
# Usage: ./build.sh [output-dir]      (default: ./build)
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
OUT="${1:-build}"
mkdir -p "$OUT"

# Find a libcuda to link against. On WSL the driver lives outside the normal search path.
CUDA_LIBDIR=""
for d in /usr/lib/wsl/lib /usr/lib/x86_64-linux-gnu /usr/local/cuda/lib64/stubs; do
    if ls "$d"/libcuda.so* >/dev/null 2>&1; then CUDA_LIBDIR="$d"; break; fi
done
if [ -z "$CUDA_LIBDIR" ]; then
    echo "FAIL: no libcuda found; cannot link the probe" >&2
    exit 1
fi
echo "linking against libcuda in $CUDA_LIBDIR"

# -Wl,--no-as-needed keeps the libcuda dependency even though the symbols are only
# referenced through declarations; without it the linker may drop it.
gcc -std=c11 -O2 -g -Wall -Wextra -Wno-unused-parameter \
    -o "$OUT/cgpu_alloc_probe" cgpu_alloc_probe.c \
    -L"$CUDA_LIBDIR" -Wl,--no-as-needed -lcuda \
    -Wl,-rpath,/usr/lib/wsl/lib

echo "built $OUT/cgpu_alloc_probe"
echo
echo "dynamic CUDA symbols (these are what the guard interposes):"
nm -D -u "$OUT/cgpu_alloc_probe" | grep -oE "cu[A-Za-z_]+(_v2)?" | sort -u | sed 's/^/  /'
echo
TARGET_GLIBC="${CGPU_TARGET_GLIBC:-2.35}"
MAX_GLIBC=$(objdump -T "$OUT/cgpu_alloc_probe" 2>/dev/null \
  | grep -oE "GLIBC_[0-9]+\.[0-9]+" | sed 's/GLIBC_//' | sort -u -V | tail -n 1)
echo "glibc required: ${MAX_GLIBC:-none}  (target ceiling: $TARGET_GLIBC)"
if [ -n "$MAX_GLIBC" ] && [ "$(printf '%s\n%s\n' "$MAX_GLIBC" "$TARGET_GLIBC" | sort -V | tail -n 1)" != "$TARGET_GLIBC" ]; then
    echo "FAIL: needs glibc $MAX_GLIBC, workload images have $TARGET_GLIBC" >&2
    exit 1
fi
echo "OK: loadable in the target workload images"
