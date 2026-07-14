#!/usr/bin/env bash
# Feature: the core memory loop — MCP listening on loopback, then
# write_note -> query_knowledge (hybrid-ranked) -> get_note round trip ->
# list_notes, plus contract enforcement and tokenless-401 (harness slice).
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

require_env ollama
setup_sandbox
build_bin
boot_daemon

for _ in $(seq 1 100); do curl -s -o /dev/null "$URL" && break; sleep 0.1; done
"$BIN" status | grep -q "mcp.*ok" || fail "status does not report MCP listening"
pass "daemon up, MCP listening on $URL"

harness memory-loop || fail "memory-loop harness failed"
pass "write -> hybrid query -> get -> list round trip over authenticated MCP"

echo
echo "memory-loop check: all criteria pass"
