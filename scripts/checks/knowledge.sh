#!/usr/bin/env bash
# Feature: knowledge management -- compile_lifecycle (dry-run purity, decay,
# reinforce, falsify + retrieval filtering, verify related-context advisories),
# personas (two-tier discovery, write_reflection, disabled rejection), and
# vault history (vault_history -> restore_note round trip). The history.md
# dashboard is asserted below against the sandbox vault root.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

require_env ollama
setup_sandbox
build_bin
boot_daemon

harness knowledge || fail "knowledge harness failed"
pass "lifecycle + verify + personas + vault_history/restore over MCP"

# The compile pass regenerates the read-only history.md dashboard at the vault
# root, carrying copy-paste restore commands.
HISTORY_MD="$XDG_DATA_HOME/cognosis/kb/history.md"
[ -f "$HISTORY_MD" ] || fail "generated history.md dashboard missing at vault root"
grep -q "cognosis vault restore" "$HISTORY_MD" || fail "history.md carries no restore command"
pass "read-only history.md dashboard maintained at the vault root"

echo
echo "knowledge check: all criteria pass"
