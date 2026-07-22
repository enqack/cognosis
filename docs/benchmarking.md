# Cognosis -- Benchmarking and Retrieval Evaluation

How to run every local measurement in this repo, and which retrieval questions are already answered.

Retrieval is the part of Cognosis where a defect does not announce itself. A leg that silently
returns half its candidates still returns *something*, and the results look plausible -- so the only
way to know is to measure. This page is the operating manual for that apparatus.

The measured conclusions about scan settings live in
[architecture.md](architecture.md#retrieval--hybrid-rrf); this page is about running the
measurements and about what has already been settled.

---

## Three tiers

| Tier | Command | Needs | Runs in CI |
|---|---|---|---|
| Unit + integration | `mage test` | `COGNOSIS_TEST_DSN` | **yes** |
| End-to-end feature checks | `mage check` | `COGNOSIS_DSN`, Ollama | no |
| Benchmarks | `mage bench` | `COGNOSIS_TEST_DSN` + `COGNOSIS_EVAL_DSN` | no |

The tiering is deliberate, and the reasoning is in the source rather than only here.
`magefile.go` on why benchmarks are local-only:

> Local/dev only -- needs COGNOSIS_TEST_DSN and COGNOSIS_EVAL_DSN set, and deliberately runs
> WITHOUT -race: race instrumentation makes latency numbers meaningless.

And `internal/query/retrievaleval/eval_test.go` on why the sweeps are:

> a perf or recall threshold on a shared runner is either meaningless or flaky, and flaky
> assertions get muted rather than fixed.

That is the whole argument. Recall numbers jitter with HNSW graph construction and machine load, so
a CI threshold over them is either so loose it catches nothing or so tight it goes red on noise --
and a test that goes red on noise gets muted, which is worse than not having it.

**What *is* asserted in CI:** bounds and relations, never absolute numbers. Raising `ef_search`
must not *reduce* recall; the candidate pool must not exceed scan capacity
(`internal/query/pool_capacity_test.go`). Those hold on any machine.

---

## The four DSN variables

They are not interchangeable, and mixing them up is the most common way to get a confusing skip.

| Variable | Read by | Unset |
|---|---|---|
| `COGNOSIS_TEST_DSN` | Go integration tests (`internal/store/storetest`) | test skips |
| `COGNOSIS_DSN` | `scripts/checks/*.sh` | check **skips** (exit 2) |
| `COGNOSIS_EVAL_DSN` | `requireEval` in the sweeps | sweep skips |
| `COGNOSIS_GRAPHTUNE_DSN` | the real-vault sweeps (see "The real-vault tier") | those sweeps skip |

Exact skip messages, so you can match what you see:

```
COGNOSIS_TEST_DSN not set; integration tests need a real Postgres (run pg-start in the dev shell)
COGNOSIS_EVAL_DSN not set; retrieval sweeps are local-tier (run scripts/checks/retrieval-eval.sh)
COGNOSIS_DSN must point at a reachable Postgres (run pg-start in the dev shell)
```

**`COGNOSIS_EVAL_DSN` is a boolean, not a DSN.** `requireEval` only checks that it is non-empty --
its value is never used to connect. The corpus connects through `COGNOSIS_TEST_DSN`. Setting only
`COGNOSIS_EVAL_DSN` gets you a skip from a different variable, which reads like a bug and is not.

In the dev shell, `pg-start` exports `COGNOSIS_DSN`; the usual incantation is:

```sh
pg-start
export COGNOSIS_TEST_DSN="$COGNOSIS_DSN"
export COGNOSIS_EVAL_DSN="$COGNOSIS_DSN"
```

**The checks skip rather than fail** when a prerequisite is missing -- exit 2, distinct from a real
failure, so `check-all.sh` reports `SKIP:` and carries on. A run where everything skipped exits 2
and says so, because "nothing was verified" must not look like "everything passed".

---

## Running the sweeps

```sh
./scripts/checks/retrieval-eval.sh          # also runs as part of mage check
COGNOSIS_EVAL_NOTES=4000 ./scripts/checks/retrieval-eval.sh   # bigger corpus
COGNOSIS_EVAL_LINKDEGREE=8 ./scripts/checks/retrieval-eval.sh # denser link graph (graph-leg sweeps)
```

Postgres only -- no Ollama. The corpus uses the deterministic clustered `Synth` provider rather than
a live embedding server, which is what makes the numbers reproducible.

**Corpus size has a floor, and it matters.** Default is 1600 notes (8k chunks). Below roughly 3k
chunks the planner picks a seqscan, and then every cell reports a full result set and perfect
recall -- *indistinguishable from having no defect*. A sweep on too small a corpus does not fail; it
quietly reports that everything is fine.

### The scan-setting matrix

Each sweep runs over eight configurations, from `PRE-FIX(ef=40,off)` through
`SHIPPED(ef=100+relaxed)`. Two properties of that matrix are load-bearing:

- **The pre-fix baseline is stated explicitly**, not left as "no settings". `store.Connect` now sets
  both knobs on every pooled connection, so a probe that sets nothing inherits the *fixed* values --
  leaving the baseline implicit would relabel the fix as the baseline and make the sweep claim there
  was never a defect.
- **Every row pins both knobs.** A partial `SET LOCAL` inherits the other from the session, so a row
  labelled `ef_search=200` silently measured `ef_search=200 + relaxed_order` once `Connect` started
  setting it -- reading 0.985 where the isolated setting measures 0.883.

### What `mage check` does not run

`retrieval-eval.sh` runs nine sweeps: capacity, recall-vs-exact, fused overlap, the ground-truth
plan record, the keyword OR-fallback sweep, the note-level membership sweep, the diversity-rerank
sweep, and the two graph-leg sweeps (weight and unique contribution). The keyword sweeps
(`TestKeywordRankerCeiling`, `TestKeywordHeadroomVsPrecision`, `TestKeywordANDvsOR`) and
`TestAllLegCapacityAtShippedSettings` are **not** in that filter.

This is deliberate: they answer questions listed as closed below, and re-running them on every
`mage check` costs minutes to reproduce a result nobody is asking for. Run them by hand if you are
reopening one:

```sh
go test ./internal/query/retrievaleval/ -v -timeout 30m -run 'TestKeyword'
```

### The real-vault tier

The synthetic corpus cannot exercise everything. Its links are uniform-random, its projects are
round-robin, and its embeddings come from the deterministic `Synth` provider -- so the graph leg
looks inert, project context looks flat, and nothing tests a mature link topology. A second tier
runs the real engine against an **isolated `pg_dump` of a real vault** (never the live DB), with
real Ollama embeddings.

`COGNOSIS_GRAPHTUNE_DSN` points at that dump. Unlike `COGNOSIS_EVAL_DSN` it is a genuine connection
string, and these sweeps also need Ollama. They are manual-only and deliberately out of
`retrieval-eval.sh` -- a shared runner cannot carry a vault snapshot -- the same treatment
`TestGraphWeightRealVault` has always had. Six sweeps run here:

- `TestGraphWeightRealVault` -- the graph leg's fusion weight over a mature link graph.
- `TestDiversityRealVault` -- the fan-effect diversity penalty on real crowding.
- `TestSpreadingActivation` / `TestSpreadingActivationDepthWeight` -- multi-hop graph depth against
  per-hop decay and against fusion weight.
- `TestRetrievability` -- P(a note returns for a self-cue drawn from its own text), the
  cue-dependent-forgetting probe.
- `TestContextPrior` -- whether a same-project boost re-ranks anything the graph leg does not.
- `TestContextRelevance` -- that boost's benefit (right context) and cross-project harm (wrong
  context).

