# Cognosis — Configuration Reference

All settings flow through one config file plus environment overrides. Nothing else in Cognosis reads an
env var or resolves a path directly.

## Where config lives

`$XDG_CONFIG_HOME/cognosis/config.yaml` (on Linux, `~/.config/cognosis/config.yaml`; native equivalents
on macOS/Windows via the XDG resolver). The file is optional — a missing config on first run is normal,
and every key has a default. It's created on the first `cognosis config set`.

## Precedence

**environment variable > config.yaml > built-in default.**

Env vars are the config key uppercased with `COGNOSIS_` prefix and `.` → `_`. Examples:
`COGNOSIS_DSN`, `COGNOSIS_BIND_ADDRESS`, `COGNOSIS_KB_PATH`, `COGNOSIS_EMBEDDING_MODEL`,
`COGNOSIS_EMBEDDING_URL`, `COGNOSIS_TLS_CERT_FILE`.

## Keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `dsn` | string | `""` | Postgres connection string. Empty = self-locate a dev Postgres by walking up for a `.pg-data` dir (the `pg-start` layout, port 5434). Set it explicitly for production. |
| `bind_address` | string | `127.0.0.1:7433` | MCP server listen address. A non-loopback value is refused unless TLS is configured. |
| `kb_path` | string | `$XDG_DATA_HOME/cognosis/kb` | The markdown vault directory (source of truth). |
| `reconcile_sweep_interval` | duration | `1h` | Period of the full BLAKE3 drift sweep that backstops the live watcher (Go duration string, e.g. `30m`, `2h`). |
| `embedding.provider` | string | `ollama` | Embedding provider name. |
| `embedding.model` | string | `nomic-embed-text:v1.5` | Embedding model. Changing it (or its tag) requires a re-embed migration — old and new vectors aren't comparable. |
| `embedding.url` | string | `http://localhost:11434` | Embedding server base URL. |
| `tls.cert_file` | string | `""` | PEM cert path. Set with `key_file` to enable built-in TLS. |
| `tls.key_file` | string | `""` | PEM key path. Both TLS files set = a non-loopback `bind_address` becomes legal. |
| `personas` | list | `[{ id: deep-thoughts }]` | Enabled-persona registry (see below). |

### `personas`

A list of lightweight entries kept alongside — not instead of — the persona files in
`$XDG_DATA_HOME/cognosis/personas/`. Each entry:

```yaml
personas:
  - id: "deep-thoughts"      # required; matches a persona file
    name: "Deep Thoughts"    # optional display name
    description: "…"          # optional one-liner (surfaced by list_personas)
```

Only `id` is required; a persona with an empty `id` is ignored. Disabling a persona is removing its
entry — the file stays for later reactivation.

### TLS / non-loopback binds

By default the server binds loopback only. To bind a non-loopback address you must set **both**
`tls.cert_file` and `tls.key_file` (built-in TLS). The recommended remote posture instead keeps
Cognosis on loopback behind a TLS-terminating reverse proxy — see [remote.md](remote.md).

## Managing config from the CLI

```sh
cognosis config get <key>          # effective value (env > file > default)
cognosis config set <key> <value>  # persist to config.yaml (creates it if absent)
```

`config set` writes only the given key; env-var overrides still win at runtime.

## Filesystem paths (XDG)

Resolved via the XDG Base Directory spec (env var first, platform default second):

| Path | Contents |
|---|---|
| `$XDG_CONFIG_HOME/cognosis/config.yaml` | this file |
| `$XDG_DATA_HOME/cognosis/kb/` | the vault (markdown source of truth) + auto-managed git history |
| `$XDG_DATA_HOME/cognosis/personas/` | operator-added persona files |
| `$XDG_STATE_HOME/cognosis/local-token` | zero-config local bearer token (mode 0600) |
| `$XDG_STATE_HOME/cognosis/daemon.lock` | local single-instance PID lock |
| `$XDG_CACHE_HOME/cognosis/` | reserved (unused in v1) |

All directories are created on first daemon start.
