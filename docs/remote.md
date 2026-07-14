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
        proxy_buffering off;      # Streamable HTTP uses long-lived responses
        proxy_read_timeout 3600s;
    }
}
```

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
under the resolved token identity with redacted argument summaries — never note content.

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
