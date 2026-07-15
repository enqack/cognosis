# Cognosis — Setup Guide

Full setup for every part of a working Cognosis install: the `cognosis` daemon, its Postgres index, the
Ollama embedding server, the markdown vault, and an MCP client (Claude Code). Two paths are covered —
the Nix dev shell (fastest) and a manual/production install — followed by configuration, session hooks,
running as a service, verification, and troubleshooting.

## What you're setting up

| Part | Role |
|---|---|
| `cognosis` daemon | Owns the vault, serves MCP over Streamable HTTP, runs the watcher/lifecycle/migration. |
| Postgres 16 + pgvector | The **derived, droppable** index (chunks, embeddings, link graph). Rebuildable from the vault. |
| Ollama + `nomic-embed-text:v1.5` | Local embedding provider (768-dim vectors). |
| Markdown vault | The **single source of truth** at `$XDG_DATA_HOME/cognosis/kb/`. |
| MCP client | Claude Code (or any MCP client) talking to the daemon with a bearer token. |

The daemon fails fast: if Postgres or the embedding provider is unreachable at startup, it exits nonzero
rather than running degraded.

---

## Path A — Nix dev shell (recommended for development)

The flake provides the whole toolchain: Go, Postgres + pgvector, Ollama, mage, golangci-lint, xz.
(The authoritative Go version pin lives in `go.mod` via its `toolchain` directive, not in the flake.)

```sh
cd cognosis
nix develop                       # enter the dev shell (tools on PATH)

pg-start                          # dev Postgres on :5434; exports COGNOSIS_DSN
ollama serve &                    # or reuse an Ollama already on :11434
ollama pull nomic-embed-text:v1.5 # one-time model pull

mage build                        # -> bin/cognosis (version-stamped from VERSION)
./bin/cognosis start              # daemonizes; mints a local token on first start
./bin/cognosis status             # postgres / embedding / schema / mcp / daemon health
```

`pg-start` runs a unix-socket-only Postgres in an in-repo, gitignored `.pg-data/` on port 5434 and
exports `COGNOSIS_DSN`, so the daemon finds it with no config. `pg-stop` stops it.

---

## Path B — Manual / production install

### 1. Build or install the binary
With Go 1.25+:

```sh
go install github.com/enqack/cognosis/cmd/cognosis@latest   # or: mage install
```

`go.mod` pins `toolchain go1.26.5` (a stdlib security fix), so building on an older Go 1.25
toolchain will transparently fetch and use Go 1.26.5 — this is expected, not an error.

Or download a release archive (`cognosis-<version>-<os>-<arch>.tar.gz`) and place `cognosis` on `PATH`.

### 2. Postgres 16 with pgvector
Provision a Postgres 16 server where the `vector` extension is **available** (the Debian/Ubuntu
`postgresql-16-pgvector` package, the `pgvector/pgvector` image, or `make install` from source), then
create a database:

```sh
createdb cognosis
```

You do **not** need to run `CREATE EXTENSION` yourself — the daemon's migrations do it on first start
(they run `create extension if not exists vector`). The connecting role must be allowed to create
extensions (superuser, or a role granted `CREATE` on the DB with pgvector trusted).

### 3. Ollama + the embedding model

```sh
# install Ollama (see ollama.com), then:
ollama serve
ollama pull nomic-embed-text:v1.5
```

A remote/other OpenAI-compatible embedding endpoint can be used instead via config (`embedding.url`),
but Ollama with the pinned model is the default and what the checks exercise.

