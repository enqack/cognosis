# Cognosis -- Architecture

How Cognosis is built and why. This is the in-repo overview; the setup and reference material live in
[setup-guide.md](setup-guide.md), [configuration.md](configuration.md), and [cli.md](cli.md).

## Shape

Cognosis is a single background daemon that gives MCP-capable AI agents persistent, cross-project
memory. One knowledge base, many projects, accessed as a service:

```
        MCP client (Claude Code, ...)
                 |  Streamable HTTP + bearer token
        +--------v---------+
        |  cognosis daemon |  write / query / compile / migrate
        +---+---------+----+
            |         |
   markdown vault   Postgres            Ollama
 (source of truth)  (derived index)   (embeddings)
```

- **The markdown vault is the single source of truth.** Everything durable is version-controlled
  markdown under `$XDG_DATA_HOME/cognosis/kb/`.
- **Postgres is a derived, droppable index** -- chunks, embeddings, and the link graph. It can be
  rebuilt from the vault at any time; nothing canonical lives only in the database.
- **Embeddings are local** (Ollama, `nomic-embed-text:v1.5`) by default.

## The vault

The vault is a flat set of stage folders -- the folder encodes a note's *processing stage*, while its
semantic type lives in frontmatter:

```
kb/
+-- index.md      (okf_version declaration; validated, never written by Cognosis)
+-- log.md        (append-only compile audit trail)
+-- history.md    (generated read-only recovery dashboard)
+-- entries/      raw, timestamped capture
+-- notes/        atomic processed notes (these decay)
+-- reflections/  persona-authored freeform writing
+-- archive/      retired notes
```

Cognosis commits only the paths it owns -- the four stage directories, `log.md` and `index.md`. The
vault directory is shared with whatever an operator runs in it (Obsidian is a designed-for
workflow), and committing everything put editor state into 22% of a real vault's commits, including
commits whose message named a note they did not contain.

The two other reserved names are treated differently, and the split is not arbitrary. `index.md` is
versioned despite never being written by Cognosis: it carries the `okf_version` declaration that
says how everything else should be read, so a change to it must leave a trace. `history.md` is
excluded because it is generated *from* `git log`, so tracking it made the recovery dashboard cite
its own churn as restorable.

The commit is assembled in a scratch index rather than the repo's own. Two things follow, and
neither is achievable with a plain `git commit`. Anything another party has staged in the real index
is never read, so it cannot ride along under a message naming only the note Cognosis just wrote. And
the commit is a *snapshot* taken when the paths are staged, not a live read of the working tree -- so
a concurrent writer's file landing mid-commit is not swept into someone else's message, and stays
pending for its own commit instead of being silently absorbed.

A vault Cognosis creates is seeded with a `.gitignore` for the same paths. Vaults predating that
still track them -- see the troubleshooting row in [setup-guide.md](setup-guide.md) for the one-time
`git rm --cached`.

Every note carries a frontmatter contract (required `category`, `created`, `updated`; extra decay
fields for `notes/`). `id` is assigned when omitted -- a new one for a new path, the existing one
when overwriting -- so a caller holding only the MCP tools need not mint one. A validator enforces the contract on every write, so a malformed note is
rejected with the offending field named rather than silently indexed.

The `id` is a **UUIDv7** -- time-ordered, so ids sort lexically by creation time and index inserts stay
sequential instead of scattering a b-tree. This is enforced, not advised: an id is written once and
never rewritten, so any version accepted at write time is permanent. Mint one with
`vault.NewNoteID()`; every code path that creates a note uses it. Changing an existing note's id is
refused: the index treats same-path-different-id as an eviction, which drops the row and re-points
its inbound links.

## The daemon

Startup is a linear, fail-fast sequence -- any failure in the first steps is fatal, never degraded:

1. Postgres reachability (named target on failure)
2. schema migrations (auto-applied, embedded SQL)
3. history repo init
4. **single-instance lock** (below) -- before any reconciliation
5. embedding-provider reachability + table provisioning
6. boot reconciliation of the vault into the index
7. serve: MCP server + file watcher + migration worker

Provisioning precedes reconciliation because indexing a note embeds its chunks: against a fresh
schema there is no active provider yet, and reconciliation would fail every note and only index the
vault on a second boot.

In the serve phase, per-item background work in the file watcher and migration worker recovers from
panics (logged and isolated to that one item) rather than crashing the daemon; a panic in a primary
runner still brings the whole process down.

**Many clients, one surface.** The MCP server serves concurrent clients over Streamable HTTP: each gets
its own session, but they share a single tool registration, and identity is resolved per request from
its bearer token rather than pinned to a connection. The tools hold no per-client state -- everything
durable lives in Postgres and the vault -- so concurrency needs no coordination beyond the write
serialization the vault history and single-instance sections describe.

