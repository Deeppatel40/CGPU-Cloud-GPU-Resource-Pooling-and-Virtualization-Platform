#!/usr/bin/env bash
set -e

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

echo "🤖 [Nishit-Agent Watcher] Starting autonomous loop for @nishit-agent..."
echo "Monitoring COMMS.md for incoming tasks from @deep-agent..."

while true; do
  git fetch upstream main >/dev/null 2>&1 || true
  LOCAL=$(git rev-parse HEAD)
  REMOTE=$(git rev-parse upstream/main)

  if [ "$LOCAL" != "$REMOTE" ]; then
    echo "📥 New commit detected from upstream! Pulling..."
    git pull --rebase upstream main

    LAST_HEADER=$(grep -E "^### .* — @.* — " COMMS.md | tail -n 1 || true)
    echo "Latest entry: $LAST_HEADER"

    if echo "$LAST_HEADER" | grep -q "@deep-agent"; then
      echo "⚡ Triggering Antigravity CLI to process NEEDS from @deep-agent..."
      /Users/nishit/.local/bin/agy -p "Pull main. Read the latest entry in COMMS.md. Address the NEEDS requested by @deep-agent by executing any necessary commands on Ubuntu PC 1 (g6@10.20.24.31, pass: Nishit) or PC 2 (g6@10.20.24.32, pass: Nishit). Then append your entry to COMMS.md following the exact rules and format (STATUS, DID, NEEDS), git commit and push to upstream main and origin main. If Issue A1 is fully verified, update GitHub Issue #7 accordingly." --dangerously-skip-permissions
      echo "✅ Turn completed by @nishit-agent."
    else
      echo "⏳ Waiting for @deep-agent..."
    fi
  fi

  sleep 5
done
