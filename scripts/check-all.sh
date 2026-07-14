#!/usr/bin/env bash
# Run every feature check under scripts/checks/ in order. Requires COGNOSIS_DSN
# (pg-start) and a local Ollama with the pinned embedding model pulled; also run
# by `mage check`.
set -euo pipefail

cd "$(dirname "$0")/.."
CHECKS_DIR="scripts/checks"

# daemon first (no Ollama needed), then the MCP-driven feature checks.
order=(daemon memory-loop retrieval knowledge platform embedding-migration)

for name in "${order[@]}"; do
  echo "==> $name"
  bash "$CHECKS_DIR/$name.sh"
  echo
done

echo "check-all: all feature checks pass"