**Reconciliation** keeps the derived index in step with hand-edits (Obsidian, a text editor, git):
a live `fsnotify` watcher plus a boot-time `mtime`/size pre-check backed by BLAKE3 hashing, plus a
periodic sweep. Cognosis's own writes suppress the watcher for the file being written, so it never
self-triggers.

## Retrieval -- hybrid RRF

`query_knowledge` fuses three rankers with reciprocal-rank fusion, computed in Go:

- **vector** -- pgvector cosine distance, one leg per provisioned embedding provider;
- **keyword** -- Postgres full-text search (`ts_rank_cd`), with an AND-starvation fallback chain (see below);
- **graph** -- one hop out along the link graph from the notes behind the other legs' candidates.

The independent legs (vector + keyword) fan out concurrently; the graph leg runs after, since it's
seeded by their candidates. Leg order is fixed, so fusion is deterministic regardless of completion
order. Optional lenses ride on top without changing the contract: `as_of` (reason over frontmatter
timestamps -- "what did the KB believe at time T"), `persona_filter` (category-bias reweighting),
project scoping, and cached one-line summaries returned with each hit.

The fused list passes through three score rewrites before the top-K cut, in order: the archived-link
penalty (see Soft-delete hygiene), the persona category bias, and a **fan-effect diversity penalty**.
Fusion is chunk-level with no per-note constraint, so one long note's chunks can crowd the answer
while a shorter relevant note never places (measured on the real vault: the un-penalized top-8
returned only ~5.3 distinct notes of 8, one note owning 7 slots). The penalty scales a note's n-th
chunk (in score order) by `diversityDecay^n` (`0.5`), so its best chunk competes at full strength
while each extra yields ground -- capping a note at roughly two top-8 slots and lifting distinct
sources to ~7.9, while a note that genuinely out-scores everything keeps a second slot. It reorders
one query's fused list only; it touches no note state. `LegStats.Sources`/`FusedSources` instrument
it, and `TestDiversityRerankSweep` swept the decay (relevance held flat on the labelled corpus).

### The keyword leg's AND-starvation fallback chain

`websearch_to_tsquery` joins terms with **AND**, and `chunks.fts` is per-heading. A query whose
terms are spread across different sections of one note therefore matches none of its chunks, and the
note is *absent* rather than demoted -- the keyword leg contributes membership rather than ordering,
so a candidate it never produces cannot be recovered downstream. On the live vault this starved the
AND conjunction on 100% of logged real queries.

When the conjunction returns fewer than `ftsFallbackBelow` (2) candidates, the leg escalates through
a chain of increasing blast radius, keeping the first result that clears the threshold:

1. **note-level membership.** A stored `notes.fts` tsvector -- `to_tsvector` over the whole note
   body (`notes.content`), GIN-indexed -- decides membership at the *note* level, so a note whose
   terms are scattered across headings AND-matches even though no single chunk does. The surviving
   notes' chunks are still ranked by the per-chunk `chunks.fts`. This recovers the scattered target
   at roughly ten times the candidate precision of a bare OR, so it is tried first.
2. **OR disjunction** -- the recall floor, reached only when note-level still starves (the terms are
   genuinely absent from any one note as a set, not merely scattered within one). It can only add to
   recall from here, never subtract, because it runs only when nothing better cleared the threshold.

The retries are sequential, not speculative: on a healthy query AND saturates the pool and neither
fires, so running the extra connectives every time would double the leg's database work to discard a
result almost always.

The threshold is 2 rather than 1, and that is the whole of the gate. Firing only on an empty result
is measurably insufficient -- the real-vault query that motivated the chain returned exactly one
candidate belonging to the wrong note, where a fire-on-empty rule is byte-identical to no fallback.

Measured on the 8000-chunk corpus `scripts/checks/retrieval-eval.sh` builds, target-note recall in
the fused top-8 over the starving query sets:

| keyword arm | totally starving | partially starving (one wrong candidate) |
|---|---|---|
| AND (per-chunk) | 0.333 | 0.067 |
| OR fallback (`fallback@2`) | 0.467 | 0.667 |
| **note-level fallback** | **1.000** | **1.000** |

Note-level reaches the target on every starving query, with ~10x the candidate precision of OR
(a handful of on-target chunks versus fifty near-random ones), and leaves healthy queries
byte-identical to the AND baseline (fused top-8 Jaccard 1.000 -- the chain never fires when AND
saturates). The recorded artifacts are
`internal/query/retrievaleval/testdata/note_level_membership.txt` and `tsquery_or_fallback_sweep.txt`,
which state their corpus size in the header -- read the numbers there rather than these, which are a
snapshot.

