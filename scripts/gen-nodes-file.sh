#!/usr/bin/env bash
# Generate the scheduler's -nodes-file with LIVE node IPs taken from Kubernetes.
#
# Why this exists:
#   cmd/cgpu-scheduler/main.go's bootstrapClusterNodes() hardcodes each worker's IP
#   (10.20.24.35 / 10.20.24.36). The GPU workers are laptops on Wi-Fi, so their address
#   changes whenever they rejoin - and when it does, the portal shows a stale IP and
#   telemetry fails permanently, because the API probes the hardcoded address on port 9400:
#
#     Get "http://10.20.24.35:9400/inventory": context deadline exceeded
#
#   The scheduler already supports overriding the built-in list with -nodes-file (or
#   CGPU_NODES_FILE), so no code change is needed: this script fills that file with the
#   addresses Kubernetes actually reports, and the GPU facts that kubectl cannot know.
#
#   The underlying fix belongs in cmd/cgpu-scheduler/reconcile.go, which already fetches
#   `kubectl get nodes -o json` every 30s but reads only Status.Conditions - the InternalIP
#   it needs is in the response it already has, and is discarded. Until that lands, run this
#   on the control plane before a demo, and after any laptop rejoins on a new address.
#
# Usage (on the control plane):
#   scripts/gen-nodes-file.sh > /etc/cgpu/nodes.json
#   CGPU_NODES_FILE=/etc/cgpu/nodes.json <restart the scheduler>
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"

# GPU facts per node, keyed by Kubernetes node name: uuid|name|vram_mib|model
# These come from nvidia-smi on each worker and are recorded in docs/gpu-worker-4050.md.
declare -A GPU_FACTS=(
  [cgpu-gpu-worker-4050]="GPU-04280514-80a3-1dcf-1c5e-94e374365bef|NVIDIA GeForce RTX 4050 Laptop GPU|6141|rtx4050"
  [cgpu-gpu-worker-3050]="GPU-7e8adcd7-96a2-1fa6-7936-445a0f2643e6|NVIDIA GeForce RTX 3050 Laptop GPU|4096|rtx3050"
)

command -v "$KUBECTL" >/dev/null || { echo "kubectl not found" >&2; exit 1; }

first=1
echo "["
for node in "${!GPU_FACTS[@]}"; do
    IFS='|' read -r uuid name vram model <<<"${GPU_FACTS[$node]}"

    # The address Kubernetes reports, which is the one the kubelet is actually reachable on.
    ip=$("$KUBECTL" get node "$node" \
            -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
    if [ -z "$ip" ]; then
        echo "  warning: $node not found in the cluster, skipping" >&2
        continue
    fi

    ready=$("$KUBECTL" get node "$node" \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
    status="OFFLINE"; [ "$ready" = "True" ] && status="ONLINE"

    [ $first -eq 0 ] && echo ","
    first=0
    cat <<JSON
  {
    "node_id": "$node",
    "hostname": "$node",
    "kubernetes_node": "$node",
    "ip_address": "$ip",
    "status": "$status",
    "labels": {
      "cgpu.io/role": "gpu-worker",
      "cgpu.io/gpu-model": "$model",
      "cgpu.io/gpu-vram": "${vram}MiB"
    },
    "gpus": [
      {
        "gpu_id": "$uuid",
        "index": 0,
        "name": "$name",
        "total_vram_mib": $vram,
        "allocated_vram_mib": 0,
        "free_vram_mib": $vram,
        "health_status": "HEALTHY",
        "active_allocations": []
      }
    ]
  }
JSON
    echo "  -> $node $status at $ip" >&2
done
echo
echo "]"
