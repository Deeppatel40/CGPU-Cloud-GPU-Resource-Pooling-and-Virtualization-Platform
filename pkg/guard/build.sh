#!/usr/bin/env bash
# Builds the CGPU CUDA allocation guard.
#
# No CUDA toolkit is required: the guard declares the few CUDA symbols it needs and resolves
# the real implementations at runtime with dlsym(). Only gcc and libc headers are needed,
# which keeps the build usable on the WSL workers where host disk space is tight.
#
# Usage: ./build.sh [output-dir]      (default: ./build)
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
OUT="${1:-build}"
mkdir -p "$OUT"

gcc -std=c11 -O2 -g -fPIC -shared \
    -Wall -Wextra -Wno-unused-parameter \
    -o "$OUT/libcgpu_guard.so" cgpu_guard.c \
    -ldl -lpthread

echo "built $OUT/libcgpu_guard.so"
echo
echo "interposed symbols:"
nm -D --defined-only "$OUT/libcgpu_guard.so" | grep -E " T (cuda|cu)" | awk '{print "  " $3}'
echo

# The guard is preloaded into the CUDA workload images, which are Ubuntu 22.04 based
# (glibc 2.35). If this build requires anything newer, the library cannot load there, so
# treat it as a build failure rather than discovering it at runtime inside a container.
TARGET_GLIBC="${CGPU_TARGET_GLIBC:-2.35}"
MAX_GLIBC=$(objdump -T "$OUT/libcgpu_guard.so" 2>/dev/null \
  | grep -oE "GLIBC_[0-9]+\.[0-9]+" | sed 's/GLIBC_//' | sort -u -V | tail -n 1)

echo "glibc required by this build: ${MAX_GLIBC:-none}  (target ceiling: $TARGET_GLIBC)"
if [ -n "$MAX_GLIBC" ] && [ "$(printf '%s\n%s\n' "$MAX_GLIBC" "$TARGET_GLIBC" | sort -V | tail -n 1)" != "$TARGET_GLIBC" ]; then
    echo
    echo "FAIL: this build needs glibc $MAX_GLIBC but the workload images only have $TARGET_GLIBC." >&2
    echo "      Replace whatever libc call pulled that in (strtoull -> cgpu_parse_u64 was one)." >&2
    exit 1
fi
echo "OK: loadable in the target workload images"
