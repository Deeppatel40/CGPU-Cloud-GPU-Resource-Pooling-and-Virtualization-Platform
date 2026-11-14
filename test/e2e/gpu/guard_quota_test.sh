#!/usr/bin/env bash
# A3 criterion 7 - VRAM quota enforcement.
#
# Proves that the CGPU allocation guard refuses an allocation that would exceed the quota the
# scheduler assigned to a workload, that the refusal reaches the workload as an ordinary CUDA
# out-of-memory error, and that the refused request would otherwise have fitted in physical
# memory - so the result demonstrates quota enforcement rather than a genuinely full device.
#
# Runs inside a container through the nvidia runtime, which is the path CGPU uses in
# production. See concurrent_share_test.sh for criterion 6.
#
# Usage: ./guard_quota_test.sh [guard-dir] [probe-dir]
set -uo pipefail

GUARD_DIR="${1:-/tmp/cgpu-guard-build}"
PROBE_DIR="${2:-/tmp/cgpu-probe-build}"
IMG=docker.io/nvidia/cuda:12.6.2-base-ubuntu24.04
NVRT=/usr/bin/nvidia-container-runtime

QUOTA_MIB=128     # what the scheduler would hand this workload
REQUEST_MIB=256   # what the workload tries to take
CHUNK_MIB=64

for f in "$GUARD_DIR/libcgpu_guard.so" "$PROBE_DIR/cgpu_alloc_probe"; do
    [ -f "$f" ] || { echo "FAIL: missing $f - run the build.sh scripts first" >&2; exit 1; }
done

echo "physical VRAM before the test:"
nvidia-smi --query-gpu=memory.total,memory.used,memory.free --format=csv | sed 's/^/  /'
FREE_MIB=$(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | tr -d ' ')
echo
echo "quota=${QUOTA_MIB} MiB  request=${REQUEST_MIB} MiB  physically free=${FREE_MIB} MiB"
if [ "$FREE_MIB" -le "$REQUEST_MIB" ]; then
    echo "FAIL: only ${FREE_MIB} MiB free, so a denial would be ambiguous. Free the GPU first." >&2
    exit 1
fi
echo "=> the full request fits in physical memory, so any denial can only be the quota"
echo

OUT=$(k3s ctr run --rm \
    --runc-binary "$NVRT" \
    --env NVIDIA_VISIBLE_DEVICES=all \
    --env NVIDIA_DRIVER_CAPABILITIES=compute,utility \
    --env LD_PRELOAD=/opt/cgpu/libcgpu_guard.so \
    --env "CGPU_ALLOCATED_VRAM_MIB=$QUOTA_MIB" \
    --env CGPU_JOB_ID=a3-c7-quota \
    --env CGPU_GUARD_VERBOSE=1 \
    --env CGPU_GUARD_STATUS_FILE=/tmp/cgpu-guard-status \
    --mount "type=bind,src=$GUARD_DIR,dst=/opt/cgpu,options=rbind:ro" \
    --mount "type=bind,src=$PROBE_DIR,dst=/opt/cgpu-test,options=rbind:ro" \
    "$IMG" "a3-c7-$$" \
    /opt/cgpu-test/cgpu_alloc_probe --label quota-enforced \
        --target-mib "$REQUEST_MIB" --chunk-mib "$CHUNK_MIB" --expect-denial 2>&1)
RC=$?

echo "$OUT"
echo
echo "physical VRAM after the test:"
nvidia-smi --query-gpu=memory.used,memory.free --format=csv,noheader | sed 's/^/  /'

echo
echo "==================== RESULT ===================="
rc=0
check() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; rc=1; fi; }

check "guard loaded inside the container"        'grep -q "guard library loaded via LD_PRELOAD" <<<"$OUT"'
check "guard enforced the assigned quota"        'grep -q "guard active quota_bytes=134217728" <<<"$OUT"'
check "allocations up to the quota were allowed" 'grep -q "total_held=${QUOTA_MIB} MiB" <<<"$OUT"'
check "the over-quota allocation was denied"     'grep -q "deny .*cuMemAlloc_v2" <<<"$OUT"'
check "denial surfaced as CUDA out-of-memory"    'grep -q "REFUSED rc=2" <<<"$OUT"'
check "workload observed the denial, not a crash" 'grep -q "RESULT=OK (denial observed as expected)" <<<"$OUT"'
check "the guard released everything on free"    'grep -q "freed 2 chunks" <<<"$OUT"'
check "probe exited cleanly"                     '[ "$RC" -eq 0 ]'

echo
[ $rc -eq 0 ] && echo "OVERALL: PASS (criterion 7 satisfied)" || echo "OVERALL: FAIL"
exit $rc
