package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/embed/embedtest"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

var now = time.Date(2026, 7, 12, 12, 0, 0, 0, time.Local)

func testEngine(t *testing.T) (*Engine, *store.Store, string, context.Context) {
	t.Helper()
	s, _ := storetest.New(t)
	root := t.TempDir()
	for _, d := range []string{"entries", "notes", "reflections", "archive"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	hist := vault.NewHistory(root)
	if err := hist.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	e := &Engine{
		Store:    s,
		Indexer:  &write.Indexer{Store: s}, // no embeddings needed for lifecycle tests
		VaultDir: root,
		Hist:     hist,
	}
	return e, s, root, ctx
}

type noteSpec struct {
	id         string
	confidence string
	maturity   string
	lastReinf  time.Time
	updated    time.Time
	// created and lastExplicit have NO defaults on purpose. created was
	// previously hardcoded to now-48h, which made the legacy-note case
	// (created long ago, no explicit anchor) inexpressible; lastExplicit must
	// stay absent unless a test sets it, or every fixture would silently cover
	// the anchored path and none would cover the created fallback.
	created      time.Time
	lastExplicit time.Time
	status       string
	count        int
	extra        string
	body         string
}

func writeSpec(t *testing.T, root, rel string, sp noteSpec) string {
	t.Helper()
	if sp.id == "" {
		sp.id = uuid.Must(uuid.NewV7()).String()
	}
	if sp.confidence == "" {
		sp.confidence = "0.5"
	}
	if sp.maturity == "" {
		sp.maturity = "seed"
	}
	if sp.lastReinf.IsZero() {
		sp.lastReinf = now.Add(-24 * time.Hour)
	}
	if sp.updated.IsZero() {
		sp.updated = now.Add(-24 * time.Hour)
	}
	if sp.created.IsZero() {
		sp.created = now.Add(-48 * time.Hour)
	}
	if sp.body == "" {
		sp.body = "Body of " + rel + "\n"
	}
	fm := fmt.Sprintf(`---
id: %s
category: concept
created: "%s"
updated: "%s"
confidence: %s
maturity: %s
last_reinforced: "%s"
reinforce_count: %d
sources:
  - "[[capture]]"
`, sp.id, sp.created.Format(vault.TimeLayout), sp.updated.Format(vault.TimeLayout),
		sp.confidence, sp.maturity, sp.lastReinf.Format(vault.TimeLayout), sp.count)
	if !sp.lastExplicit.IsZero() {
		fm += "last_explicit_reinforce: \"" + sp.lastExplicit.Format(vault.TimeLayout) + "\"\n"
	}
	if sp.status != "" {
		fm += "status: " + sp.status + "\n"
	}
	fm += sp.extra
	content := fm + "---\n" + sp.body
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return sp.id
}

func reparse(t *testing.T, root, rel string) *vault.Note {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	n, err := vault.ParseNote(rel, b)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func kinds(r *Report) []string {
	out := make([]string, 0, len(r.Actions))
	for _, a := range r.Actions {
		out = append(out, a.Kind)
	}
	return out
}

// TestTransitions is the state-machine table: every legal transition acts,
// every illegal one is rejected before any write.
func TestTransitions(t *testing.T) {
	cases := []struct {
		name    string
		spec    noteSpec
		ops     func(id string) Options
		want    []string // expected action kinds, in order
		wantErr string   // substring of the rejection, when illegal
	}{
		{
			name: "reinforce raises confidence",
			spec: noteSpec{confidence: "0.5"},
			ops:  func(id string) Options { return Options{Reinforce: []string{id}} },
			want: []string{"reinforced"},
		},
		{
			name: "reinforce promotes seed to developing at threshold",
			spec: noteSpec{confidence: "0.7", maturity: "seed"},
			ops:  func(id string) Options { return Options{Reinforce: []string{id}} },
			want: []string{"reinforced", "promoted"},
		},
		{
			name: "reinforce promotes developing to stable with enough runs",
			spec: noteSpec{confidence: "0.8", maturity: "developing", count: 2},
			ops:  func(id string) Options { return Options{Reinforce: []string{id}} },
			want: []string{"reinforced", "promoted"},
		},
		{
			name: "developing stays without enough runs",
			spec: noteSpec{confidence: "0.8", maturity: "developing", count: 1},
			ops:  func(id string) Options { return Options{Reinforce: []string{id}} },
			want: []string{"reinforced"},
		},
		{
			name: "stale note decays",
			spec: noteSpec{confidence: "0.5", lastReinf: now.Add(-31 * 24 * time.Hour)},
			ops:  func(string) Options { return Options{} },
			want: []string{"decayed"},
		},
		{
			name: "fresh note untouched",
			spec: noteSpec{confidence: "0.5", lastReinf: now.Add(-24 * time.Hour)},
			ops:  func(string) Options { return Options{} },
			want: nil,
		},
		{
			name: "paused note refreshes instead of decaying",
			spec: noteSpec{status: "paused", lastReinf: now.Add(-31 * 24 * time.Hour)},
			ops:  func(string) Options { return Options{} },
			want: []string{"refreshed"},
		},
		{
			name: "graduated note refreshes instead of decaying",
			spec: noteSpec{maturity: "stable", lastReinf: now.Add(-31 * 24 * time.Hour), extra: "graduated_at: \"2026-07-01 00:00:00\"\n"},
			ops:  func(string) Options { return Options{} },
			want: []string{"refreshed"},
		},
		{
			name: "confidence zero archives as faded",
			spec: noteSpec{confidence: "0.1", lastReinf: now.Add(-31 * 24 * time.Hour)},
			ops:  func(string) Options { return Options{} },
			want: []string{"decayed", "archived-faded"},
		},
		{
			name: "abandoned note archives as ancient",
			spec: noteSpec{confidence: "0.5", updated: now.Add(-200 * 24 * time.Hour)},
			ops:  func(string) Options { return Options{} },
			want: []string{"archived-ancient"},
		},
		{
			name: "falsify without reason rejected",
			spec: noteSpec{},
			ops: func(id string) Options {
				return Options{Falsify: map[string]string{id: "  "}}
			},
			wantErr: "without a reason",
		},
		{
			name: "falsify freezes in place",
			spec: noteSpec{},
			ops: func(id string) Options {
				return Options{Falsify: map[string]string{id: "measured otherwise"}}
			},
			want: []string{"falsified"},
		},
		{
			name: "dispute keeps the note live",
			spec: noteSpec{},
			ops: func(id string) Options {
				return Options{Dispute: map[string]string{id: "contradicted by run 14"}}
			},
			want: []string{"disputed"},
		},
		{
			name: "reinforce clears a dispute",
			spec: noteSpec{status: "disputed", extra: "disputed_reason: earlier doubt\ndisputed_at: \"2026-07-10 00:00:00\"\n"},
			ops:  func(id string) Options { return Options{Reinforce: []string{id}} },
			want: []string{"reinforced", "dispute-cleared"},
		},
		{
			name:    "graduate non-stable rejected",
			spec:    noteSpec{maturity: "developing"},
			ops:     func(id string) Options { return Options{Graduate: []string{id}} },
			wantErr: "non-stable",
		},
		{
			name:    "graduate paused rejected",
			spec:    noteSpec{maturity: "stable", status: "paused"},
			ops:     func(id string) Options { return Options{Graduate: []string{id}} },
			wantErr: "paused",
		},
		{
			name:    "graduate disputed rejected",
			spec:    noteSpec{maturity: "stable", status: "disputed"},
			ops:     func(id string) Options { return Options{Graduate: []string{id}} },
			wantErr: "disputed",
		},
		{
			name: "graduate stable canonizes in place",
			spec: noteSpec{maturity: "stable"},
			ops:  func(id string) Options { return Options{Graduate: []string{id}} },
			want: []string{"graduated"},
		},
		{
			name:    "already graduated rejected",
			spec:    noteSpec{maturity: "stable", extra: "graduated_at: \"2026-07-01 00:00:00\"\n"},
			ops:     func(id string) Options { return Options{Graduate: []string{id}} },
			wantErr: "already graduated",
		},
		{
			name: "reinforce and falsify same note rejected as contradiction",
			spec: noteSpec{},
			ops: func(id string) Options {
				return Options{Reinforce: []string{id}, Falsify: map[string]string{id: "but also wrong?"}}
			},
			wantErr: "contradictory",
		},
		{
			name: "unknown target rejected up front",
			spec: noteSpec{},
			ops: func(string) Options {
				return Options{Reinforce: []string{"no-such-note"}}
			},
			wantErr: "no live decaying note",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _, root, ctx := testEngine(t)
			id := writeSpec(t, root, "notes/subject.md", c.spec)
			r, err := e.Run(ctx, c.ops(id))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := kinds(r)
			if len(got) != len(c.want) {
				t.Fatalf("actions = %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("actions = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// TestVerifyAdvisories -- with Verify set and a retrieval engine wired, a
// terminal move's report carries related-context lines; without Verify the
// behavior is unchanged.
func TestVerifyAdvisories(t *testing.T) {
	e, s, root, ctx := testEngine(t)

	// Wire retrieval: a stub provider and a related note that shares the
	// target's vocabulary.
	stub := embedtest.New()
	table := embed.TableSlug(stub.Name(), stub.Model())
	if err := s.EnsureProvider(ctx, stub.Name(), stub.Model(), table, stub.Dim, true); err != nil {
		t.Fatal(err)
	}
	e.Indexer = &write.Indexer{Store: s, Provider: stub, Table: table}
	e.Query = &query.Engine{Store: s, Providers: []query.ProviderLeg{{Provider: stub, Table: table}}}

	id := writeSpec(t, root, "notes/target.md", noteSpec{
		maturity: "stable",
		body:     "The retention window governs the sweep cadence.\n",
	})
	writeSpec(t, root, "notes/related.md", noteSpec{
		body: "Contradiction: the retention window does not govern the sweep cadence.\n",
	})
	// Index both so retrieval can see them.
	for _, rel := range []string{"notes/target.md", "notes/related.md"} {
		n := reparse(t, root, rel)
		if err := e.Indexer.Index(ctx, n, write.FileMeta{Mtime: now, Size: 1, Blake3: rel}); err != nil {
			t.Fatal(err)
		}
	}

	r, err := e.Run(ctx, Options{Now: now, Graduate: []string{id}, Verify: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	foundAdvisory := false
	for _, a := range r.Actions {
		if a.Kind == "related-context" && strings.Contains(a.Detail, "related") {
			foundAdvisory = true
		}
	}
	if !foundAdvisory {
		t.Fatalf("verify produced no related-context advisory: %v", kinds(r))
	}

	// Without Verify: same run, no advisory.
	r, err = e.Run(ctx, Options{Now: now, Graduate: []string{id}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Actions {
		if a.Kind == "related-context" {
			t.Fatal("advisory emitted without verify")
		}
	}
}

// TestFalsifiedIsLifecycleTerminal -- a falsified note is retained but inert:
// naming it in a later run is a typo worth surfacing.
func TestFalsifiedIsLifecycleTerminal(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	id := writeSpec(t, root, "notes/wrong.md", noteSpec{})
	if _, err := e.Run(ctx, Options{Now: now, Falsify: map[string]string{id: "disproven"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(ctx, Options{Now: now, Reinforce: []string{id}}); err == nil {
		t.Fatal("reinforcing a falsified note must be rejected")
	}
	// And it never decays.
	r, err := e.Run(ctx, Options{Now: now.Add(60 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Actions) != 0 {
		t.Fatalf("falsified note acted on: %v", kinds(r))
	}
}

// TestDryRunWritesNothing -- same report, zero writes: files, DB, log.md, and
// history are all byte-identical after a dry run.
