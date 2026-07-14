#!/usr/bin/env bash
# Feature: zero-downtime embedding-provider migration. The under-load proof
# (5k-chunk corpus, continuous query load, pause/resume, kill-mid-batch,
# rollback) lives in the Go test suite; this runs it, then checks the live
# daemon surfaces (status, dry-run, prune guard, get_migration_status).
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

require_env ollama

# --- 1. the under-load proof (the heart of the feature) ----------------------
( cd "$ROOT" && COGNOSIS_TEST_DSN="$COGNOSIS_DSN" go test -race -timeout 300s ./internal/migrate/ ./internal/store/ ) \
  || fail "migration test suite failed"
pass "migration suite green under -race (zero-downtime, pause/resume, kill-mid-batch, rollback)"

# --- 2. live-daemon surfaces -------------------------------------------------
setup_sandbox
build_bin
boot_daemon

"$BIN" embeddings status | grep -qi "no migration in progress\|complete\|rolled_back" \
  || fail "embeddings status did not report an idle state"
pass "embeddings status reports idle state"

OUT="$("$BIN" embeddings migrate --from ollama/nomic-embed-text:v1.5 --to ollama/nomic-embed-text:v2-test --dry-run)"
echo "$OUT" | grep -q "dry run" || fail "dry-run output unexpected: $OUT"
"$BIN" embeddings status | grep -qi "no migration in progress\|complete\|rolled_back" \
  || fail "dry run left state behind"
pass "migrate --dry-run plans and writes nothing"

set +e
OUT="$("$BIN" embeddings prune ollama/nomic-embed-text:v1.5 2>&1)"; RC=$?
set -e
[ "$RC" -ne 0 ] || fail "prune dropped the active provider"
echo "$OUT" | grep -qi "active" || fail "prune refusal does not explain itself: $OUT"
pass "prune refuses the active provider"

harness migration || fail "get_migration_status harness failed"
pass "get_migration_status answers over MCP"

echo
echo "embedding-migration check: all criteria pass"
