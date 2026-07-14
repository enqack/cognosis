#!/usr/bin/env bash
# Feature: retrieval extensions — cached summaries in hits, as_of temporal
# views, list_decaying, and soft-delete hygiene (archived/faded excluded by
# default, surfaced with include_archived). The archived-link RRF penalty is
# proven deterministically by the query unit goldens (go test ./internal/query).
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

require_env ollama
setup_sandbox
build_bin
boot_daemon

harness retrieval || fail "retrieval harness failed"
pass "summaries + as_of + list_decaying + archived-exclusion over MCP"

echo
echo "retrieval check: all criteria pass"