```sh
# an isolated dump, never postgres:///cognosis
export COGNOSIS_GRAPHTUNE_DSN="postgres:///cognosis_dump?host=...&sslmode=disable"
export OLLAMA_MODEL=nomic-embed-text:v1.5   # must match the dump's embedding table
go test ./internal/query/retrievaleval/ -v -timeout 30m \
  -run 'TestGraphWeightRealVault|TestDiversityRealVault|TestSpreadingActivation|TestRetrievability|TestContextPrior|TestContextRelevance'
```

The graphweight and diversity real-vault sweeps print to the test log; the other four write
artifacts (below).

---

## Benchmarks

```sh
mage bench                      # -bench . -benchtime 20x, no -race
mage bench | tee new.txt
benchstat old.txt new.txt
```

Never with `-race`: instrumentation makes latency numbers meaningless. Nothing in the benchmarks
asserts on wall-clock -- they exist to be read by a human, or diffed with `benchstat`.

`BenchmarkRunEndToEnd` is the one that matters: fan-out plus fusion, pre-fix scan settings against
shipped. The delta is what the fix costs as an agent experiences it.

---

## Artifacts

The synthetic-corpus sweeps write thirteen files into `internal/query/retrievaleval/testdata/`:

| File | Written by |
|---|---|
| `leg_capacity.txt` | `TestVectorLegCapacity` |
| `vector_recall.txt` | `TestVectorLegRecallVsExact` |
| `fused_overlap.txt` | `TestFusedTopKUnderCorrectedScan` |
| `explain_vector_exact.txt` | `TestRecordExactProbePlan` |
| `tsquery_or_fallback_sweep.txt` | `TestKeywordORFallbackSweep` |
| `graph_weight_sweep.txt` | `TestGraphWeightSweep` |
| `graph_leg_contribution.txt` | `TestGraphLegContribution` |
| `all_legs_capacity.txt` | `TestAllLegCapacityAtShippedSettings` |
| `keyword_ceiling.txt` | `TestKeywordRankerCeiling` |
| `keyword_precision_sweep.txt` | `TestKeywordHeadroomVsPrecision` |
| `tsquery_and_vs_or.txt` | `TestKeywordANDvsOR` |
| `note_level_membership.txt` | `TestNoteLevelMembershipSweep` |
| `diversity_rerank_sweep.txt` | `TestDiversityRerankSweep` |

