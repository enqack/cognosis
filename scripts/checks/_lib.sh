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
# a reachable embedding server. Exit 2 (skip) when a prerequisite is missing.
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
# Default bind is a random loopback port; sets PORT/URL.
boot_daemon() {
  : "${BIN:?call build_bin first}"
  PORT=$(( (RANDOM % 2000) + 20000 ))
  export COGNOSIS_BIND_ADDRESS="${1:-127.0.0.1:$PORT}"
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
harness() {
  ( cd "$ROOT" && COGNOSIS_MCP_URL="$URL" COGNOSIS_TOKEN_FILE="$TOKEN_FILE" COGNOSIS_DSN="$COGNOSIS_DSN" \
      go run ./scripts/checks/harness "$1" )
}
