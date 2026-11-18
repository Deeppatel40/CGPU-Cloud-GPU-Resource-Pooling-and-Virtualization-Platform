#!/usr/bin/env bash
set -e

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

echo "================================================================="
echo "               CGPU CLUSTER & AGENT STATUS                       "
echo "================================================================="
echo ""
echo "--- 🖥️  KUBERNETES NODES ---"
sshpass -p 'Nishit' ssh -o StrictHostKeyChecking=no -o ConnectTimeout=3 g6@10.20.24.31 'kubectl get nodes -o wide' 2>/dev/null || echo "Unable to query PC 1"

echo ""
echo "--- 📦 ACTIVE PODS ACROSS ALL NODES ---"
sshpass -p 'Nishit' ssh -o StrictHostKeyChecking=no -o ConnectTimeout=3 g6@10.20.24.31 'kubectl get pods -A -o wide' 2>/dev/null || echo "Unable to query pods"

echo ""
echo "--- 🚀 CGPU SCHEDULER & POOL TELEMETRY ---"
curl -s http://10.20.24.31:8082/api/v1/pool 2>/dev/null || echo "CGPU Scheduler not reachable on :8082"
echo ""

echo ""
echo "================================================================="
