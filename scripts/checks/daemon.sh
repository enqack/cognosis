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
"$BIN" status | grep -q "daemon.*ok" || fail "status does not report a healthy daemon"
set +e
OUT2="$("$BIN" start 2>&1)"; RC2=$?
set -e
[ "$RC2" -ne 0 ] || fail "second start was not refused"
echo "$OUT2" | grep -q "already\|running" || fail "refusal does not explain itself: $OUT2"
pass "daemon up; second start refused via the local lock"

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
id: $(uuidgen | tr 'A-Z' 'a-z')
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Edited while the daemon was stopped.
EOF
boot_daemon
for _ in $(seq 1 100); do grep -q "note indexed.*handedit" "$LOG" && break; sleep 0.1; done
grep -q "note indexed.*handedit" "$LOG" || fail "hand-edited note was not reconciled on boot"
# The history commit lands *after* the "note indexed" line, so it needs its own
# wait. Asserting it immediately was a latent race: it happened to win while
# connection setup was cheap, and started losing consistently when store.Connect
# gained a per-connection SET. Every other step here already polls; this one
# was the exception.
for _ in $(seq 1 100); do
  git -C "$KB" log --oneline -- entries/handedit.md | grep -q . && break
  sleep 0.1
done
git -C "$KB" log --oneline -- entries/handedit.md | grep -q . \
  || fail "drift not committed to the vault history repo"
pass "hand-edit reconciled on boot and committed to vault history"

echo
echo "daemon check: all criteria pass"