### 4. Point Cognosis at Postgres
Set the DSN (env or config — see [Configuration](#configuration)):

```sh
export COGNOSIS_DSN="postgres://user:pass@localhost:5432/cognosis"
cognosis start
cognosis status
```

---

## Configuration

Config lives at `$XDG_CONFIG_HOME/cognosis/config.yaml` (created on first `cognosis config set`).
Any key can be overridden by a `COGNOSIS_*` env var (`.` → `_`, e.g. `COGNOSIS_DSN`,
`COGNOSIS_BIND_ADDRESS`, `COGNOSIS_EMBEDDING_MODEL`). Precedence is **env > file > default**.

```yaml
# ~/.config/cognosis/config.yaml
dsn: "postgres://user:pass@localhost:5432/cognosis"   # empty = self-locate a dev .pg-data
bind_address: "127.0.0.1:7433"
kb_path: "~/.local/share/cognosis/kb"                 # defaults under XDG_DATA_HOME
reconcile_sweep_interval: "1h"
embedding:
  provider: "ollama"
  model: "nomic-embed-text:v1.5"
  url: "http://localhost:11434"
tls:
  cert_file: ""     # set both to allow a non-loopback bind (see docs/remote.md)
  key_file: ""
personas:
  - id: "deep-thoughts"
```

```sh
cognosis config set dsn "postgres://…"     # persists to config.yaml
cognosis config get bind_address           # env > file > default
```

The full key reference is in [configuration.md](configuration.md).

### XDG paths

| Path | Contents |
|---|---|
| `$XDG_CONFIG_HOME/cognosis/config.yaml` | the one hand-edited file |
| `$XDG_DATA_HOME/cognosis/kb/` | the markdown vault (source of truth) + its git history repo |
| `$XDG_DATA_HOME/cognosis/personas/` | operator-added persona files |
| `$XDG_STATE_HOME/cognosis/local-token` | zero-config local bearer token (mode 0600) |
| `$XDG_STATE_HOME/cognosis/daemon.lock` | local single-instance PID lock |

---

## First start & health

On first `cognosis start` the daemon mints a **local bearer token** at
`$XDG_STATE_HOME/cognosis/local-token` (0600). `cognosis status` reports each dependency:

```
postgres    ok  ...
embedding   ok  ollama nomic-embed-text:v1.5
schema      ok  version 1
mcp         ok  listening on 127.0.0.1:7433
daemon      ok  pid 12345
```

Any `postgres`/`embedding`/`schema` failure means the daemon refused to serve — fix the dependency and
restart.

---

## Register with Claude Code

```sh
claude mcp add --transport http cognosis http://127.0.0.1:7433 \
  --header "Authorization: Bearer $(cat ~/.local/state/cognosis/local-token)"
```

That exposes the MCP tool surface (`write_note`, `query_knowledge`, … — see [cli.md](cli.md)).

## Session hooks (optional, per repo)

Automatic session context uses Claude Code hooks, wired in `.claude/settings.json`. Copy
`hooks/settings.sample.json` and point the commands at your checkout's `hooks/` scripts:

- **SessionStart** → `hooks/session-start-inject.sh` runs `cognosis context inject` and injects a
  project-scoped knowledge index. Budget via `COGNOSIS_INJECT_BUDGET` (default 2000).
- **SessionEnd** → `hooks/session-end-nudge.sh` resumes the ending session (via the `session_id` on the
  hook's stdin) for one headless `claude` turn nudging the agent to persist anything durable — resuming
  so the turn sees what happened, and guarding against re-entry so the nested run can't loop (needs the
  `claude` CLI logged in; exits quietly if not).

Both are **marker-gated**: they no-op unless a `.cognosis-project` file exists at (or above) the repo
root, so a stopped daemon never blocks unrelated sessions. Inside a marked repo a stopped daemon makes
SessionStart fail loudly (~2s) rather than start a context-less session.

```sh
echo "my-project" > .cognosis-project   # tag a repo; the value is the project name
```

## Git commit capture (optional, opt-in per repo)

Land commits as structured vault entries — install the hook only where wanted:

```sh
cp /path/to/cognosis/hooks/post-commit.sh .git/hooks/post-commit
chmod +x .git/hooks/post-commit
```

Marker-gated and never fails the commit: in a repo without `.cognosis-project` it exits 0 silently.

## Run as a service (persistent daemon)

Both files invoke the self-managed CLI in foreground mode; the service manager only supervises it.

**Linux (systemd user unit):**
```sh
cp contrib/cognosis.service ~/.config/systemd/user/
systemctl --user enable --now cognosis
```

**macOS (launchd):**
```sh
cp contrib/com.enqack.cognosis.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.enqack.cognosis.plist
```

Adjust the binary path in the unit/plist to wherever `cognosis` is installed.

## Remote access

Keep the daemon on loopback behind a TLS-terminating reverse proxy (recommended), or configure built-in
TLS (`tls.cert_file`/`tls.key_file`) — the only way a non-loopback bind is permitted. Full guidance,
including per-client tokens and threat notes, is in [remote.md](remote.md).

---

## Verify

With the daemon up and registered, from your MCP client:

1. `write_note` a note under `entries/…` with valid frontmatter.
2. `query_knowledge` for its text — it comes back hybrid-ranked with a score.
3. `get_note` returns the exact content.

Or run the full end-to-end feature checks (needs `COGNOSIS_DSN` + a local Ollama):

```sh
mage check          # scripts/check-all.sh — daemon, memory-loop, retrieval,
                    # knowledge, platform, embedding-migration
```

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `startup: postgres: … unreachable` | Postgres not running or wrong DSN. Start it (`pg-start` in dev) and check `COGNOSIS_DSN` / `dsn`. |
| `startup: embedding provider: …` | Ollama not running. `ollama serve`, and confirm `embedding.url`. |
| Writes/queries error about the model | Model not pulled: `ollama pull nomic-embed-text:v1.5`. |
| `bind_address … is not loopback` | A non-loopback `bind_address` needs TLS. Set `tls.cert_file`/`tls.key_file`, or bind loopback behind a proxy (see [remote.md](remote.md)). |
| `another cognosis daemon …` / lock refusal | The single-instance invariant: only one daemon per database. Stop the other instance (even on another machine — the Postgres advisory lock is the arbiter). |
| `no migration found for version N` after a manual DB reset | The derived index is rebuildable, not migrated across a schema renumber. Recreate it: drop the schema (`drop schema public cascade; create schema public;`) or the database, then restart — boot reconciliation re-indexes from the vault. |
| SessionStart hook does nothing | No `.cognosis-project` marker at/above the repo root (by design). Add one to opt the repo in. |
| `401` from an MCP call | Missing/typo'd bearer token, or the token was revoked. Re-read `local-token`, or mint one with `cognosis token create <name>`. |
