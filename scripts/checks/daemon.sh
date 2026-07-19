#!/usr/bin/env bash
# Feature: daemon process invariants — startup gates, single-instance lock
# (local + cross-machine), clean shutdown, and boot reconciliation of
# out-of-band edits into the vault history.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

require_env            # Postgres only; this feature does not touch Ollama
setup_sandbox
build_bin
KB="$XDG_DATA_HOME/cognosis/kb"

# --- 1. Postgres down => fatal, names Postgres + the DSN target --------------
set +e
OUT="$(COGNOSIS_DSN="postgres://127.0.0.1:1/nope" "$BIN" start --foreground 2>&1)"; RC=$?
set -e
[ "$RC" -ne 0 ] || fail "start succeeded with unreachable Postgres"
echo "$OUT" | grep -qi "postgres" || fail "error does not name Postgres: $OUT"
echo "$OUT" | grep -q "127.0.0.1:1" || fail "error does not name the DSN target: $OUT"
pass "unreachable Postgres is fatal and named"

# --- 2. non-loopback bind refused --------------------------------------------
set +e
OUT="$(COGNOSIS_BIND_ADDRESS="0.0.0.0:29999" "$BIN" start --foreground 2>&1)"; RC=$?
set -e
[ "$RC" -ne 0 ] || fail "daemon started with a non-loopback bind"
echo "$OUT" | grep -qi "loopback" || fail "refusal does not explain the loopback rule: $OUT"
pass "non-loopback bind refused"

# --- 3. daemon up; second start refused (local PID lock) ---------------------
boot_daemon
PID="$(cat "$LOCK")"
kill -0 "$PID" || fail "lock pid $PID not alive"
# status is captured once and asserted line by line, naming each check rather
# than asserting "no line says FAIL".
#
# The generic form is tempting because `status` already exits nonzero when any
# check fails — but this script deliberately declares only `require_env` (no
# ollama), so the embedding line legitimately reports FAIL here whenever no
# embedding server is running, and a blanket assertion would silently couple
# daemon.sh to a prerequisite it does not require. Naming them also makes a
# regression say *which* invariant broke. Hence the capture: status's own exit
# code cannot be the assertion under those declared prerequisites.
set +e
STATUS="$("$BIN" status 2>&1)"
set -e
echo "$STATUS" | grep -qE "^daemon +ok" || fail "status does not report a healthy daemon:
$STATUS"
# auth: the stashed local token still resolves to a live database row. A token
# file that outlives its row 401s every client while every other line stays
# green, so nothing above this can see it.
echo "$STATUS" | grep -qE "^auth +ok" || fail "status does not report healthy auth:
$STATUS"
# graph: the stored link edges still agree with the indexed note content. Notes,
# chunks and embeddings can all be correct while an inbound edge has silently
# gone missing.
echo "$STATUS" | grep -qE "^graph +ok" || fail "status does not report a healthy link graph:
$STATUS"
set +e
OUT2="$("$BIN" start 2>&1)"; RC2=$?
set -e
[ "$RC2" -ne 0 ] || fail "second start was not refused"
echo "$OUT2" | grep -q "already\|running" || fail "refusal does not explain itself: $OUT2"
pass "daemon up; second start refused via the local lock"
pass "status reports daemon, auth and link-graph health"

# --- 4. cross-machine guard: a second daemon with a *different* local state
# dir (so the PID lockfile can't catch it) is refused by the Postgres advisory
# instance lock. ------------------------------------------------------------
set +e
OUT2B="$(XDG_STATE_HOME="$SANDBOX/alt-state" XDG_DATA_HOME="$SANDBOX/alt-data" \
  XDG_CONFIG_HOME="$SANDBOX/alt-config" "$BIN" start --foreground 2>&1)"; RC2B=$?
set -e
[ "$RC2B" -ne 0 ] || fail "second daemon with a separate state dir was not refused"
echo "$OUT2B" | grep -qi "already owns this database\|another cognosis daemon" \
  || fail "cross-machine refusal does not name the database instance lock: $OUT2B"
pass "second daemon on a separate filesystem refused by the Postgres instance lock"

# --- 5. SIGTERM => clean shutdown, lock released -----------------------------
kill -TERM "$PID"
for _ in $(seq 1 100); do kill -0 "$PID" 2>/dev/null || break; sleep 0.1; done
kill -0 "$PID" 2>/dev/null && fail "daemon still alive after SIGTERM"
[ ! -f "$LOCK" ] || fail "lock file not released on shutdown"
pass "SIGTERM shuts down cleanly and releases the lock"

# --- 6. hand-edit while stopped => reconciled + committed to history on boot --
mkdir -p "$KB/entries"
cat > "$KB/entries/handedit.md" <<EOF
---
id: $(printf '0192%04x-0000-7000-8000-%012x' $((RANDOM)) $((RANDOM)))
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Edited while the daemon was stopped.
EOF
# A second note linking to the first, so the graph audit asserted below has an
# actual edge to agree or disagree about. At step 3 the sandbox vault is empty
# and `graph ok` there means "0 edges across 0 notes" — true, but vacuous, and a
# vacuously-true assertion is the failure mode this whole pass is guarding
# against.
cat > "$KB/entries/handedit-src.md" <<EOF
---
id: $(printf '0192%04x-0001-7000-8000-%012x' $((RANDOM)) $((RANDOM)))
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Written out of band, citing [[handedit]] so the link graph has an edge.
EOF
boot_daemon
for _ in $(seq 1 100); do grep -q "note indexed.*handedit\.md" "$LOG" && break; sleep 0.1; done
grep -q "note indexed.*handedit\.md" "$LOG" || fail "hand-edited note was not reconciled on boot"
# The history commit lands *after* the "note indexed" line, so it needs its own
# wait. Asserting it immediately was a latent race — every other step in this
# script already polls; this one was the exception, and it fires whenever
# anything slows the path between indexing and committing. It happened to win
# while connection setup was cheap, and started losing consistently once
# store.Connect gained a per-connection SET.
for _ in $(seq 1 100); do
  git -C "$KB" log --oneline -- entries/handedit.md | grep -q . && break
  sleep 0.1
done
git -C "$KB" log --oneline -- entries/handedit.md | grep -q . \
  || fail "drift not committed to the vault history repo"
pass "hand-edit reconciled on boot and committed to vault history"

# The graph audit re-asserted over a vault that actually has an edge — an
# out-of-band write is precisely the route by which stored edges drift out of
# agreement with note content, so this is where the check earns its keep.
for _ in $(seq 1 100); do grep -q "note indexed.*handedit-src" "$LOG" && break; sleep 0.1; done
set +e
STATUS2="$("$BIN" status 2>&1)"
set -e
echo "$STATUS2" | grep -qE "^graph +ok" || fail "link graph disagrees with note content after reconciliation:
$STATUS2"
echo "$STATUS2" | grep -qE "^graph +ok +0 edges" && fail "graph check saw no edges, so it proved nothing:
$STATUS2"
pass "link-graph audit agrees with note content after an out-of-band write"

echo
echo "daemon check: all criteria pass"
