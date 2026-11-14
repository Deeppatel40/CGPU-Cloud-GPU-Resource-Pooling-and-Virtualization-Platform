#!/usr/bin/env bash
# A3 criterion 5 - verify guard coverage for the intended demo workload.
#
# The guard can only police allocations that reach it. PyTorch is the project's demo
# workload, so whether its caching allocator is covered decides whether a VRAM quota is a
# real enforcement boundary for the demo or merely advisory.
#
# PyTorch has two allocator backends:
#   native          -> cudaMalloc / cudaFree            -> COVERED by the guard
#   cudaMallocAsync -> cudaMallocAsync / pool APIs      -> NOT COVERED (documented gap)
#
# This test exercises both and reports what it finds, rather than assuming. A workload that
# logs the guard's load line but produces no allocation lines is unpoliced - that is the
# signal this test looks for.
#
# Usage: ./pytorch_coverage_test.sh [guard-dir]
set -uo pipefail

GUARD_DIR="${1:-/tmp/cgpu-guard-build}"
IMG="${CGPU_TORCH_IMAGE:-docker.io/pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime}"
NVRT=/usr/bin/nvidia-container-runtime

QUOTA_MIB=512
UNDER_MIB=256    # comfortably inside the quota
OVER_MIB=1024    # beyond the quota, but far inside the card's 6141 MiB

[ -f "$GUARD_DIR/libcgpu_guard.so" ] || { echo "FAIL: missing guard - run pkg/guard/build.sh" >&2; exit 1; }

FREE_MIB=$(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | tr -d ' ')
echo "physically free: ${FREE_MIB} MiB; quota ${QUOTA_MIB} MiB; over-quota request ${OVER_MIB} MiB"
if [ "$FREE_MIB" -le "$OVER_MIB" ]; then
    echo "FAIL: not enough free VRAM for an unambiguous test" >&2
    exit 1
fi

echo "pulling $IMG (large - needs host disk space)"
if ! k3s ctr image pull "$IMG" >/dev/null 2>&1; then
    echo "FAIL: could not pull $IMG (host disk space is the usual cause)" >&2
    exit 1
fi

PY='
import os, sys, torch
mib = int(os.environ["TRY_MIB"])
print("torch", torch.__version__, "cuda", torch.version.cuda, flush=True)
print("backend_conf", os.environ.get("PYTORCH_CUDA_ALLOC_CONF", "(default/native)"), flush=True)
print("device", torch.cuda.get_device_name(0), flush=True)
try:
    x = torch.empty((mib * 1024 * 1024) // 4, dtype=torch.float32, device="cuda")
    torch.cuda.synchronize()
    print(f"ALLOC_OK requested={mib}MiB reserved={torch.cuda.memory_reserved()//1024**2}MiB", flush=True)
    del x
except Exception as e:
    print(f"ALLOC_FAILED requested={mib}MiB err={type(e).__name__}: {str(e)[:160]}", flush=True)
'

run() { # name, quota_mib, try_mib, alloc_conf
    k3s ctr run --rm \
        --runc-binary "$NVRT" \
        --env NVIDIA_VISIBLE_DEVICES=all \
        --env NVIDIA_DRIVER_CAPABILITIES=compute,utility \
        --env LD_PRELOAD=/opt/cgpu/libcgpu_guard.so \
        --env "CGPU_ALLOCATED_VRAM_MIB=$2" \
        --env "CGPU_JOB_ID=$1" \
        --env CGPU_GUARD_VERBOSE=1 \
        --env "TRY_MIB=$3" \
        --env "PYTORCH_CUDA_ALLOC_CONF=$4" \
        --mount "type=bind,src=$GUARD_DIR,dst=/opt/cgpu,options=rbind:ro" \
        "$IMG" "cgpu-$1-$$" python -c "$PY" 2>&1
}

echo
echo "==================== 1. native backend, inside quota ===================="
N_UNDER=$(run torch-native-under "$QUOTA_MIB" "$UNDER_MIB" "")
echo "$N_UNDER"

echo
echo "==================== 2. native backend, over quota ===================="
N_OVER=$(run torch-native-over "$QUOTA_MIB" "$OVER_MIB" "")
echo "$N_OVER"

echo
echo "==================== 3. cudaMallocAsync backend, over quota ===================="
A_OVER=$(run torch-async-over "$QUOTA_MIB" "$OVER_MIB" "backend:cudaMallocAsync")
echo "$A_OVER"

echo
echo "==================== FINDINGS ===================="
rc=0
check() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; rc=1; fi; }

check "guard loaded inside the PyTorch image" 'grep -q "guard library loaded via LD_PRELOAD" <<<"$N_UNDER"'

if grep -qE "allow .*(cudaMalloc|cuMemAlloc)" <<<"$N_UNDER"; then
    echo "PASS: native backend allocations ARE intercepted by the guard"
else
    echo "FAIL: native backend allocations were NOT intercepted - the quota is advisory only"
    rc=1
fi

check "workload inside quota succeeded"    'grep -q "ALLOC_OK" <<<"$N_UNDER"'
check "over-quota request was denied"      'grep -q "deny " <<<"$N_OVER"'
check "PyTorch surfaced the denial"        'grep -q "ALLOC_FAILED" <<<"$N_OVER"'

echo
echo "--- async backend (expected to be a documented gap, not a regression) ---"
if grep -q "deny " <<<"$A_OVER"; then
    echo "NOTE: cudaMallocAsync allocations were also intercepted - coverage is broader than documented"
elif grep -q "ALLOC_OK" <<<"$A_OVER"; then
    echo "CONFIRMED GAP: with backend:cudaMallocAsync the over-quota allocation SUCCEEDED."
    echo "               The guard does not cover the pool APIs, so CGPU must either pin"
    echo "               PYTORCH_CUDA_ALLOC_CONF to the native backend for submitted jobs,"
    echo "               or extend the guard to cudaMallocAsync/cuMemAllocFromPoolAsync."
else
    echo "INCONCLUSIVE: async backend run neither allocated nor was denied; see output above."
fi

echo
[ $rc -eq 0 ] && echo "OVERALL: PASS (demo workload coverage verified)" || echo "OVERALL: FAIL"
exit $rc
