#!/usr/bin/env bash
# Shared boilerplate for the feature check scripts under scripts/checks/.
# Source it, then use: require_env [ollama], setup_sandbox, build_bin,
# boot_daemon [bind], stop_daemon, harness <slice>, pass, fail.
#
# Each script that drives MCP boots its own daemon in an mktemp sandbox against
# the dev Postgres (COGNOSIS_DSN) and a local Ollama.

# Repo root: scripts/checks/ -> ../../
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1" >&2; [ -n "${LOG:-}" ] && [ -f "$LOG" ] && tail -25 "$LOG" >&2; exit 1; }

# require_env [ollama] — COGNOSIS_DSN must be set; pass "ollama" to also require
# a reachable embedding server. Exit 2 (skip) when a prerequisite is missing;
# check-all.sh reports the skip and carries on to the next check.
#
# The ollama probe deliberately mirrors ollamaAvailable in
# internal/embed/embed_test.go — same COGNOSIS_TEST_OLLAMA override, same
# http://localhost:11434 default, same /api/version endpoint. The duplication is
# a language boundary (bash here, Go there), not an oversight: change one and the
# other needs the same change.
require_env() {
  if [ -z "${COGNOSIS_DSN:-}" ]; then
    echo "COGNOSIS_DSN must point at a reachable Postgres (run pg-start in the dev shell)" >&2
    exit 2
  fi
  if [ "${1:-}" = "ollama" ]; then
    if ! curl -sf --max-time 3 "${COGNOSIS_TEST_OLLAMA:-http://localhost:11434}/api/version" >/dev/null; then
      echo "Ollama is not reachable; start it with the embedding model pulled" >&2
      exit 2
    fi
  fi
}

# setup_sandbox — isolated XDG dirs under mktemp, cleaned up on exit.
setup_sandbox() {
  SANDBOX="$(mktemp -d)"
  trap 'stop_daemon; rm -rf "$SANDBOX"' EXIT
  export XDG_CONFIG_HOME="$SANDBOX/config"
  export XDG_DATA_HOME="$SANDBOX/data"
  export XDG_STATE_HOME="$SANDBOX/state"
  export XDG_CACHE_HOME="$SANDBOX/cache"
  LOCK="$XDG_STATE_HOME/cognosis/daemon.lock"
  LOG="$XDG_STATE_HOME/cognosis/daemon.log"
  TOKEN_FILE="$XDG_STATE_HOME/cognosis/local-token"
}

# build_bin — compile the daemon once into bin/cognosis; sets BIN.
build_bin() {
  BIN="$ROOT/bin/cognosis"
  ( cd "$ROOT" && go build -o bin/cognosis ./cmd/cognosis )
}

# boot_daemon [bind] — start the daemon and wait for the lock + local token.
# Default bind is a random loopback port; sets PORT/URL. An explicit bind sets
# PORT from it, so callers that need to reach the daemon on a different host than
# it binds (tls.sh binds 0.0.0.0 and connects over loopback) can still find it.
boot_daemon() {
  : "${BIN:?call build_bin first}"
  if [ -n "${1:-}" ]; then
    export COGNOSIS_BIND_ADDRESS="$1"
    PORT="${1##*:}"
  else
    PORT=$(( (RANDOM % 2000) + 20000 ))
    export COGNOSIS_BIND_ADDRESS="127.0.0.1:$PORT"
  fi
  URL="http://${COGNOSIS_BIND_ADDRESS}"
  "$BIN" start >/dev/null
  for _ in $(seq 1 100); do [ -f "$LOCK" ] && break; sleep 0.1; done
  [ -f "$LOCK" ] || fail "daemon did not come up (no lock file)"
  for _ in $(seq 1 100); do [ -f "$TOKEN_FILE" ] && break; sleep 0.1; done
  [ -f "$TOKEN_FILE" ] || fail "daemon did not mint the zero-config local token"
}

stop_daemon() { [ -n "${BIN:-}" ] && "$BIN" stop >/dev/null 2>&1 || true; }

# harness <slice> — run one slice of the shared Go MCP harness against the
# running daemon (COGNOSIS_MCP_URL/TOKEN_FILE/DSN threaded through).
#
# The harness returns 0 or 1 and never 2: 2 is spoken for here (require_env's
# skip), and a harness usage error is a failure, not a missing prerequisite.
# Keep guarding these calls with `|| fail` so a harness exit can never become
# the script's own exit code by accident.
harness() {
  ( cd "$ROOT" && COGNOSIS_MCP_URL="$URL" COGNOSIS_TOKEN_FILE="$TOKEN_FILE" COGNOSIS_DSN="$COGNOSIS_DSN" \
      go run ./scripts/checks/harness "$1" )
}
