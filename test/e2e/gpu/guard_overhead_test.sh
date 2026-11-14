#!/usr/bin/env bash
# A3 KPI - what does the virtualisation layer cost?
#
# Owner / engineer: Deep Patel (@Deeppatel40)
#
# Every other test in this directory asks whether the guard *works*. This one asks what it
# costs, which is the question that decides whether CGPU is usable: a quota mechanism that
# halves GPU throughput is not a sharing platform, it is a tax.
#
# Two measurements, because the guard's cost is not uniform:
#
#   1. Allocation path - interposed. Every cudaMalloc/cuMemAlloc goes through the guard's
#      accounting (a mutex, an addition, a bounds check) before reaching the driver. This is
#      where overhead, if any, must appear. Measured with many small allocations, which is
#      the worst case: the per-call cost is amortised over the least work.
#
#   2. Compute path - NOT interposed. Kernel launches do not touch the guard at all, so the
#      expectation is no measurable difference. Stating that expectation and then testing it
#      is the point: it shows the measurement can detect a difference where one exists.
#
# Each configuration is run REPS times and the median is reported, because a single run on a
# laptop GPU is noisy - clocks boost and throttle, and the first run of any CUDA process pays
# context creation.
#
# Usage: ./guard_overhead_test.sh [guard-dir] [probe-dir]
set -uo pipefail

GUARD_DIR="${1:-/tmp/cgpu-guard-build}"
PROBE_DIR="${2:-/tmp/cgpu-probe-build}"
IMG=docker.io/nvidia/cuda:12.6.2-base-ubuntu24.04
NVRT=/usr/bin/nvidia-container-runtime
REPS="${CGPU_OVERHEAD_REPS:-5}"

for f in "$GUARD_DIR/libcgpu_guard.so" "$PROBE_DIR/cgpu_alloc_probe"; do
    [ -f "$f" ] || { echo "FAIL: missing $f - run the build.sh scripts first" >&2; exit 1; }
done

# Runs the probe with the guard either preloaded or absent, and prints elapsed seconds.
# Timed around the container so the number includes nothing the guard does not affect
# differently between the two arms - the image, runtime and probe are identical.
timed() { # mode(guard|bare), probe args...
    local mode="$1"; shift
    local -a env_args=()
    if [ "$mode" = guard ]; then
        env_args=(--env LD_PRELOAD=/opt/cgpu/libcgpu_guard.so
                  --env CGPU_ALLOCATED_VRAM_MIB=4096
                  --env "CGPU_JOB_ID=overhead-$mode")
    fi
    local t0 t1
    t0=$(date +%s.%N)
    k3s ctr run --rm \
        --runc-binary "$NVRT" \
        --env NVIDIA_VISIBLE_DEVICES=all \
        --env NVIDIA_DRIVER_CAPABILITIES=compute,utility \
        "${env_args[@]}" \
        --mount "type=bind,src=$GUARD_DIR,dst=/opt/cgpu,options=rbind:ro" \
        --mount "type=bind,src=$PROBE_DIR,dst=/opt/cgpu-test,options=rbind:ro" \
        "$IMG" "cgpu-ovh-$mode-$RANDOM" \
        /opt/cgpu-test/cgpu_alloc_probe --label "ovh-$mode" "$@" >/dev/null 2>&1
    t1=$(date +%s.%N)
    awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.3f", b - a}'
}

median() { printf '%s\n' "$@" | sort -n | awk '{v[NR]=$1} END{
    if (NR % 2) printf "%.3f", v[(NR+1)/2];
    else printf "%.3f", (v[NR/2] + v[NR/2+1]) / 2 }'; }

run_arm() { # label, mode, args...  -> prints ONLY the median on stdout
    local label="$1" mode="$2"; shift 2
    local -a samples=()
    for ((i = 0; i < REPS; i++)); do
        samples+=("$(timed "$mode" "$@")")
    done
    # The human-readable line goes to stderr on purpose: stdout is captured by the caller
    # via $(...) and must carry the median alone, or the number is unparseable.
    echo "  $label: $(median "${samples[@]}")s   (samples: ${samples[*]})" >&2
    median "${samples[@]}"
}

echo "guard overhead, median of $REPS runs per arm"
echo

# ---- 1. allocation-heavy: 2048 MiB in 4 MiB chunks = 512 interposed allocations ----
echo "==================== allocation path (512 x 4 MiB) ===================="
ALLOC_ARGS=(--target-mib 2048 --chunk-mib 4 --seconds 0)
A_BARE=$(run_arm "without guard" bare  "${ALLOC_ARGS[@]}")
A_GUARD=$(run_arm "with guard   " guard "${ALLOC_ARGS[@]}")

# ---- 2. compute-heavy: one allocation, then 10 s of kernels ----
echo
echo "==================== compute path (10 s of kernels) ===================="
COMPUTE_ARGS=(--target-mib 256 --chunk-mib 128 --seconds 10)
C_BARE=$(run_arm "without guard" bare  "${COMPUTE_ARGS[@]}")
C_GUARD=$(run_arm "with guard   " guard "${COMPUTE_ARGS[@]}")

pct() { awk -v g="$1" -v b="$2" 'BEGIN{ if (b <= 0) print "n/a"; else printf "%+.1f", (g - b) / b * 100 }'; }
A_PCT=$(pct "$A_GUARD" "$A_BARE")
C_PCT=$(pct "$C_GUARD" "$C_BARE")

echo
echo "==================== KPI ===================="
printf "allocation path : %ss -> %ss   (%s%%)\n" "$A_BARE" "$A_GUARD" "$A_PCT"
printf "compute path    : %ss -> %ss   (%s%%)\n" "$C_BARE" "$C_GUARD" "$C_PCT"
echo
echo "Both numbers include container start and CUDA context creation, which dominate a run"
echo "this short - so they are an upper bound on the guard's share, not an isolated measure"
echo "of it. Treat them as 'what a job actually experiences', which is the useful framing."
echo

rc=0
check() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; rc=1; fi; }

# The thresholds are deliberately loose. This is a laptop GPU with boost clocks and a shared
# host; asserting a tight bound would produce a flaky test that gets ignored, which is worse
# than no test. What must hold is that the guard does not cost anything near enough to
# change a scheduling decision.
check "allocation path within 25% of unguarded" \
    '[ "$(awk -v p="$A_PCT" "BEGIN{print (p < 25)}")" = "1" ]'
check "compute path within 15% of unguarded (kernels are not interposed)" \
    '[ "$(awk -v p="$C_PCT" "BEGIN{print (p < 15)}")" = "1" ]'
check "both arms actually ran" \
    '[ "$(awk -v a="$A_BARE" -v c="$C_BARE" "BEGIN{print (a > 0 && c > 0)}")" = "1" ]'

echo
[ $rc -eq 0 ] && echo "OVERALL: PASS (guard overhead is not scheduling-relevant)" \
             || echo "OVERALL: FAIL"
exit $rc
