# Cognosis — Remote Access

Cognosis defaults to local-only: the daemon binds loopback and mints a local bearer token on first
start. Remote, multi-client access is opt-in, and there are exactly two supported shapes — bearer
tokens travel only over TLS, never plaintext on a network. See [setup-guide.md](setup-guide.md) for the
rest of a working install and [configuration.md](configuration.md) for the `bind_address`/`tls` keys.

## Recommended: reverse proxy terminates TLS (Cognosis stays on loopback)

Cognosis keeps its default `bind_address: 127.0.0.1:7433`; the proxy owns certificates and forwards to
loopback. No Cognosis config changes at all.

Caddy:

```caddyfile
cognosis.example.com {
    reverse_proxy 127.0.0.1:7433
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name cognosis.example.com;
    ssl_certificate     /etc/letsencrypt/live/cognosis.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/cognosis.example.com/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:7433;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;      # Streamable HTTP uses long-lived responses
        proxy_read_timeout 3600s;
    }
}
```

Both snippets forward from loopback, so **every remote caller reaches the daemon with
`RemoteAddr` of `127.0.0.1`** — indistinguishable from the local CLI by network position alone.
Two consequences:

- Leave `trust_local_errors` at its default (`false`). It releases the full cause of internal
  failures — DSNs, socket paths, database users — to callers the daemon judges local, and behind a
  proxy that judgement is wrong. The setting is an operator assertion that *nothing* proxies this
  daemon.
- Keep the `X-Forwarded-For` line above. Cognosis checks each call for a forwarding marker and
  withholds detail when it finds one, so the header is a second line of defence for the case where
  `trust_local_errors` was set by mistake. Caddy's `reverse_proxy` sets it automatically.

The check is per call rather than per daemon precisely because of this topology: the bind really is
loopback and the operator may really have opted in, so the marker on the individual request is the
only thing left that distinguishes a forwarded caller from the local CLI.

## Fallback: built-in TLS (no proxy)

Set both keys in `config.yaml`; a non-loopback `bind_address` is refused unless they are set:

```yaml
bind_address: 0.0.0.0:7433
tls:
  cert_file: /etc/cognosis/tls/cert.pem
  key_file: /etc/cognosis/tls/key.pem
```

## Tokens for remote clients

Mint one token per client — never share the local auto-token (full command detail in [cli.md](cli.md)):

```sh
cognosis token create laptop-alice     # printed once; store it client-side
cognosis token revoke laptop-alice     # effective on the very next request
cognosis token list
```

Each client sends `Authorization: Bearer <token>`. Every tool call is audit-logged (`audit_log` table)
under the resolved token identity with redacted argument summaries — never note content. MCP-originated
log lines also carry `token=<name>`, so per-leg retrieval counters can be attributed per client; a
missing `token=` marks daemon-internal work rather than a gap.

Keep each token out of the client's config file — see
[contrib/cognosis-mcp-headers](../contrib/cognosis-mcp-headers) and
[setup-guide.md](setup-guide.md#keeping-the-token-out-of-client-config). Interpolating it with
`$(cat …)` leaves a copy nothing rotates, which is the usual cause of a `401` after re-minting.

**Mint one token per client even locally.** A shared token makes every caller indistinguishable in both
the audit table and the log, which silently ruins any telemetry drawn from them — an agent debugging
retrieval writes traffic that looks exactly like ordinary use.

Leave the auto-minted `local` token to the daemon. `cognosis token create local` is rejected —
`local` is reserved, so an operator cannot take the name the daemon mints under. Rotating it is a
three-step sequence documented in [setup-guide.md](setup-guide.md#rotating-the-local-token); revoking
it without removing the state-dir file stops the daemon booting.

Rotating any other token is just `revoke <name>` then `create <name>`: names are unique among live
tokens only, so the name comes back. Revoked rows accumulate for the audit trail —
`cognosis token prune` clears the ones nothing references and keeps the rest by design.

## Threat notes

- The bearer token is the entire authorization; TLS is what keeps it secret in transit. Rotate by
  create-new + revoke-old.
- Revocation is checked synchronously on every request: there is no revoked-token grace window, by
  design.
- The `/context` endpoint and all MCP tools sit behind the same middleware; there is no unauthenticated
  surface.
- Remote access means many clients, one daemon. A second daemon pointed at the same Postgres is refused
  by the single-instance lock (a session advisory lock is the cross-machine arbiter) — see
  [architecture.md](architecture.md).
