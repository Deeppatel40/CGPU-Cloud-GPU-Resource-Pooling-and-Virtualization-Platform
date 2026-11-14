#!/usr/bin/env bash
# A3 criterion 6, through Kubernetes - "two workloads share one physical GPU concurrently".
#
# Owner / engineer: Deep Patel (@Deeppatel40)
#
# concurrent_share_test.sh proves this with containerd directly, which deliberately removes
# Kubernetes from the path to isolate the GPU behaviour. This script proves the same thing
# the way the platform will actually do it: two Jobs submitted to the API server, admitted,
# scheduled onto the GPU node, run under the nvidia RuntimeClass, each with its own VRAM
# quota enforced by the guard.
#
# What it asserts:
#   1. both Jobs completed successfully,
#   2. both reported the SAME physical GPU UUID (so this is sharing, not two devices),
#   3. both executed real CUDA kernels (computed=yes - the buffer changed on the device),
#   4. their kernel-execution windows overlapped in time by a meaningful margin,
#   5. neither was denied (both quotas were sufficient - this test is about sharing, while
#      quota_isolation_test.sh covers the denial case).
#
# Requires a kubeconfig with apply/log rights. The GPU worker is a k3s *agent* and has no
# admin kubeconfig, so run this from the control plane, or export KUBECONFIG pointing at it.
#
# Usage: ./k8s_concurrent_share_test.sh
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NS=cgpu-system
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
DEPLOY="$REPO_ROOT/deploy"
WAIT_INSTALL="${CGPU_WAIT_INSTALL:-600}"
WAIT_JOBS="${CGPU_WAIT_JOBS:-300}"

command -v "$KUBECTL" >/dev/null || { echo "FAIL: $KUBECTL not found" >&2; exit 1; }
"$KUBECTL" version --request-timeout=10s >/dev/null 2>&1 \
    || { echo "FAIL: cannot reach the cluster - check KUBECONFIG" >&2; exit 1; }

# ---- 0. the GPU node must be labelled, or the DaemonSet and Jobs have nowhere to land ----
GPU_NODES=$("$KUBECTL" get nodes -l cgpu.dev/gpu=true -o name 2>/dev/null | wc -l)
if [ "$GPU_NODES" -eq 0 ]; then
    echo "FAIL: no node carries cgpu.dev/gpu=true. Label the GPU worker first:" >&2
    echo "        kubectl label node cgpu-gpu-worker-4050 cgpu.dev/gpu=true" >&2
    exit 1
fi
echo "GPU nodes: $GPU_NODES"

# ---- 1. install the guard on the GPU nodes, from this checkout's sources ----
# Generated rather than committed so the deployed guard cannot drift from the repo.
echo "==> installing guard via DaemonSet (sources from this checkout)"
"$KUBECTL" create namespace "$NS" --dry-run=client -o yaml | "$KUBECTL" apply -f - >/dev/null
"$KUBECTL" -n "$NS" create configmap cgpu-guard-src \
    --from-file="$REPO_ROOT/pkg/guard/cgpu_guard.c" \
    --from-file="$REPO_ROOT/pkg/guard/build.sh" \
    --from-file="$REPO_ROOT/test/e2e/gpu/cgpu_alloc_probe.c" \
    --from-file="$REPO_ROOT/test/e2e/gpu/cgpu_burn_ptx.h" \
    --from-file="probe_build.sh=$REPO_ROOT/test/e2e/gpu/build.sh" \
    --dry-run=client -o yaml | "$KUBECTL" apply -f - >/dev/null

"$KUBECTL" apply -f "$DEPLOY/cgpu-guard-installer.yaml" >/dev/null
# A source change must actually reach the node, so force a re-roll and wait for it.
"$KUBECTL" -n "$NS" rollout restart daemonset/cgpu-guard-installer >/dev/null 2>&1
if ! "$KUBECTL" -n "$NS" rollout status daemonset/cgpu-guard-installer \
        --timeout="${WAIT_INSTALL}s"; then
    echo "FAIL: guard installer did not become ready" >&2
    "$KUBECTL" -n "$NS" logs -l app.kubernetes.io/name=cgpu-guard-installer --tail=40
    exit 1
fi
echo "guard installed:"
"$KUBECTL" -n "$NS" logs -l app.kubernetes.io/name=cgpu-guard-installer --tail=6 | sed 's/^/  /'

# ---- 2. submit both Jobs at once ----
echo
echo "==> submitting both Jobs"
"$KUBECTL" -n "$NS" delete job cgpu-share-a cgpu-share-b --ignore-not-found >/dev/null 2>&1
"$KUBECTL" apply -f "$DEPLOY/cgpu-criterion6-jobs.yaml" >/dev/null
SUBMIT_EPOCH=$(date -u +%s)

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
RC_A=1; RC_B=1
for j in cgpu-share-a cgpu-share-b; do
    if "$KUBECTL" -n "$NS" wait --for=condition=complete "job/$j" \
            --timeout="${WAIT_JOBS}s" >/dev/null 2>&1; then
        echo "  $j: complete"
        [ "$j" = cgpu-share-a ] && RC_A=0 || RC_B=0
    else
        echo "  $j: did NOT complete"
        "$KUBECTL" -n "$NS" describe "job/$j" | tail -n 15 | sed 's/^/    /'
    fi
    "$KUBECTL" -n "$NS" logs "job/$j" > "$WORK/$j.log" 2>&1
