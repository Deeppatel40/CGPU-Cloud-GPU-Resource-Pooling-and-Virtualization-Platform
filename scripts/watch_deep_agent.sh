#!/usr/bin/env bash
set -e

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

echo "🤖 [Deep-Agent Watcher] Starting autonomous loop for @deep-agent..."
echo "Monitoring COMMS.md for incoming tasks from @nishit-agent..."

while true; do
  git fetch origin main >/dev/null 2>&1 || true
  LOCAL=$(git rev-parse HEAD)
  REMOTE=$(git rev-parse origin/main)

  if [ "$LOCAL" != "$REMOTE" ]; then
    echo "📥 New commit detected! Pulling..."
    git pull --rebase origin main

    LAST_HEADER=$(grep -E "^### .* — @.* — " COMMS.md | tail -n 1 || true)
    echo "Latest entry: $LAST_HEADER"

    if echo "$LAST_HEADER" | grep -q "@nishit-agent"; then
      echo "⚡ Triggering Claude Code to process NEEDS from @nishit-agent..."
      claude -p "Pull main. Read the latest entry in COMMS.md. Address the NEEDS requested by @nishit-agent by executing any necessary commands or checks on your machine/WSL. Then append your entry to COMMS.md following the rules and format in the file (STATUS, DID, NEEDS), git commit and push to main. If Issue A1 is fully satisfied, declare completion in STATUS." --dangerously-skip-permissions
      echo "✅ Turn completed by @deep-agent."
    else
      echo "⏳ Waiting for @nishit-agent..."
    fi
  fi

  sleep 5
done
