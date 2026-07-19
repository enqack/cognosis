#!/usr/bin/env bash
# Feature: built-in TLS -- the only door to a non-loopback bind. daemon.sh proves
# the refusal (no cert => no non-loopback bind); this proves the other half: with
# tls.cert_file/tls.key_file set the daemon really binds off loopback, really
# completes a TLS handshake, still refuses plaintext on that port, and still
# gates the encrypted door on a bearer token.
#
# Assertions run over GET /context rather than MCP: auth.Middleware wraps both,
# so the door is the same one, and memory-loop.sh already proves MCP itself.
set -euo pipefail
source "$(dirname "$0")/_lib.sh"

require_env ollama   # the daemon's boot sequence gates on the embedding provider
setup_sandbox
build_bin

CERTS="$SANDBOX/certs"
( cd "$ROOT" && go run ./scripts/checks/harness gen-cert "$CERTS" ) \
  || fail "could not generate the throwaway TLS chain"

export COGNOSIS_TLS_CERT_FILE="$CERTS/server.pem"
export COGNOSIS_TLS_KEY_FILE="$CERTS/server-key.pem"

# Bind every interface -- "non-loopback binds work when TLS is configured" is the
# claim under test -- but connect over loopback, which the leaf's SAN covers.
PORT=$(( (RANDOM % 2000) + 22000 ))
BASE="https://127.0.0.1:$PORT"

# --- 1. non-loopback bind accepted once TLS is configured --------------------
# The mirror of daemon.sh's refusal case. boot_daemon fails the check itself if
# the daemon never comes up, so reaching the next line is the assertion.
boot_daemon "0.0.0.0:$PORT"
pass "non-loopback bind accepted with tls.cert_file/tls.key_file set"

TOKEN="$(cat "$TOKEN_FILE")"

# --- 2. TLS handshake completes and auth passes ------------------------------
set +e
BODY="$(curl -sS --cacert "$CERTS/ca.pem" -H "Authorization: Bearer $TOKEN" \
          -w '\n%{http_code}' "$BASE/context" 2>&1)"; RC=$?
set -e
[ "$RC" -eq 0 ] || fail "TLS handshake or request failed: $BODY"
[ "$(echo "$BODY" | tail -1)" = "200" ] || fail "authenticated TLS request did not return 200: $BODY"
pass "TLS handshake completes against the CA and an authenticated request returns 200"

# --- 3. the cert is actually verified ----------------------------------------
# Without the CA, curl must reject it -- proves assertion 2 verified a chain
# rather than trusting anything the daemon presented.
set +e
OUT="$(curl -sS "$BASE/context" -H "Authorization: Bearer $TOKEN" 2>&1)"; RC=$?
set -e
[ "$RC" -ne 0 ] || fail "curl accepted the daemon's cert with no CA configured"
echo "$OUT" | grep -qi "certificate\|SSL\|TLS" || fail "rejection is not about the certificate: $OUT"
pass "an untrusted client rejects the self-signed chain"

# --- 4. plaintext is refused on the TLS port ---------------------------------
# Go's TLS listener answers a plaintext request with a plain 400 explaining the
# mistake rather than dropping the connection, so curl exits 0 here. The
# invariant is that /context is never *served* over plaintext -- not that the
# socket dies -- so assert on what came back, not on curl's exit code.
set +e
OUT="$(curl -sS --max-time 5 -w '\n%{http_code}' "http://127.0.0.1:$PORT/context" \
         -H "Authorization: Bearer $TOKEN" 2>&1)"; RC=$?
set -e
if [ "$RC" -eq 0 ] && [ "$(echo "$OUT" | tail -1)" = "200" ]; then
  fail "plaintext HTTP served /context on the TLS port: $OUT"
fi
pass "plaintext refused on the TLS port"

# --- 5. TLS does not replace auth --------------------------------------------
CODE="$(curl -sS --cacert "$CERTS/ca.pem" -o /dev/null -w '%{http_code}' "$BASE/context")"
[ "$CODE" = "401" ] || fail "tokenless request over TLS returned $CODE, want 401"
pass "bearer auth still gates the encrypted door"

echo
echo "tls check: all criteria pass"