`LegStats.FTSFallback` records when the chain fires and `FTSFallbackKind` records which arm produced
the result (`note-level` or `or`), both logged per query. This matters: `fts=35` looks like a
healthy keyword leg whether it came from a conjunction that matched, a precise note-level recovery,
or a disjunction papering over a failed conjunction -- and the three have very different precision.

The sweeps are `internal/query/retrievaleval/notelevelmembership_test.go` and
`tsqueryfallback_test.go`, and the request-path wiring is pinned by
`TestNoteLevelFallbackReachesRun` (`internal/query/note_level_wiring_test.go`).

### Scan settings for the vector leg

Every pooled connection sets `hnsw.ef_search` and `hnsw.iterative_scan` (`store.Connect`). These
are not tuning preferences; without them the vector leg silently under-returns.

pgvector's scope filters (`project`, `include_archived`, `as_of`) are applied in the same statement
as the ANN order-by, but the index walk itself is unaware of them: HNSW produces one `ef_search`-sized
candidate list and the predicate is applied to *that*. With `iterative_scan = off` the scan then
stops, so a filtered query returns however few of those candidates survived. Measured on an
8k-chunk corpus asking for 50 candidates, a scope holding a quarter of the notes returned **10 rows,
recall 0.205** against exact KNN -- and 8 rows at 20k chunks, so it worsens as a vault grows.

`iterative_scan = relaxed_order` is the fix: the scan resumes until the `LIMIT` is met. Raising
`ef_search` alone does not fix it (0.881 at `ef_search=200`) -- it only enlarges the list being
filtered down. `ef_search = 100` is kept as headroom above the 50-candidate pool.

`relaxed_order` over `strict_order` is deliberate: relaxed admits slightly out-of-distance-order
rows (Kendall ~0.995 vs 1.000) but retrieves more true neighbours (0.978 vs 0.965 recall on the
worst scope). RRF consumes rank position with `k=60` damping, so rank 1 scores `1/61` and rank 50
scores `1/110` -- the whole rank range spans 1.8x, while a missing row contributes exactly 0.
Recall dominates ordering under this fusion.

How to run the harness, what the artifacts mean, and which retrieval questions are already
closed: [benchmarking.md](benchmarking.md).

The measurement harness behind these numbers is `internal/query/retrievaleval` (local-tier; run
`scripts/checks/retrieval-eval.sh`, artifacts under its `testdata/`). `TestCandidatePoolWithinScanCapacity`
guards the invariant in CI.

**Requires pgvector >= 0.8** for `iterative_scan`. On older versions the settings fail with a logged
warning and retrieval degrades to the previous under-returning behavior rather than breaking.

## Knowledge lifecycle

`compile_lifecycle` is an explicit, agent-driven pass (never a timer). It reinforces, decays,
archives, and -- on explicit request -- falsifies, disputes, supersedes, or graduates notes:

- **decay** -- confidence is a read-time function of time since the last explicit reinforce, not a
  per-run decrement (see below); a note that fades below the archival floor is archived;
- **reinforce** -- an assertion of belief: confidence returns to its peak and the note's stability
  grows, so each review widens the interval before it fades again (the spacing effect);
- **citation-refresh** -- a note cited by a recently-updated note is shielded from the archival *move*
  (within a budget past the last explicit reinforce); citation is evidence of use, not belief, so it
  never touches confidence;
- **falsify** -- terminal: wrong, retained, frozen, excluded from default retrieval;
- **dispute** -- non-terminal: contested, keeps decaying; a later reinforce clears it;
- **graduate** -- in-place canonization via a `graduated_at` frontmatter stamp (the layout has no canon
  folder), which exempts a stable note from further decay;
- **verify** (optional) -- a retrieval-augmented pass surfaces related notes as advisory context before
  a terminal move.

Each run is one revertible history commit and appends its report to `log.md`; `dry_run` computes the
same report and writes nothing.

### Read-time decay

Confidence follows a power-law forgetting curve, `confidence(t) = (1 + t/S)^-0.5`, where `t` is the
time since the note's last explicit reinforce and `S` is a per-note **stability** (days), stored in
frontmatter. The pass evaluates the curve fresh every run, so confidence is a pure function of time
and never an accumulation -- the decay rate cannot depend on how often the agent happens to compile,
which the previous flat-then-staircase model got wrong.

Stability is the memory of reinforcement: a fresh note starts at 14 days (half-life ~42d, volatile
enough to signal "reinforce me"), each explicit reinforce multiplies it by 1.9 (the spacing effect),
and promotion to `stable` multiplies it by 4 (semantic consolidation -- canon gets an effectively
permanent tail). So a well-reinforced note barely moves over a year while an unreinforced one crosses
the archival floor (`0.2`) around 336 days. Notes written before stability was tracked reconstruct it
from their reinforcement history on the first pass. Paused and graduated notes are frozen, exempt from
the curve entirely.

