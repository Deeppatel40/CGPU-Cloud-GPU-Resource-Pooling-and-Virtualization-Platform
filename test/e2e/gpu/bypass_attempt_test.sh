#!/usr/bin/env bash
# A3 - quota bypass attempts, and the mitigation for the one that works.
#
# A quota is only worth something if a workload cannot opt out of it. PyTorch can be pointed
# at three allocator strategies with one environment variable, and each reaches the driver by
# a different route. This test records, on hardware, which of them the guard contains.
#
# Result on the RTX 4050 (see docs/a3-guard-evidence.md):
#
#   native            cudaMalloc            CONTAINED - linked dynamically, interposed
#   cudaMallocAsync   cudaMallocAsync       CONTAINED - linked dynamically, interposed
#   expandable_segs   cuMemCreate via dlsym ESCAPES   - resolved at runtime, not interposable
#
# The third is a real, known gap. LD_PRELOAD cannot police a symbol the workload looks up by
# name at runtime, and the guard's dlsym hook that would have caught it is disabled because it
# makes the driver's own internal lookups re-enter the guard (it hung the workload; see
# CGPU_GUARD_HOOK_DLSYM in pkg/guard/cgpu_guard.c).
#
# The mitigation is therefore in the scheduler, not the guard: strip or pin
# PYTORCH_CUDA_ALLOC_CONF on submitted jobs. This test proves the mitigation holds, so it
# fails if the sanitised case leaks - which is the property CGPU actually depends on.
#
# Usage: ./bypass_attempt_test.sh [guard-dir]
set -uo pipefail

GUARD_DIR="${1:-/tmp/cgpu-guard-build}"
IMG="${CGPU_TORCH_IMAGE:-docker.io/pytorch/pytorch:2.5.1-cuda12.4-cudnn9-runtime}"
NVRT=/usr/bin/nvidia-container-runtime
QUOTA_MIB=512
OVER_MIB=1024

[ -f "$GUARD_DIR/libcgpu_guard.so" ] || { echo "FAIL: missing guard" >&2; exit 1; }

FREE_MIB=$(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | tr -d ' ')
echo "quota ${QUOTA_MIB} MiB, each attempt requests ${OVER_MIB} MiB, physically free ${FREE_MIB} MiB"
[ "$FREE_MIB" -gt "$OVER_MIB" ] || { echo "FAIL: need more free VRAM" >&2; exit 1; }
echo

PY='
import os, torch
mib = int(os.environ["TRY_MIB"])
free, total = torch.cuda.mem_get_info()
print(f"SEES total={total//1024**2}MiB free={free//1024**2}MiB", flush=True)
print("CONF=" + os.environ.get("PYTORCH_CUDA_ALLOC_CONF", "(unset)"), flush=True)
try:
    x = torch.empty((mib*1024*1024)//4, dtype=torch.float32, device="cuda")
    torch.cuda.synchronize()
    print(f"ESCAPED requested={mib}MiB reserved={torch.cuda.memory_reserved()//1024**2}MiB", flush=True)
except Exception as e:
    print(f"CONTAINED requested={mib}MiB err={type(e).__name__}", flush=True)
'

attempt() { # job-name, env args...
    local job="$1"; shift
    k3s ctr run --rm --runc-binary "$NVRT" \
        --env NVIDIA_VISIBLE_DEVICES=all --env NVIDIA_DRIVER_CAPABILITIES=compute,utility \
        --env LD_PRELOAD=/opt/cgpu/libcgpu_guard.so \
        --env "CGPU_ALLOCATED_VRAM_MIB=$QUOTA_MIB" --env "CGPU_JOB_ID=$job" \
        --env CGPU_GUARD_VERBOSE=1 --env "TRY_MIB=$OVER_MIB" "$@" \
        --mount "type=bind,src=$GUARD_DIR,dst=/opt/cgpu,options=rbind:ro" \
        "$IMG" "cgpu-$job-$$" python -c "$PY" 2>&1
}

show() { grep -E "SEES|CONF=|ESCAPED|CONTAINED" <<<"$1" | sed 's/^/  /'; echo "  denials: $(grep -c 'deny ' <<<"$1")"; }

echo "=== 1. native backend ==="
NATIVE=$(attempt bypass-native --env "PYTORCH_CUDA_ALLOC_CONF="); show "$NATIVE"
echo
echo "=== 2. cudaMallocAsync backend ==="
ASYNC=$(attempt bypass-async --env "PYTORCH_CUDA_ALLOC_CONF=backend:cudaMallocAsync"); show "$ASYNC"
echo
echo "=== 3. expandable_segments (known gap) ==="
VMM=$(attempt bypass-vmm --env "PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True"); show "$VMM"
echo
echo "=== 4. MITIGATION: scheduler strips PYTORCH_CUDA_ALLOC_CONF ==="
echo "    (same job, the variable simply not passed through)"
SANE=$(attempt bypass-sanitised); show "$SANE"

echo
echo "==================== RESULT ===================="
rc=0
chk() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; rc=1; fi; }

chk "native: guard loaded"                     'grep -q "guard library loaded" <<<"$NATIVE"'
chk "native: over-quota request contained"     'grep -q CONTAINED <<<"$NATIVE"'
chk "native: guard refused the allocation"     'grep -q "deny " <<<"$NATIVE"'
chk "async: over-quota request contained"      'grep -q CONTAINED <<<"$ASYNC"'
chk "async: guard refused the allocation"      'grep -q "deny " <<<"$ASYNC"'
chk "workload sees its quota, not the card"    'grep -q "SEES total=512MiB" <<<"$NATIVE"'
chk "MITIGATION: sanitised job is contained"   'grep -q CONTAINED <<<"$SANE"'

echo
if grep -q ESCAPED <<<"$VMM"; then
    echo "KNOWN GAP (expected): expandable_segments:True escaped the quota."
    echo "  reserved $(grep -oE 'reserved=[0-9]+MiB' <<<"$VMM") against a ${QUOTA_MIB} MiB quota, 0 guard denials."
    echo "  Mitigation verified above: CGPU must strip or pin PYTORCH_CUDA_ALLOC_CONF on"
    echo "  submitted jobs. Without that, a job can opt out of its quota with one variable."
else
    echo "NOTE: expandable_segments was contained - coverage is better than documented."
fi

echo
[ $rc -eq 0 ] && echo "OVERALL: PASS" || echo "OVERALL: FAIL"
exit $rc
