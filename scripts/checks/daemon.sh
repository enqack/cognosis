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

# --- 7. `vault restore` routes on who owns the *database*, not the PID file ---
#
# The daemon and the CLI write the same files, and only the daemon's door
# (restore_note) normalises the path and takes the per-path lock. So the routing
# decision is a correctness boundary, and all three branches are asserted here
# against a real daemon: routed, refused, and forced.
#
# Setup: handedit.md already has one committed version (step 6). Give it a
# second, out of band, so a restore to the first ref is observable as the
# original text coming back — and so a restore that silently does nothing cannot
# read as success.
ORIG_REF="$(git -C "$KB" log --format=%H -- entries/handedit.md | tail -1)"
[ -n "$ORIG_REF" ] || fail "no committed version of handedit.md to restore to"
NOTE="$KB/entries/handedit.md"
sed_v2() { perl -pi -e 's/Edited while the daemon was stopped\./SECOND VERSION./' "$NOTE"; }
sed_v2
for _ in $(seq 1 100); do
  [ "$(git -C "$KB" log --oneline -- entries/handedit.md | wc -l | tr -d ' ')" -ge 2 ] && break
  sleep 0.1
done
grep -q "SECOND VERSION" "$NOTE" || fail "fixture setup: second version not on disk"

# 7a. daemon owns the database but its MCP door does not answer => refuse.
# Deliberately not a fallback: a direct write is the race precisely when the
# thing that owns the data is already misbehaving. The refusal has to name
# --force-local or the operator has no way forward.
set +e
OUT7="$(COGNOSIS_BIND_ADDRESS="127.0.0.1:1" "$BIN" vault restore entries/handedit.md --at "$ORIG_REF" 2>&1)"; RC7=$?
set -e
[ "$RC7" -ne 0 ] || fail "restore succeeded while the owning daemon was unreachable: $OUT7"
echo "$OUT7" | grep -q -- "--force-local" || fail "refusal does not name the escape hatch: $OUT7"
# The assertion that matters: it refused *and wrote nothing*. Asserting only the
# exit code would pass against a version that fell back to a direct write and
# then reported the transport error anyway.
grep -q "SECOND VERSION" "$NOTE" || fail "the refused restore wrote the file anyway"
pass "restore refuses when a daemon owns the database and cannot be reached"

# 7b. --force-local is that way forward: it must actually write while the daemon
# is up, and must say on stderr that it bypassed the per-path lock. A flag that
# warns without writing, or writes without warning, is worse than no flag.
set +e
ERR7B="$("$BIN" vault restore entries/handedit.md --at "$ORIG_REF" --force-local 2>&1 >/dev/null)"; RC7B=$?
set -e
[ "$RC7B" -eq 0 ] || fail "--force-local refused to write under a live daemon: $ERR7B"
echo "$ERR7B" | grep -qi "force-local" || fail "--force-local did not warn on stderr: $ERR7B"
echo "$ERR7B" | grep -qi "lock" || fail "the warning does not say what was bypassed: $ERR7B"
grep -q "Edited while the daemon was stopped" "$NOTE" || fail "--force-local did not restore the file"
pass "--force-local writes directly under a live daemon and warns that it did"

# 7c. the ordinary path: a daemon owns the database and answers, so the restore
# is routed through restore_note rather than written here. "via the running
# daemon" on stdout is the routing evidence — the direct path prints a different
# line — and the audit row is the daemon's own record that it did the work.
sed_v2
for _ in $(seq 1 100); do grep -q "SECOND VERSION" "$NOTE" && break; sleep 0.1; done
set +e
OUT7C="$("$BIN" vault restore entries/handedit.md --at "$ORIG_REF" 2>&1)"; RC7C=$?
set -e
[ "$RC7C" -eq 0 ] || fail "routed restore failed: $OUT7C"
echo "$OUT7C" | grep -q "via the running daemon" \
  || fail "restore did not report routing through the daemon, so it took the direct path: $OUT7C"
grep -q "Edited while the daemon was stopped" "$NOTE" || fail "routed restore did not land the old version"
AUDIT="$(psql "$BASE_DSN" -At -c \
  "select count(*) from \"$CHECK_SCHEMA\".audit_log where tool_name = 'restore_note' and success" 2>/dev/null || echo 0)"
[ "${AUDIT:-0}" -ge 1 ] || fail "no restore_note audit row: the daemon never ran the restore, the CLI did"
pass "restore routes through the daemon's restore_note when a daemon owns the database"

# --- 8. hard delete is refused while a daemon owns the database --------------
#
# It rewrites git history, drops rows and removes a file, none of it under the
# per-path lock, and there is no MCP tool to route it through — so refusing is
# the available correctness.
set +e
OUT8="$("$BIN" note delete entries/handedit.md --hard --yes 2>&1)"; RC8=$?
set -e
[ "$RC8" -ne 0 ] || fail "hard delete succeeded underneath a live daemon: $OUT8"
echo "$OUT8" | grep -qi "daemon" || fail "refusal does not name the daemon as the reason: $OUT8"
[ -f "$NOTE" ] || fail "the refused hard delete removed the file anyway"
pass "hard delete refused while a daemon owns the database"

# --- 9. ...and permitted once it does not -----------------------------------
#
# The half that would rot silently. A guard asserted only in its refusing
# direction passes just as well when it has become an unconditional wall, and
# the failure mode there — erasure impossible even with the daemon stopped — is
# invisible until someone actually needs it.
stop_daemon
for _ in $(seq 1 100); do [ ! -f "$LOCK" ] && break; sleep 0.1; done
[ -f "$LOCK" ] && fail "daemon did not release the lock before the permitted-delete case"
set +e
OUT9="$("$BIN" note delete entries/handedit.md --hard --yes 2>&1)"; RC9=$?
set -e
[ "$RC9" -eq 0 ] || fail "hard delete refused with the daemon stopped: $OUT9"
[ ! -f "$NOTE" ] || fail "hard delete reported success but the file is still there"
# Captured rather than piped into `grep -q`. Under `set -o pipefail`, grep -q
# exits on the first match and SIGPIPEs git, so the pipeline reports 141 and a
# trailing `&& fail` never runs — the assertion passes precisely when history
# still holds the note, which is the bug it exists to catch. Verified by
# removing the PurgePath call: the piped form stayed green, this one fails.
HIST9="$(git -C "$KB" log --oneline -- entries/handedit.md)"
[ -z "$HIST9" ] || fail "hard delete left the note in vault history, which is the one thing it promises to erase:
$HIST9"
pass "hard delete permitted and complete once no daemon owns the database"

echo
echo "daemon check: all criteria pass"