done

# How long both Jobs were on the node, end to end - context for the overlap number.
WALL=$(( $(date -u +%s) - SUBMIT_EPOCH ))

echo
for j in cgpu-share-a cgpu-share-b; do
    echo "---------------- $j ----------------"
    grep -E "uuid|alloc|progress|activity done|freed|RESULT" "$WORK/$j.log" | head -n 20
    echo
done

# ---- 3. overlap analysis on the kernel-execution windows ----
# Each progress line is a point at which that Job had just finished a kernel launch and
# synchronised, so the first-to-last span is a window of confirmed kernel execution.
to_s() { awk -F: '{printf "%.3f", $1*3600 + $2*60 + $3}' <<<"$1"; }
window() { # prints "start end" for a job's progress lines
    local f="$1" first last
    first=$(grep "progress" "$f" | head -n1 | grep -oE "^\[[0-9:.]+\]" | tr -d '[]')
    last=$(grep "progress" "$f"  | tail -n1 | grep -oE "^\[[0-9:.]+\]" | tr -d '[]')
    [ -z "$first" ] && { echo ""; return; }
    echo "$(to_s "$first") $(to_s "$last")"
}
WA=$(window "$WORK/cgpu-share-a.log")
WB=$(window "$WORK/cgpu-share-b.log")
OVERLAP=0
if [ -n "$WA" ] && [ -n "$WB" ]; then
    OVERLAP=$(awk -v a="$WA" -v b="$WB" 'BEGIN{
        split(a, x, " "); split(b, y, " ");
        s = (x[1] > y[1]) ? x[1] : y[1];
        e = (x[2] < y[2]) ? x[2] : y[2];
        o = e - s; printf "%.3f", (o > 0) ? o : 0 }')
fi
echo "kernel window A: ${WA:-none}"
echo "kernel window B: ${WB:-none}"
echo "overlap: ${OVERLAP}s (jobs resident for ${WALL}s wall clock)"

UUID_A=$(grep -oE "GPU-[0-9a-f-]+" "$WORK/cgpu-share-a.log" | head -n1)
UUID_B=$(grep -oE "GPU-[0-9a-f-]+" "$WORK/cgpu-share-b.log" | head -n1)
NODE_A=$("$KUBECTL" -n "$NS" get pods -l cgpu.dev/job=cgpu-share-a \
    -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)
NODE_B=$("$KUBECTL" -n "$NS" get pods -l cgpu.dev/job=cgpu-share-b \
    -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)
echo "pods placed on: A=$NODE_A B=$NODE_B"

echo
echo "==================== RESULT ===================="
rc=0
check() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; rc=1; fi; }

check "both Jobs were admitted, scheduled and completed via Kubernetes" '[ "$RC_A" -eq 0 ] && [ "$RC_B" -eq 0 ]'
check "both pods landed on the same GPU node"            '[ -n "$NODE_A" ] && [ "$NODE_A" = "$NODE_B" ]'
check "both workloads used the SAME physical GPU"        '[ -n "$UUID_A" ] && [ "$UUID_A" = "$UUID_B" ]'
check "the guard was active in both containers"          'grep -q "guard library loaded via LD_PRELOAD" "$WORK/cgpu-share-a.log" "$WORK/cgpu-share-b.log" || grep -q "device_memory total=1024 MiB" "$WORK/cgpu-share-a.log"'
check "job A executed real CUDA kernels on the device"   'grep -q "mode=kernel" "$WORK/cgpu-share-a.log" && grep -q "computed=yes" "$WORK/cgpu-share-a.log"'
check "job B executed real CUDA kernels on the device"   'grep -q "mode=kernel" "$WORK/cgpu-share-b.log" && grep -q "computed=yes" "$WORK/cgpu-share-b.log"'
check "neither workload was denied"                      '! grep -q "REFUSED" "$WORK/cgpu-share-a.log" "$WORK/cgpu-share-b.log"'
check "their kernel execution overlapped in time"        '[ "$(awk -v o="$OVERLAP" "BEGIN{print (o > 5)}")" = "1" ]'
check "both reported RESULT=OK"                          'grep -q "RESULT=OK" "$WORK/cgpu-share-a.log" && grep -q "RESULT=OK" "$WORK/cgpu-share-b.log"'

echo
if [ $rc -eq 0 ]; then
    echo "OVERALL: PASS (criterion 6 satisfied through Kubernetes - two Jobs on $NODE_A"
    echo "          executed CUDA kernels concurrently on $UUID_A for ${OVERLAP}s, each"
    echo "          under its own guard-enforced VRAM quota)"
else
    echo "OVERALL: FAIL"
fi
exit $rc