The real-vault tier writes six more into the same directory: `spreading_activation_sweep.txt`,
`spreading_activation_weight_sweep.txt`, `retrievability_sweep.txt`, `context_prior_sweep.txt`,
`context_relevance_sweep.txt`, and `context_crossproject_sweep.txt`.

**That directory is gitignored.** Per `.gitignore`: the sweeps rewrite them on every run, they are
"recorded measurements for a human to read, never diffed or asserted against", and a committed copy
is just whoever ran the sweep last.

Do not confuse it with `internal/query/testdata/`, which **is** tracked and where
`golden_rankings.txt` and `golden_rankings_biased.txt` are compared byte-for-byte. Those are real
goldens over a 4-note fixture with deterministic pinned vectors. There is no `-update` flag -- if a
golden changes, that is a claim about retrieval behaviour and wants a human decision.

`explain_vector_exact.txt` deserves a look after any sweep: the exact-KNN ground truth is only
ground truth if it genuinely bypassed the index, and that file is the evidence rather than the
assertion.

---

## The corpus

`CorpusSpec` in `corpus.go`. Most fields are ordinary; three carry more weight than they look:

- **`ProjectMix`** -- skewed `{"": 0.74, "wide": 0.25, "narrow": 0.01}`, not round-robin. Uniform
  selectivity would under-demonstrate the defect: it is the *selective* scope that forces a filtered
  scan to walk far past its candidate list.
- **`BorrowedTerms`** -- how many of a neighbouring cluster's head words each vocabulary carries.
  At `0` the keyword leg is perfectly precise, because a conjunction can only match its own cluster
  -- which makes any ranker comparison vacuous. This was learned the hard way; see the closed BM25
  item below.
- **`Queries`** -- 30, because HNSW recall is not monotone per query. A single-query measurement is
  noise.

