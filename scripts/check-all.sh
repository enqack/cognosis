#!/usr/bin/env bash
# Run every feature check under scripts/checks/ in order; also run by `mage
# check`. Checks want COGNOSIS_DSN (pg-start) and a local Ollama with the pinned
# embedding model pulled -- a check whose prerequisites are missing reports itself
# skipped (exit 2 from _lib.sh's require_env) and the run carries on, so a
# partial environment still gets whatever signal it can. A real failure (exit 1)
# still stops the run at the first one.
set -euo pipefail

cd "$(dirname "$0")/.."
CHECKS_DIR="scripts/checks"

# daemon first (no Ollama needed), then the MCP-driven feature checks.
order=(daemon memory-loop retrieval knowledge platform tls embedding-migration retrieval-eval)

skipped=()

for name in "${order[@]}"; do
  echo "==> $name"
  rc=0
  # `|| rc=$?` keeps set -e from killing the run on a non-zero exit, so the
  # case below gets to tell a skip apart from a failure.
  bash "$CHECKS_DIR/$name.sh" || rc=$?
  case "$rc" in
    0) ;;
    2) echo "SKIP: $name (prerequisite missing; see above)"; skipped+=("$name") ;;
    *) echo "check-all: $name failed (exit $rc)" >&2; exit "$rc" ;;
  esac
  echo
done

ran=$(( ${#order[@]} - ${#skipped[@]} ))
if [ "${#skipped[@]}" -eq 0 ]; then
  echo "check-all: all ${#order[@]} feature checks pass"
elif [ "$ran" -eq 0 ]; then
  echo "check-all: every check skipped -- nothing was verified (needs COGNOSIS_DSN + Ollama)" >&2
  exit 2
else
  echo "check-all: $ran passed, ${#skipped[@]} skipped (${skipped[*]})"
fi
