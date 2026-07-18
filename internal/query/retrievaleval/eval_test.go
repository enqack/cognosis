package retrievaleval

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/store"
)

// The sweeps below are local-tier: they build multi-second corpora and produce
// numbers that jitter with HNSW graph construction and machine load. CI is the
// wrong home for that — a perf or recall threshold on a shared runner is either
// meaningless or flaky, and flaky assertions get muted rather than fixed.
//
// The cheap harness-correctness tests (synth, metrics, the small corpus smoke
// test) are deliberately NOT gated: they are fast, deterministic, and they are
// what catches a degenerate corpus or a broken metric. Gating the whole package
// as originally planned would have removed exactly the checks worth keeping.
func requireEval(t testing.TB) {
	t.Helper()
	if os.Getenv("COGNOSIS_EVAL_DSN") == "" {
		t.Skip("COGNOSIS_EVAL_DSN not set; retrieval sweeps are local-tier (run scripts/checks/retrieval-eval.sh)")
	}
}

// evalSpec is the sweep corpus. Size is env-tunable because the only hard
// requirement is that the planner actually chooses HNSW — below roughly 3k
// chunks it picks a seqscan and every cell reports a full result set and
// perfect recall, which is indistinguishable from "no defect".
func evalSpec(t testing.TB) CorpusSpec {
	t.Helper()
	spec := DefaultSpec()
	if v := os.Getenv("COGNOSIS_EVAL_NOTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("COGNOSIS_EVAL_NOTES=%q: %v", v, err)
		}
		spec.Notes = n
	} else {
		spec.Notes = 1600 // 8k chunks: past the seqscan threshold, ~7s to build
	}
	return spec
}

// gucSettings are the scan configurations every sweep runs over.
var gucSettings = []struct {
	Name string
	Set  store.SessionSettings
}{
	// The pre-fix baseline must be stated explicitly. store.Connect now sets
	// ef_search and iterative_scan on every pooled connection, so a probe with
	// no SET LOCAL inherits the *fixed* settings — leaving "default" as nil
	// would silently relabel the fix as the baseline and make the sweep claim
	// there was never a defect.
	{"PRE-FIX(ef=40,off)", store.SessionSettings{
		"hnsw.ef_search": "40", "hnsw.iterative_scan": "off"}},
	{"session(inherits Connect)", nil},
	// Every row below pins BOTH knobs. A partial SET LOCAL inherits the other
	// from the session, so "ef_search=200" with iterative_scan left alone
	// silently measured ef_search=200 + relaxed_order once Connect started
	// setting it — reading 0.985 where the isolated setting measures 0.883.
	{"ef_search=200,iter=off", store.SessionSettings{
		"hnsw.ef_search": "200", "hnsw.iterative_scan": "off"}},
	{"ef=40,iterative=relaxed", store.SessionSettings{
		"hnsw.ef_search": "40", "hnsw.iterative_scan": "relaxed_order"}},
	{"ef=40,iterative=strict", store.SessionSettings{
		"hnsw.ef_search": "40", "hnsw.iterative_scan": "strict_order"}},
	// relaxed_order may return rows slightly out of distance order. RRF
	// consumes rank *position*, so ordering error in a leg propagates into
	// fusion — which is why strict_order is measured alongside rather than
	// assumed equivalent. Kendall tau in the recall sweep is the number that
	// separates them.
	{"ef_search=200+relaxed", store.SessionSettings{
		"hnsw.ef_search": "200", "hnsw.iterative_scan": "relaxed_order"}},
	{"ef_search=200+strict", store.SessionSettings{
		"hnsw.ef_search": "200", "hnsw.iterative_scan": "strict_order"}},
	// SHIPPED is the configuration store.Connect actually applies. It is in
	// the sweep so the deployed setting is a measured one rather than an
	// interpolation between measured neighbours.
	{"SHIPPED(ef=100+relaxed)", store.SessionSettings{
		"hnsw.ef_search": "100", "hnsw.iterative_scan": "relaxed_order"}},
}

// usedHNSW reports whether a plan touched the HNSW index. Every measurement
// that claims to say something about ANN behavior must check this first: a
// seqscan plan silently voids the result by reporting a full, exact answer.
func usedHNSW(plan string) bool { return strings.Contains(plan, "hnsw_idx") }

// accessPath extracts the plan node touching the embeddings table, for the
// recorded artifacts.
func accessPath(plan, table string) string {
	for line := range strings.SplitSeq(plan, "\n") {
		if strings.Contains(line, table) {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "->"))
			if i := strings.Index(line, "  (cost"); i > 0 {
				line = line[:i]
			}
			return strings.TrimSpace(line)
		}
	}
	return "(embeddings relation not in plan)"
}

// evalDSN returns the corpus DSN with extra session GUCs appended to the
// startup-packet options. Engine.Run has no per-query settings hook, so this is
// how an end-to-end run gets non-default scan settings. Postgres accepts
// dotted (extension-namespaced) GUC names as placeholders even before the
// extension loads, which is what makes this work for hnsw.*.
func evalDSN(t testing.TB, dsn string, set store.SessionSettings) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	opts := q.Get("options") // already carries -csearch_path=<schema>
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		opts += fmt.Sprintf(" -c%s=%s", k, set[k])
	}
	q.Set("options", strings.TrimSpace(opts))
	u.RawQuery = q.Encode()
	return u.String()
}

func sortStrings(xs []string) {
	for i := range xs {
		for j := i + 1; j < len(xs); j++ {
			if xs[j] < xs[i] {
				xs[i], xs[j] = xs[j], xs[i]
			}
		}
	}
}

// elideVectors shortens the 768-dimension vector literals that EXPLAIN echoes
// back, which otherwise make a recorded plan unreadable.
func elideVectors(plan string) string {
	for {
		i := strings.Index(plan, "'[")
		if i < 0 {
			return plan
		}
		j := strings.Index(plan[i:], "]'")
		if j < 0 {
			return plan
		}
		j += i + 2
		if j-i < 80 {
			// Already short; skip past it to avoid an infinite loop.
			head, tail := plan[:j], plan[j:]
			rest := elideVectors(tail)
			return head + rest
		}
		plan = plan[:i] + "'[<768-dim vector elided>]'" + plan[j:]
	}
}

// writeArtifact records a measurement table under testdata/. These are
// recorded, not diffed: HNSW graph construction depends on insert order and
// parallel build workers, so byte-diffing recall numbers produces flaky CI.
// Bounds are asserted in the tests; the numbers are written down for a human.
func writeArtifact(t testing.TB, name, body string) {
	t.Helper()
	dir := "testdata"
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %s", path)
}

// baselineSetting names the pre-fix row that every other row is compared
// against. Referenced by name so a rename in gucSettings breaks compilation
// rather than silently reading a zero value out of a results map.
const baselineSetting = "PRE-FIX(ef=40,off)"
