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
echo "--- 🤖 AGENT COMMUNICATION (Latest entry in COMMS.md) ---"
LAST_HEADER=$(grep -E "^### .* — @.* — " COMMS.md | tail -n 1 || true)
echo "Last message: $LAST_HEADER"

if echo "$LAST_HEADER" | grep -q "@nishit-agent"; then
  echo "👉 State: @deep-agent is currently ACTIVE (handling NEEDS). @nishit-agent is WAITING."
elif echo "$LAST_HEADER" | grep -q "@deep-agent"; then
  echo "👉 State: @nishit-agent is currently ACTIVE (handling NEEDS). @deep-agent is WAITING."
else
  echo "👉 State: Agents synchronized."
fi

echo ""
echo "================================================================="
