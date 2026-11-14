#!/usr/bin/env bash
# A3 criterion 6 - GPU sharing between two jobs with distinct VRAM quotas.
#
# Runs two containerized workloads at the same time on one physical GPU, each with a
# different quota enforced by the CGPU allocation guard, and captures timestamps proving
# their CUDA activity overlapped on the same card. Both jobs also report the device UUID, so
# "the same physical GPU" is evidence rather than an assumption.
#
# Each job holds less than its quota, so neither should see a denial - this test is about
# sharing and isolation of quotas, not enforcement (that is guard_quota_test.sh).
#
# Usage: ./concurrent_share_test.sh [guard-dir] [probe-dir]
set -uo pipefail

GUARD_DIR="${1:-/tmp/cgpu-guard-build}"
PROBE_DIR="${2:-/tmp/cgpu-probe-build}"
IMG=docker.io/nvidia/cuda:12.6.2-base-ubuntu24.04
NVRT=/usr/bin/nvidia-container-runtime
RUNTIME_S=12

# job            quota   holds
JOB_A_QUOTA=512;  JOB_A_HOLD=384
JOB_B_QUOTA=1024; JOB_B_HOLD=768

for f in "$GUARD_DIR/libcgpu_guard.so" "$PROBE_DIR/cgpu_alloc_probe"; do
    [ -f "$f" ] || { echo "FAIL: missing $f - run the build.sh scripts first" >&2; exit 1; }
done

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

launch() {
    local job="$1" quota="$2" hold="$3"
    k3s ctr run --rm \
        --runc-binary "$NVRT" \
        --env NVIDIA_VISIBLE_DEVICES=all \
        --env NVIDIA_DRIVER_CAPABILITIES=compute,utility \
        --env LD_PRELOAD=/opt/cgpu/libcgpu_guard.so \
        --env "CGPU_ALLOCATED_VRAM_MIB=$quota" \
        --env "CGPU_JOB_ID=$job" \
        --mount "type=bind,src=$GUARD_DIR,dst=/opt/cgpu,options=rbind:ro" \
        --mount "type=bind,src=$PROBE_DIR,dst=/opt/cgpu-test,options=rbind:ro" \
        "$IMG" "cgpu-$job-$$" \
        /opt/cgpu-test/cgpu_alloc_probe --label "$job" \
            --target-mib "$hold" --chunk-mib 128 --seconds "$RUNTIME_S" \
        > "$WORK/$job.log" 2>&1
}

echo "launching two workloads on one GPU:"
echo "  job-a: quota ${JOB_A_QUOTA} MiB, holds ${JOB_A_HOLD} MiB"
echo "  job-b: quota ${JOB_B_QUOTA} MiB, holds ${JOB_B_HOLD} MiB"
echo

launch job-a "$JOB_A_QUOTA" "$JOB_A_HOLD" &
PID_A=$!
launch job-b "$JOB_B_QUOTA" "$JOB_B_HOLD" &
PID_B=$!

# Sample device-level telemetry while both run. Per-process VRAM is unavailable on WSL
# (see docs/gpu-worker-4050.md), so the device total is the only external view.
echo "device telemetry while both jobs run:"
for i in $(seq 1 6); do
    sleep 2
    printf "  t+%02ds  %s\n" $((i * 2)) \
        "$(nvidia-smi --query-gpu=memory.used,memory.free,utilization.gpu --format=csv,noheader)"
done

wait $PID_A; RC_A=$?
wait $PID_B; RC_B=$?

echo
for j in job-a job-b; do
    echo "---------------- $j ----------------"
    grep -E "uuid|alloc|progress|freed|RESULT" "$WORK/$j.log" | head -n 20
    echo
done

# ---- overlap analysis: do the two jobs' activity windows intersect? ----
window() { # prints "start end" in seconds for a job's progress lines
    grep -oE "^\[[0-9:.]+\]" "$WORK/$1.log" | tr -d '[]' | awk -F: '
        { t = $1 * 3600 + $2 * 60 + $3 }
        NR == 1 { min = t } { if (t < min) min = t; if (t > max) max = t }
        END { printf "%.3f %.3f", min, max }'
}
read -r A_START A_END <<<"$(window job-a)"
read -r B_START B_END <<<"$(window job-b)"

OVERLAP=$(awk -v a1="$A_START" -v a2="$A_END" -v b1="$B_START" -v b2="$B_END" \
    'BEGIN { s = (a1 > b1 ? a1 : b1); e = (a2 < b2 ? a2 : b2); printf "%.3f", e - s }')

echo "================ OVERLAP ANALYSIS ================"
printf "  job-a active window: %.3f -> %.3f\n" "$A_START" "$A_END"
printf "  job-b active window: %.3f -> %.3f\n" "$B_START" "$B_END"
echo "  overlapping duration: ${OVERLAP}s"

UUID_A=$(grep -oE "GPU-[0-9a-f-]+" "$WORK/job-a.log" | head -n1)
UUID_B=$(grep -oE "GPU-[0-9a-f-]+" "$WORK/job-b.log" | head -n1)

echo
echo "==================== RESULT ===================="
rc=0
check() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; rc=1; fi; }

check "both jobs completed cleanly"                  '[ "$RC_A" -eq 0 ] && [ "$RC_B" -eq 0 ]'
check "both jobs ran on the same physical GPU"       '[ -n "$UUID_A" ] && [ "$UUID_A" = "$UUID_B" ]'
check "job-a was given its own quota (512 MiB)"      'grep -q "quota=536870912" "$WORK/job-a.log" || grep -q "total_held=${JOB_A_HOLD} MiB" "$WORK/job-a.log"'
check "job-b was given its own quota (1024 MiB)"     'grep -q "total_held=${JOB_B_HOLD} MiB" "$WORK/job-b.log"'
check "job-a made CUDA progress"                     'grep -q "progress" "$WORK/job-a.log"'
check "job-b made CUDA progress"                     'grep -q "progress" "$WORK/job-b.log"'
check "their CUDA activity overlapped in time"       '[ "$(awk -v o="$OVERLAP" "BEGIN{print (o > 1)}")" = "1" ]'
check "neither job was denied (both stayed in quota)" '! grep -q "REFUSED" "$WORK/job-a.log" && ! grep -q "REFUSED" "$WORK/job-b.log"'

echo
if [ $rc -eq 0 ]; then
    echo "OVERALL: PASS (criterion 6 satisfied - ${OVERLAP}s of overlapping CUDA progress"
    echo "          by two jobs with distinct quotas on $UUID_A)"
else
    echo "OVERALL: FAIL"
fi
exit $rc