## Soft-delete hygiene

A soft delete (archival) must not leak back into the agent's context. Two mechanisms:

- **status exclusion** -- retrieval excludes `faded`/`archived` (and `falsified`) notes by default;
  `include_archived` opts them back in for audits. An `archived_at` stamp keeps `as_of` honest (a note
  archived after T still counts as live at T).
- **archived-link RRF penalty** -- any chunk whose parent note links to an archived note is severely
  penalized in fusion, so a dense stale reflection about a shelved concept can't outrank live truth.
  The append-only text is never rewritten; only its live ranking is discounted.

## Vault history -- the recovery net

An auto-managed, local-only git repo inside the vault records every sanctioned write and every compile
run (one commit per run). It's surfaced three ways so recovery never requires arcane git:

- a generated read-only `history.md` dashboard at the vault root (refreshed on compile and boot) listing
  recent revertable states with the exact restore command;
- the `vault_history` / `restore_note` MCP tools, so an agent can read the log and mediate a rollback;
- the `cognosis vault history` / `restore` CLI.

All git-index mutations are serialized process-wide, so concurrent writers (pipeline, compile,
restore, watcher) can't collide.

## Single-instance invariant

Only one daemon may own a database. Two guards:

- a **Postgres session advisory lock** held on a dedicated connection for the daemon's lifetime -- the
  cross-machine arbiter (two daemons on different hosts pointed at one database can't both run), which
  releases automatically if the daemon crashes;
- the local PID lockfile -- a fast same-machine guard.

A liveness ping stops the daemon if the lock connection is lost, rather than run unguarded.

## Embedding-provider migration

Switching embedding providers/models is zero-downtime. Each provider gets its own `vector(N)` table
(dimensions from different models aren't comparable). A migration runs a background back-fill worker,
lazily migrates chunks the moment they're queried, dual-embeds new writes, and reads through the
multi-provider fan-out as the fallback -- so retrieval never sees a chunk with no embedding. Pausable,
resumable, and rollback-able; the old table survives until an explicit `embeddings prune`.

## Auth & audit

Per-client bearer tokens, Argon2id-hashed at rest, checked synchronously on every request (no cache --
revocation is effective on the very next request; the `last_used_at` touch is debounced so the read
path isn't a write-per-request). A zero-config local token is minted on first start. Every tool call is
audit-logged under the resolved token identity, with a redacted `args_summary` that never contains note
content. Non-loopback binds require TLS; see [remote.md](remote.md).

### Log attribution

MCP-originated log lines carry `token=<name>`, added by `auth.NewIdentityHandler` from the identity
`auth.Middleware` puts in the request context. This is what makes per-leg retrieval telemetry --
`query_knowledge`'s `vector`/`fts`/`fts_and`/`graph`/`sources`/`fts_fallback`/`fts_fallback_kind` counters, which live only in the log
and never in `audit_log` -- separable by client.

**A missing `token=` identifies daemon-internal work, not broken attribution.** The watcher, the
migration worker, CLI-driven lifecycle compiles and every startup line run with no authenticated caller.

Two asymmetries are deliberate. The log records the token *name* while `audit_log` records its *id* --
**a name identifies a client, an id identifies a credential.** That split is what lets a name be
reused: rotating a client's token keeps `token=desktop` meaning Desktop, while `audit_log.token_id`
still pins exactly which credential made each call. Token ids are UUIDv7, so they are time-ordered
and generations sort. `cognosis token prune` is the only delete and refuses any row `audit_log`
references, so the read-time join to `tokens.name` never dangles; the FK's NO ACTION is the backstop
that would turn a bug there into an error rather than a silent orphan.

Names are unique among **live** tokens only (`tokens_live_name_idx`), so a revoked row keeps its name
for the audit trail without reserving it. `local` is reserved from operator creation
(`auth.ValidateTokenName`) and the daemon mints under exactly that name or refuses -- so `token=local`
always means the daemon itself.

Request-scoped log calls must use slog's `*Context` variants or the identity
is silently dropped -- enforced by `TestRequestScopedLogsCarryContext`, which parses the package, plus
`sloglint`'s `context` check scoped to `internal/mcpserver`.

Per-client attribution requires per-client tokens; a shared token makes every caller look identical.
If that ever proves insufficient, the MCP SDK exposes `req.GetSession().ID()` in receiving middleware,
which distinguishes clients even on one token.

## Deletion

Two paths: **soft delete** (archival via `compile_lifecycle`, the only deletion an agent can do --
recoverable) and **hard delete** (`cognosis note delete --hard`, CLI-only, genuine erasure that cascades
across the file, index, embeddings, `log.md` mentions, and vault history -- for compliance cases where
"excluded from retrieval" isn't enough).
