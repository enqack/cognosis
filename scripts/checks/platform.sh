#!/usr/bin/env bash
# Feature: platform surfaces -- auth (synchronous revocation) + audit-trail
# redaction, session context injection (marker-gated), git commit capture, and
# the shipped hook/service artifacts.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

require_env ollama
setup_sandbox
build_bin
boot_daemon

# --- 1. audit trail: rows recorded, note content never leaked (harness) ------
harness platform || fail "platform audit harness failed"
pass "every tool call audited; args_summary carries identifiers, never note content"

# --- 2. token lifecycle: synchronous revocation, name reuse, prune -----------
CI_NAME="ci-revoke-$$-$RANDOM"   # scope to this run; uniqueness is live-scoped, revoke frees the name
CI_TOKEN="$("$BIN" token create "$CI_NAME" | head -1)"
CODE="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $CI_TOKEN" "$URL/context?budget=100")"
[ "$CODE" = "200" ] || fail "fresh token rejected ($CODE)"
"$BIN" token revoke "$CI_NAME" >/dev/null
CODE="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $CI_TOKEN" "$URL/context?budget=100")"
[ "$CODE" = "401" ] || fail "revoked token still accepted ($CODE)"
CODE="$(curl -s -o /dev/null -w '%{http_code}' "$URL/context?budget=100")"
[ "$CODE" = "401" ] || fail "tokenless request accepted ($CODE)"
pass "revocation effective on the next request; tokenless 401s"

# Uniqueness is scoped to live rows (tokens_live_name_idx), so a revoked name is
# immediately mintable again; the fresh plaintext authenticates while the old
# one stays dead.
CI_TOKEN2="$("$BIN" token create "$CI_NAME" | head -1)"
[ "$CI_TOKEN2" != "$CI_TOKEN" ] || fail "re-created token reused the old plaintext"
CODE="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $CI_TOKEN2" "$URL/context?budget=100")"
[ "$CODE" = "200" ] || fail "token re-created under a revoked name rejected ($CODE)"
CODE="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $CI_TOKEN" "$URL/context?budget=100")"
[ "$CODE" = "401" ] || fail "old plaintext authenticates after name reuse ($CODE)"
"$BIN" token revoke "$CI_NAME" >/dev/null
pass "revoked name reusable; new plaintext live, old plaintext stays dead"

# Prune deletes revoked tokens no audit row references. Both ci-revoke mints hit
# /context, which is audited, so they must survive the prune; a revoked token
# that never made a call must go.
PRUNE_NAME="ci-prune-$$-$RANDOM"
"$BIN" token create "$PRUNE_NAME" >/dev/null
"$BIN" token revoke "$PRUNE_NAME" >/dev/null
OUT="$("$BIN" token prune)"
echo "$OUT" | grep -q "$PRUNE_NAME" || fail "prune kept the unreferenced revoked token: $OUT"
"$BIN" token list | grep -q "$CI_NAME" || fail "prune deleted an audit-referenced token"
pass "prune deletes unreferenced revoked tokens, keeps audit-referenced ones"

# --- 3. context inject: marker-gated ----------------------------------------
UNMARKED="$SANDBOX/unmarked"; mkdir -p "$UNMARKED"
OUT="$(cd "$UNMARKED" && "$BIN" context inject)" || fail "unmarked inject exited nonzero"
[ -z "$OUT" ] || fail "unmarked inject produced output: $OUT"
MARKED="$UNMARKED/repo"; mkdir -p "$MARKED"; echo "platform-project" > "$MARKED/.cognosis-project"
OUT="$(cd "$MARKED" && "$BIN" context inject --budget 2000)" || fail "marked inject failed"
echo "$OUT" | grep -q "knowledge index" || fail "inject output is not an index: $OUT"
echo "$OUT" | grep -q "platform-project" || fail "inject not scoped to the marker project: $OUT"
SMALL="$(cd "$MARKED" && "$BIN" context inject --budget 10)"
# Bounds the output; does not prove truncation, since this vault is empty and has
# no index lines to drop. The ceiling clears the embedded SOP preamble (~3000
# chars, internal/mcpserver/sop.md), which is fixed overhead exempt from the
# budget -- the budget governs the index alone. Truncation itself is asserted in
# TestRenderContextPreamble, which can stage notes; keep the two in step.
[ "${#SMALL}" -le 3600 ] || fail "budget not respected (${#SMALL} chars for budget 10)"
pass "context inject: unmarked no-op; marked scoped + budget respected"

# --- 4. git commit capture (opt-in, marker-gated) ----------------------------
REPO="$SANDBOX/hooked-repo"; mkdir -p "$REPO"
git -C "$REPO" init -q
echo "platform-hook-project" > "$REPO/.cognosis-project"
echo "package main" > "$REPO/main.go"
git -C "$REPO" -c user.name=t -c user.email=t@t add -A
git -C "$REPO" -c user.name=t -c user.email=t@t commit -q -m "wire the frobnicator"
(cd "$REPO" && "$BIN" hook post-commit) | grep -q "captured" || fail "commit capture produced no entry"
grep -rq "wire the frobnicator" "$XDG_DATA_HOME/cognosis/kb/entries/" || fail "capture missing the commit subject"
pass "git commit captured into the vault (marker-gated, opt-in)"

# --- 5. stopped daemon => loud, fast failure in a marked repo ----------------
stop_daemon
START=$(date +%s)
set +e
(cd "$MARKED" && "$BIN" context inject >/dev/null 2>&1); RC=$?
set -e
ELAPSED=$(( $(date +%s) - START ))
[ "$RC" -ne 0 ] || fail "inject with daemon stopped exited 0 in a marked repo"
[ "$ELAPSED" -le 5 ] || fail "inject failure took ${ELAPSED}s (want ~2s)"
pass "marked repo + stopped daemon: loud, fast failure (${ELAPSED}s)"

# --- 6. shipped artifacts ----------------------------------------------------
for f in hooks/session-start-inject.sh hooks/session-end-nudge.sh hooks/settings.sample.json \
         hooks/post-commit.sh contrib/cognosis.service contrib/com.enqack.cognosis.plist; do
  [ -f "$ROOT/$f" ] || fail "missing $f"
done
[ -x "$ROOT/hooks/session-start-inject.sh" ] || fail "hook scripts must be executable"
pass "hooks + service files shipped"

echo
echo "platform check: all criteria pass"
