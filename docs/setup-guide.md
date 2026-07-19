# Cognosis — Setup Guide

Full setup for every part of a working Cognosis install: the `cognosis` daemon, its Postgres index, the
Ollama embedding server, the markdown vault, and an MCP client (Claude Code). Three paths are covered —
the Nix dev shell (fastest), a manual/production install, and a Nix flake install — followed by
configuration, session hooks, running as a service, verification, and troubleshooting.

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
./bin/cognosis status             # postgres / embedding / schema / mcp / auth / graph / daemon health
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

**pgvector 0.8 or newer is strongly recommended.** The daemon sets `hnsw.iterative_scan` on each
connection, which older versions do not have. Without it the semantic leg under-returns on any
project-scoped or archive-filtered query — measurably so: a scope holding a quarter of the corpus
retrieved 20% of the correct results. The daemon still starts on an older pgvector, logging a
warning per connection; retrieval degrades rather than failing. No `shared_preload_libraries` entry
or other server-level configuration is needed — these are ordinary session settings.

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

## Path C — Nix flake install

The flake also exposes a `cognosis` package/app and service modules, as an alternative to Path B for
Nix users. It does **not** provision Postgres+pgvector or Ollama — point the module's `environment` at
whatever already makes those reachable (your own `services.postgresql`/`services.ollama`, or a remote
endpoint), the same way `dsn`/`embedding.url` work in [Configuration](#configuration).

**Just the binary**, no service management:

```sh
nix run github:enqack/cognosis -- start --foreground
nix profile install github:enqack/cognosis            # onto PATH, no service
```

**As a managed service**, import the module matching your setup into your own flake and set
`services.cognosis`:

```nix
# NixOS
{
  imports = [ cognosis.nixosModules.default ];
  services.cognosis = {
    enable = true;
    environment.COGNOSIS_DSN = "postgres:///cognosis?host=/run/postgresql";
    environment.COGNOSIS_EMBEDDING_URL = "http://localhost:11434";
  };
}

# nix-darwin
{
  imports = [ cognosis.darwinModules.default ];
  services.cognosis = {
    enable = true;
    environment.COGNOSIS_DSN = "postgres://localhost/cognosis";
  };
}

# home-manager (user-level; systemd --user on Linux, launchd on Darwin)
{
  imports = [ cognosis.homeManagerModules.default ];
  services.cognosis = {
    enable = true;
    environment.COGNOSIS_DSN = "postgres://localhost/cognosis";
  };
}
```

All three modules generate the same `cognosis start --foreground` unit that
[contrib/cognosis.service](../contrib/cognosis.service) / [contrib/com.enqack.cognosis.plist](../contrib/com.enqack.cognosis.plist)
document for manual installs — use the Nix module *or* the manual copy-paste, not both. The `contrib/`
files stay as the reference for non-Nix installs (Path B).

For a complete single-host example wiring Postgres+pgvector, Ollama, and the service together —
peer-authenticated, no passwords, the embedding model pre-pulled — see
[contrib/flake.nix](../contrib/flake.nix).

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
trust_local_errors: false                             # see docs/configuration.md before enabling
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

That exposes the MCP tool surface (`write_note`, `edit_note`, `query_knowledge`, … — see [cli.md](cli.md)).

Note that `$(cat …)` interpolates the token **into the client config**, which then holds a copy the
daemon knows nothing about. See [Keeping the token out of client config](#keeping-the-token-out-of-client-config)
below, and mint one token per client rather than sharing `local` — see [remote.md](remote.md), which
applies locally too: a shared token makes every caller indistinguishable in `audit_log` and in the
`token=` log attribute.

---

## Rotating the local token

**Deleting `local-token` does not invalidate the old credential** — it only removes your copy. Both
steps are required, and **both must happen before the restart**:

```sh
cognosis token revoke local                 # the old credential dies on the next request
rm "$XDG_STATE_HOME/cognosis/local-token"   # before restarting, not after
cognosis stop && cognosis start             # mints a fresh token, under the same name `local`
```

The order is not arbitrary, and each wrong order fails differently:

- **Restarting with the file still present** → the daemon refuses to start. It will not mint around
  a revocation while a file points at the revoked row (see the `local token in … was revoked` row
  under [Troubleshooting](#troubleshooting)).
- **Restarting before the revoke** → the old row is still live and owns the name, so the daemon
  refuses to start rather than renaming itself. The error names the fix.

The name survives because uniqueness applies to **live** tokens only: the revoked row keeps its name
for the audit trail without reserving it. So `token=local` in a log line always means the daemon,
across any number of rotations.

Verify the old token is actually dead rather than assuming it:

```sh
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer <the old token>" http://127.0.0.1:7433/   # expect 401
```

Any client still holding the old token now gets `401`, including ones you forgot about; `cognosis token
list` shows `last used` per token, which is the quickest way to find them.

Other tokens rotate the same way, without the file dance:

```sh
cognosis token revoke desktop
cognosis token create desktop   # same name, new credential
```

Revoked rows accumulate for the audit trail. `cognosis token prune` deletes the ones nothing in
`audit_log` references; the rest are kept **by design**, so a revoked token surviving a prune means
it was used, not that the prune failed.

---

## Keeping the token out of client config

Interpolating the token into a client config copies a secret into a file nothing rotates. Both clients
below can read it from its 0600 file instead, so rotation is a file rewrite rather than a config edit.
`cognosis token create` prints the plaintext once, so write it straight into that file — taking the
first line only, since the command also prints a "shown once" notice that must not land in the file:

```sh
(umask 077; cognosis token create desktop | head -1 \
  > "${XDG_STATE_HOME:-$HOME/.local/state}/cognosis/desktop-token")
```

A file that captured the notice too fails loudly rather than silently: the helper rejects a value
containing spaces, and `mcp-remote` sends a malformed header that the daemon answers with `401`.

### Claude Code

Claude Code fetches headers from a command on every connection, and again after a `401` — so rotation
needs no client reconfiguration at all:

```sh
cp contrib/cognosis-mcp-headers ~/.local/bin/
chmod 755 ~/.local/bin/cognosis-mcp-headers
```

[contrib/cognosis-mcp-headers](../contrib/cognosis-mcp-headers) reads
`$XDG_STATE_HOME/cognosis/local-token` by default; set `COGNOSIS_TOKEN_FILE` to point a given client at
its own token.

Then set `headersHelper` on the server entry instead of `headers`:

```json
{
  "type": "http",
  "url": "http://127.0.0.1:7433",
  "headersHelper": "\"$HOME/.local/bin/cognosis-mcp-headers\""
}
```

`claude mcp add` has no flag for this — edit the config entry directly. The token then exists only in
its 0600 file.

The value runs through a shell, so a client with its own token sets the variable inline rather than
needing a second copy of the script:

```json
"headersHelper": "COGNOSIS_TOKEN_FILE=\"$HOME/.local/state/cognosis/code-token\" \"$HOME/.local/bin/cognosis-mcp-headers\""
```

Omitting that is easy to miss and fails *silently in the direction that looks fine*: the helper falls
back to `local-token`, the client connects, and every call is attributed to the shared token instead of
this client. Check with `cognosis token list` — the per-client token should show a recent `last used`.

### Claude Desktop

Claude Desktop has no `headersHelper`, and its config speaks only stdio — a remote server is reached
through the `mcp-remote` shim. Putting the token in `env` leaves the same unrotatable copy the helper
exists to avoid, so run the command under a shell and let it read the file:

```json
"cognosis": {
  "command": "/bin/sh",
  "args": [
    "-c",
    "exec npx -y mcp-remote http://127.0.0.1:7433 --header \"Authorization: Bearer $(cat \"${COGNOSIS_TOKEN_FILE:-$HOME/.local/state/cognosis/desktop-token}\")\""
  ]
}
```

The config lives at `~/Library/Application Support/Claude/claude_desktop_config.json` on macOS. The
entry above assumes a POSIX shell at `/bin/sh`; on Windows the equivalent needs a different wrapper.

Unlike `headersHelper`, this resolves the token **once, at launch**. Rotating means restarting Claude
Desktop, not just rewriting the file — the running process holds the old value until it exits, and the
symptom of forgetting is a `401` on every call.

Point it at a per-client token (`cognosis token create desktop`) for the same reason Claude Code does;
the fallback path above is `desktop-token` rather than `local-token` so that omitting
`COGNOSIS_TOKEN_FILE` does not quietly share the local credential.

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

Or run the full end-to-end feature checks (wants `COGNOSIS_DSN` + a local Ollama;
a check whose prerequisites are missing reports itself skipped and the run goes on):

```sh
mage check          # scripts/check-all.sh — daemon, memory-loop, retrieval,
                    # knowledge, platform, tls, embedding-migration,
                    # retrieval-eval
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
| `no migration found for version N` after a manual DB reset | The derived index is rebuildable, not migrated across a schema renumber. Recreate it: drop the schema (`drop schema public cascade; create schema public;`) or the database, then restart — boot reconciliation re-indexes from the vault. **This also drops the `tokens` table**, so the daemon mints a fresh local token on restart: re-read `local-token` and update any client config holding the old one (see the `401` row). No revoke is needed here — dropping the table removes the rows outright, unlike [rotation](#rotating-the-local-token). |
| SessionStart hook does nothing | No `.cognosis-project` marker at/above the repo root (by design). Add one to opt the repo in. |
| `401` from an MCP call | Run `cognosis status` first — a failing `auth` check confirms the stashed token itself no longer authenticates, and a passing one means the caller is sending something else. Otherwise: missing/typo'd bearer token, or the token was revoked. Re-read `local-token`, or mint one with `cognosis token create <name>`. A client config carrying a *copy* of the token is the usual culprit after a schema rebuild: the daemon re-mints into `local-token`, but nothing rewrites the copy. |
| `local token in … was revoked` on startup | Deliberate: the daemon will not mint around a revocation. Delete `local-token` to provision a new one — the file is the source of truth for whether local access is granted. The replacement reuses the name `local`; the revoked row stays for the audit trail until `cognosis token prune`. |
| `a live token named "local" already exists but … is missing` on startup | The database has a live `local` row but the state-dir file is gone — a fresh state dir pointed at an existing database, or a rotation done out of order. The plaintext cannot be recovered from the hash, so the daemon refuses rather than minting under a mangled name. `cognosis token revoke local`, then restart. |
| Rotated the local token but the old one still works | Deleting `local-token` removes your copy without revoking the row. Revoke it explicitly — see [Rotating the local token](#rotating-the-local-token). |
| Vault history full of `.obsidian` or `history.md` commits | Vaults created before this behaviour existed still *track* those files, and `.gitignore` alone does not untrack. Cognosis no longer commits them, but the already-tracked copies keep showing as modified — which also makes `note delete --hard` retry, since git refuses to rewrite history on a dirty tree. One-time cleanup: `cd "$XDG_DATA_HOME/cognosis/kb" && git rm -r --cached --quiet .obsidian/workspace.json .obsidian/graph.json history.md && git commit -m "stop tracking editor and generated state"`. Nothing is deleted from disk. |
| `graph FAIL … edge(s) missing` from `cognosis status` | The link graph disagrees with what the indexed note content implies. Links are resolved once at index time and never re-derived, so an edge lost to an interrupted write stays lost — reconciliation confirms drift by content hash and skips an unchanged file forever. Repair by **changing the content** of a named note (`edit_note` — `touch` will not do, for the same hash reason), which re-resolves its links; or drop the schema and restart to rebuild the whole index from the vault. Retrieval still works meanwhile: only the graph leg is degraded. |
