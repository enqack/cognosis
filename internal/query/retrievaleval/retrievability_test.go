package retrievaleval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// TestRetrievability measures cue-dependent forgetting on the REAL vault dump.
//
// The paradigm: cognosis models storage decay (notes.confidence, a read-time
// power law) but not RETRIEVABILITY -- whether an intact, indexed note can be
// reached from a plausible retrieval cue. On this dump confidence carries zero
// variance (every decaying note pinned at 1.0), so any spread in "can this note
// be found?" is purely cue-dependent, not trace decay: a clean natural
// experiment. We probe P(note returned @K | a self-cue drawn from the note's
// OWN text) -- its summary, each chunk's heading path, and each chunk's first
// sentence -- through the full shipped fused retrieval. A note whose own words
// fail to surface it is a retrievability failure with the storage strength held
// constant.
//
// Gated on COGNOSIS_GRAPHTUNE_DSN (an isolated dump, never the live DB) +
// Ollama; skipped in CI. Deterministic given the dump. Writes the per-note
// table to testdata/retrievability_sweep.txt (gitignored).
func TestRetrievability(t *testing.T) {
	dsn := os.Getenv("COGNOSIS_GRAPHTUNE_DSN")
	if dsn == "" {
		t.Skip("set COGNOSIS_GRAPHTUNE_DSN to an isolated real-vault dump")
	}
	ctx := context.Background()

	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	prov := embed.NewOllama(envOr("OLLAMA_URL", "http://localhost:11434"),
		envOr("OLLAMA_MODEL", "nomic-embed-text:v1.5"))
	if err := prov.Health(ctx); err != nil {
		t.Fatalf("ollama health: %v", err)
	}
	e := &query.Engine{Store: s, Providers: []query.ProviderLeg{
		{Provider: prov, Table: "embeddings_ollama_nomic_embed_text_v1_5"}}}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	notes := loadRetrievabilityNotes(ctx, t, pool)
	if len(notes) == 0 {
		t.Fatal("no notes with cues to probe")
	}

	// K levels we score each cue at. topK=8 is DefaultTopK; @1/@3 are stricter
	// slices of the same ranked list.
	const kTop = 8
	kLevels := []int{1, 3, 8}

	type noteScore struct {
		path       string
		conf       float64
		confNull   bool
		maturity   string
		nCues      int
		hitsAtK    map[int]int // K -> count of cues that returned the note @K
	}

	scores := make([]noteScore, 0, len(notes))
	for _, n := range notes {
		ns := noteScore{path: n.path, conf: n.conf, confNull: n.confNull,
			maturity: n.maturity, nCues: len(n.cues), hitsAtK: map[int]int{}}
		for _, cue := range n.cues {
			res, err := e.Run(ctx, cue, query.Options{})
			if err != nil {
				t.Fatal(err)
			}
			top := capK(res, kTop)
			// Best rank at which this note's own path appears (1-based); 0 = absent.
			best := 0
			for i, r := range top {
				if r.Path == n.path {
					best = i + 1
					break
				}
			}
			for _, k := range kLevels {
				if best != 0 && best <= k {
					ns.hitsAtK[k]++
				}
			}
		}
		scores = append(scores, ns)
	}

	// Sort worst-first by retrievability@8, then @3, @1, then path (deterministic).
	rAt := func(ns noteScore, k int) float64 {
		if ns.nCues == 0 {
			return 0
		}
		return float64(ns.hitsAtK[k]) / float64(ns.nCues)
	}
	sort.Slice(scores, func(i, j int) bool {
		a, b := scores[i], scores[j]
		for _, k := range []int{8, 3, 1} {
			ra, rb := rAt(a, k), rAt(b, k)
			if ra != rb {
				return ra < rb
			}
		}
		return a.path < b.path
	})

	// Vault-wide distribution per K.
	stats := map[int]struct{ mean, median float64 }{}
	for _, k := range kLevels {
		vals := make([]float64, len(scores))
		var sum float64
		for i, ns := range scores {
			vals[i] = rAt(ns, k)
			sum += vals[i]
		}
		sort.Float64s(vals)
		med := vals[len(vals)/2]
		if len(vals)%2 == 0 {
			med = (vals[len(vals)/2-1] + vals[len(vals)/2]) / 2
		}
		stats[k] = struct{ mean, median float64 }{sum / float64(len(scores)), med}
	}

	// Hard-to-cue: retrievability@8 below 0.5.
	hard := 0
	for _, ns := range scores {
		if rAt(ns, 8) < 0.5 {
			hard++
		}
	}

	// Confidence variance check: the natural-experiment premise.
	var confVals []float64
	confMin, confMax := 1e18, -1e18
	for _, ns := range scores {
		if ns.confNull {
			continue
		}
		confVals = append(confVals, ns.conf)
		if ns.conf < confMin {
			confMin = ns.conf
		}
		if ns.conf > confMax {
			confMax = ns.conf
		}
	}
	uniformConf := len(confVals) > 0 && confMax-confMin < 1e-9

	var b strings.Builder
	fmt.Fprintf(&b, "cue-dependent retrievability sweep -- REAL vault dump, %d notes, %d total self-cues\n",
		len(scores), totalCues(notes))
	b.WriteString("self-cue = the note's own summary + each chunk heading_path + each chunk first sentence.\n")
	b.WriteString("retrievability@K = fraction of a note's self-cues that return the note in the top-K\n")
	b.WriteString("of the FULL shipped fused retrieval. Storage strength (confidence) is held ~constant,\n")
	b.WriteString("so spread in retrievability is cue-dependent, not trace decay.\n\n")

	fmt.Fprintf(&b, "vault-wide retrievability:\n")
	for _, k := range kLevels {
		fmt.Fprintf(&b, "  @%d: mean %.3f  median %.3f\n", k, stats[k].mean, stats[k].median)
	}
	fmt.Fprintf(&b, "hard-to-cue notes (retrievability@8 < 0.5): %d / %d\n", hard, len(scores))
	if len(confVals) > 0 {
		fmt.Fprintf(&b, "confidence over %d decaying notes: min %.4f  max %.4f  uniform=%t\n",
			len(confVals), confMin, confMax, uniformConf)
	} else {
		b.WriteString("confidence: no non-null values on this dump\n")
	}
	b.WriteString("=> retrievability varies while storage strength does not: decoupled.\n\n")

	fmt.Fprintf(&b, "%-52s %6s %6s %6s %6s %5s %-8s %5s\n",
		"PATH", "r@1", "r@3", "r@8", "conf", "cues", "maturity", "")
	for _, ns := range scores {
		conf := "null"
		if !ns.confNull {
			conf = fmt.Sprintf("%.3f", ns.conf)
		}
		mat := ns.maturity
		if mat == "" {
			mat = "-"
		}
		fmt.Fprintf(&b, "%-52s %6.3f %6.3f %6.3f %6s %5d %-8s\n",
			truncPath(ns.path, 52), rAt(ns, 1), rAt(ns, 3), rAt(ns, 8),
			conf, ns.nCues, mat)
	}

	// Lowest-retrievability@8 note paths (already sorted worst-first).
	b.WriteString("\nlowest-retrievability@8 notes:\n")
	for i := 0; i < len(scores) && i < 5; i++ {
		fmt.Fprintf(&b, "  %d. %s  (r@8=%.3f, conf=%s)\n", i+1, scores[i].path,
			rAt(scores[i], 8), confStr(scores[i].confNull, scores[i].conf))
	}

	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	art := filepath.Join("testdata", "retrievability_sweep.txt")
	if err := os.WriteFile(art, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s\n\n%s", art, b.String())
}

type retrievabilityNote struct {
	path     string
	project  string
	conf     float64
	confNull bool
	maturity string
	cues     []string
}

// loadRetrievabilityNotes pulls each note plus a set of self-cues drawn from its
// own text: the summary, every chunk's heading_path, and every chunk's first
// sentence. Cues are trimmed, de-duplicated, and empties dropped. Deterministic
// order (by path, then chunk ordinal).
func loadRetrievabilityNotes(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []retrievabilityNote {
	t.Helper()
	rows, err := pool.Query(ctx,
		`select path, coalesce(project,''), confidence, coalesce(maturity,''), coalesce(summary,'')
		   from notes order by path`)
	if err != nil {
		t.Fatal(err)
	}
	type meta struct {
		project  string
		conf     float64
		confNull bool
		maturity string
		summary  string
	}
	order := []string{}
	metas := map[string]meta{}
	for rows.Next() {
		var path, project, maturity, summary string
		var conf *float64
		if err := rows.Scan(&path, &project, &conf, &maturity, &summary); err != nil {
			t.Fatal(err)
		}
		m := meta{project: project, maturity: maturity, summary: summary}
		if conf == nil {
			m.confNull = true
		} else {
			m.conf = *conf
		}
		metas[path] = m
		order = append(order, path)
	}
	rows.Close()

	// Chunk-derived cues, in note/ordinal order.
	crows, err := pool.Query(ctx,
		`select note_path, coalesce(heading_path,''), content
		   from chunks order by note_path, ordinal`)
	if err != nil {
		t.Fatal(err)
	}
	headings := map[string][]string{}
	sentences := map[string][]string{}
	for crows.Next() {
		var np, hp, content string
		if err := crows.Scan(&np, &hp, &content); err != nil {
			t.Fatal(err)
		}
		if h := strings.TrimSpace(hp); h != "" {
			headings[np] = append(headings[np], h)
		}
		if fs := firstSentence(content); fs != "" {
			sentences[np] = append(sentences[np], fs)
		}
	}
	crows.Close()

	out := []retrievabilityNote{}
	for _, path := range order {
		m := metas[path]
		var cues []string
		seen := map[string]bool{}
		add := func(c string) {
			c = strings.TrimSpace(c)
			if c == "" || seen[c] {
				return
			}
			seen[c] = true
			cues = append(cues, c)
		}
		add(m.summary)
		for _, h := range headings[path] {
			add(h)
		}
		for _, fs := range sentences[path] {
			add(fs)
		}
		if len(cues) == 0 {
			continue
		}
		out = append(out, retrievabilityNote{
			path: path, project: m.project, conf: m.conf, confNull: m.confNull,
			maturity: m.maturity, cues: cues,
		})
	}
	return out
}

// firstSentence returns the first sentence of a chunk body as a self-cue,
// stripping a leading markdown heading line and capping length. Empty for
// heading-only or whitespace chunks.
func firstSentence(content string) string {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// First sentence terminator or the whole line.
		if i := strings.IndexAny(line, ".!?"); i > 0 {
			line = line[:i+1]
		}
		if len(line) > 240 {
			line = line[:240]
		}
		return strings.TrimSpace(line)
	}
	return ""
}

func totalCues(notes []retrievabilityNote) int {
	n := 0
	for _, x := range notes {
		n += len(x.cues)
	}
	return n
}

func truncPath(p string, n int) string {
	if len(p) <= n {
		return p
	}
	return p[:n-3] + "..."
}

func confStr(isNull bool, v float64) string {
	if isNull {
		return "null"
	}
	return fmt.Sprintf("%.3f", v)
}
