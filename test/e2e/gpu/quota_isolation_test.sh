#!/usr/bin/env bash
# A3 criterion 7, complete form - enforcement AND isolation in one run.
#
# guard_quota_test.sh proves an over-quota allocation is refused. The criterion also requires
# that "the other workload continues" while that happens. This test captures both facts in a
# single run: a victim job is denied mid-flight while a neighbour job on the same physical GPU
# keeps making CUDA progress, unaffected.
#
#   job-neighbour  quota 1024 MiB, holds 512 MiB, runs for the whole window
#   job-victim     quota  256 MiB, tries to take 1024 MiB -> denied partway
#
# Both requests together (1536 MiB) fit comfortably in the card's 6141 MiB, so the denial
# cannot be the device running out of memory.
#
# Usage: ./quota_isolation_test.sh [guard-dir] [probe-dir]
set -uo pipefail

GUARD_DIR="${1:-/tmp/cgpu-guard-build}"
PROBE_DIR="${2:-/tmp/cgpu-probe-build}"
IMG=docker.io/nvidia/cuda:12.6.2-base-ubuntu24.04
NVRT=/usr/bin/nvidia-container-runtime

NB_QUOTA=1024; NB_HOLD=512; NB_SECONDS=14
VI_QUOTA=256;  VI_REQUEST=1024

for f in "$GUARD_DIR/libcgpu_guard.so" "$PROBE_DIR/cgpu_alloc_probe"; do
    [ -f "$f" ] || { echo "FAIL: missing $f - run the build.sh scripts first" >&2; exit 1; }
done

FREE_MIB=$(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | tr -d ' ')
TOTAL_REQ=$((NB_HOLD + VI_REQUEST))
echo "neighbour holds ${NB_HOLD} MiB, victim requests ${VI_REQUEST} MiB (total ${TOTAL_REQ} MiB)"
echo "physically free: ${FREE_MIB} MiB"
if [ "$FREE_MIB" -le "$TOTAL_REQ" ]; then
    echo "FAIL: only ${FREE_MIB} MiB free; a denial would be ambiguous" >&2
    exit 1
fi
echo "=> both would fit physically, so any denial is quota enforcement"
echo

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

launch() { # job, quota, target, seconds, extra-args...
    local job="$1" quota="$2" target="$3" secs="$4"; shift 4
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
            --target-mib "$target" --chunk-mib 128 --seconds "$secs" "$@" \
        > "$WORK/$job.log" 2>&1
}

# Neighbour starts first and runs through the whole window.
launch job-neighbour "$NB_QUOTA" "$NB_HOLD" "$NB_SECONDS" &
PID_NB=$!

# Give it time to allocate and get into its activity phase, so the denial lands mid-flight.
sleep 4

launch job-victim "$VI_QUOTA" "$VI_REQUEST" 0 --expect-denial &
PID_VI=$!

wait $PID_VI; RC_VI=$?
echo "victim finished (exit=$RC_VI) - neighbour still running:"
nvidia-smi --query-gpu=memory.used,utilization.gpu --format=csv,noheader | sed 's/^/  /'

wait $PID_NB; RC_NB=$?

echo
echo "---------------- job-victim (denied) ----------------"
grep -E "uuid|alloc|RESULT" "$WORK/job-victim.log"
echo
echo "---------------- job-neighbour (unaffected) ----------------"
grep -E "uuid|alloc|progress|freed|RESULT" "$WORK/job-neighbour.log"

# Did the neighbour keep progressing after the victim was denied?
DENY_TS=$(grep -oE "^\[[0-9:.]+\]" "$WORK/job-victim.log" | tail -n1 | tr -d '[]')
to_s() { awk -F: '{printf "%.3f", $1*3600 + $2*60 + $3}' <<<"$1"; }
DENY_S=$(to_s "$DENY_TS")
AFTER=0
while read -r ts; do
    [ "$(awk -v a="$(to_s "$ts")" -v b="$DENY_S" 'BEGIN{print (a > b)}')" = "1" ] && AFTER=$((AFTER + 1))
done < <(grep "progress" "$WORK/job-neighbour.log" | grep -oE "^\[[0-9:.]+\]" | tr -d '[]')

UUID_V=$(grep -oE "GPU-[0-9a-f-]+" "$WORK/job-victim.log" | head -n1)
UUID_N=$(grep -oE "GPU-[0-9a-f-]+" "$WORK/job-neighbour.log" | head -n1)

echo
echo "neighbour progress reports after the victim's denial: $AFTER"
echo
echo "==================== RESULT ===================="
rc=0
check() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; rc=1; fi; }

check "both jobs were on the same physical GPU"          '[ -n "$UUID_V" ] && [ "$UUID_V" = "$UUID_N" ]'
check "victim was allowed up to its quota (256 MiB)"     'grep -q "total_held=256 MiB" "$WORK/job-victim.log"'
check "victim's over-quota allocation was refused"       'grep -q "REFUSED rc=2" "$WORK/job-victim.log"'
check "victim observed the denial and exited cleanly"    '[ "$RC_VI" -eq 0 ]'
check "neighbour was never denied"                       '! grep -q "REFUSED" "$WORK/job-neighbour.log"'
check "neighbour held its full 512 MiB throughout"       'grep -q "holding=512 MiB" "$WORK/job-neighbour.log"'
check "neighbour kept making CUDA progress after the denial" '[ "$AFTER" -ge 3 ]'
check "neighbour completed successfully"                 '[ "$RC_NB" -eq 0 ] && grep -q "RESULT=OK" "$WORK/job-neighbour.log"'

echo
if [ $rc -eq 0 ]; then
    echo "OVERALL: PASS (criterion 7 fully satisfied - one job denied by its quota while a"
    echo "          neighbour on $UUID_V continued uninterrupted, $AFTER progress reports after)"
else
    echo "OVERALL: FAIL"
fi
exit $rc