---

## What has been measured

Conclusions only. Figures live in the commits that produced them and in
[architecture.md](architecture.md#retrieval--hybrid-rrf), which is the home for the shipped scan
settings. They are deliberately not restated here: the corpus has been rebuilt twice, and each
rebuild invalidated the previous numbers.

**The vector leg was silently under-returning on filtered scopes.** Fixed with
`hnsw.iterative_scan = relaxed_order` plus `ef_search = 100` per pooled connection. Raising
`ef_search` alone does not fix it -- a larger candidate list is still filtered afterwards.

**The keyword leg contributes membership, not order.** Deleting it changes essentially every fused
result; ordering it *perfectly* changes about two queries in thirty. This is the single most useful
retrieval fact in the repo, because it redirects effort from rankers to the tsquery.

**On real traffic, the keyword leg is frequently silent.** `query_knowledge` now logs per-leg
candidate counts (`vector=`, `fts=`, `graph=`, `fused=`). Ordinary multi-term questions produce
`fts=0`, because `websearch_to_tsquery` joins terms with AND and no chunk contains all of them.

Note `fts=0` does not attribute itself -- genuine term absence looks identical to AND starvation.
Comparing against an OR count over the same corpus is what separates them.

---

## Closed, and what would reopen each

**BM25 (`pg_textsearch`, `pg_search`, `vchord_bm25`) -- not worth it.**
RRF consumes rank, so any keyword ranker can only permute the candidate list. Ordering that list by
ground truth is better than any real ranker can be, and it moves the fused top-8 by about two
queries in thirty. Two explanations for that flatness were tested and refuted: RRF damping (swept
`k` from 1 to 60, flat) and corpus precision (swept 1.00 to 0.87, flat, while the oracle permuted up
to 78% of slots).
*Reopens if:* the keyword leg's candidate set stops being the bottleneck -- i.e. after an AND/OR
change -- or if measured production keyword precision is far below 0.87.

**pgvectorscale / label-filtered DiskANN -- dominated.**
The recall problem it addresses was real but had a different cause, fixed by two session GUCs
already available in pgvector >= 0.8. No Rust/PGRX dependency, no second index-build path, no
`shared_preload_libraries`. *Reopens if:* a vault outgrows what HNSW with iterative scan can serve.

**RRF `k` sweep -- no headroom there.**
Plausible hypothesis that `k=60` damping flattens rank differences. Measured: headroom is flat at
every `k`. *Reopens if:* the fusion weights change materially.

**Keyword precision sweep -- flat across the measurable range.**
*Reopens if:* production data shows precision well below the 0.87 floor reached synthetically.

**AND vs OR on the synthetic corpus -- inconclusive by construction.**
Both arms scored 1.000 top-8 relevance. That is a **ceiling effect, not a tie**: when both arms are
perfect, "no difference" carries no information. The corpus has 40 well-separated clusters and AND
never returned zero, so OR's entire advantage never fired. *Reopens with:* production `fts=` counts,
which the per-leg logging is now collecting -- not another synthetic corpus.

That last one is the general lesson. The corpus was rebuilt twice in one session; each time it
answered the question it was built for and was then too easy for the next one. A fixture answers the
question you designed it for and quietly fails the next, and the failure looks like a clean result.

---

## Open

- **Real-vault keyword precision.** Needs accumulated `fts=` counts from live traffic.
- **AND vs OR.** Blocked on the same data, not on effort. A few weeks of ordinary use answers it
  with production numbers instead of a fixture.

---

## Related

- [architecture.md](architecture.md#retrieval--hybrid-rrf) -- the retrieval design and the shipped
  scan settings with their measured justification.
- [setup-guide.md](setup-guide.md) -- getting Postgres, Ollama and the daemon running first.
- The vault note `measuring-retrieval-without-fooling-yourself` catalogues the ways a retrieval
  measurement can report a confident, plausible, wrong number. Worth reading before adding a sweep.
