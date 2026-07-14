package lifecycle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
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
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
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
	status     string
	count      int
	extra      string
	body       string
}

func writeSpec(t *testing.T, root, rel string, sp noteSpec) string {
	t.Helper()
	if sp.id == "" {
		sp.id = uuid.NewString()
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
`, sp.id, now.Add(-48*time.Hour).Format(vault.TimeLayout), sp.updated.Format(vault.TimeLayout),
		sp.confidence, sp.maturity, sp.lastReinf.Format(vault.TimeLayout), sp.count)
	if sp.status != "" {
		fm += "status: " + sp.status + "\n"
	}
	fm += sp.extra
	content := fm + "---\n" + sp.body
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
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
	var out []string
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

// TestVerifyAdvisories — with Verify set and a retrieval engine wired, a
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

// TestFalsifiedIsLifecycleTerminal — a falsified note is retained but inert:
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

// TestDryRunWritesNothing — same report, zero writes: files, DB, log.md, and
// history are all byte-identical after a dry run.
func TestDryRunWritesNothing(t *testing.T) {
	e, s, root, ctx := testEngine(t)
	id := writeSpec(t, root, "notes/subject.md", noteSpec{lastReinf: now.Add(-31 * 24 * time.Hour)})
	_ = id

	before, err := os.ReadFile(filepath.Join(root, "notes", "subject.md"))
	if err != nil {
		t.Fatal(err)
	}
	statesBefore, err := s.FileStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commitsBefore := gitCommitCount(t, root)

	r, err := e.Run(ctx, Options{Now: now, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Actions) == 0 {
		t.Fatal("dry run should still report the decay it would apply")
	}

	after, err := os.ReadFile(filepath.Join(root, "notes", "subject.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("dry run modified a file")
	}
	statesAfter, err := s.FileStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statesBefore) != len(statesAfter) {
		t.Fatal("dry run modified the DB")
	}
	if _, err := os.Stat(filepath.Join(root, "log.md")); err == nil {
		t.Fatal("dry run appended to log.md")
	}
	if gitCommitCount(t, root) != commitsBefore {
		t.Fatal("dry run created a history commit")
	}
}

// TestOneCommitPerRun — a run with several actions is one revertible unit.
func TestOneCommitPerRun(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/a.md", noteSpec{lastReinf: now.Add(-31 * 24 * time.Hour)})
	writeSpec(t, root, "notes/b.md", noteSpec{lastReinf: now.Add(-31 * 24 * time.Hour)})
	before := gitCommitCount(t, root)
	r, err := e.Run(ctx, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Actions) != 2 {
		t.Fatalf("actions = %v", kinds(r))
	}
	if got := gitCommitCount(t, root); got != before+1 {
		t.Fatalf("commits = %d, want %d (one per run)", got, before+1)
	}
}

// TestRevertRunRestores — reverting the run's commit and re-reconciling
// restores the pre-run state end to end (the vault-history recovery promise).
func TestRevertRunRestores(t *testing.T) {
	e, s, root, ctx := testEngine(t)
	id := writeSpec(t, root, "notes/subject.md", noteSpec{confidence: "0.5", lastReinf: now.Add(-31 * 24 * time.Hour)})

	// Baseline commit so the run's commit has a parent to revert to.
	if err := e.Hist.CommitAll(ctx, "baseline"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(ctx, Options{Now: now}); err != nil {
		t.Fatal(err)
	}
	n := reparse(t, root, "notes/subject.md")
	if got := fmt.Sprint(n.Frontmatter["confidence"]); got != "0.4" {
		t.Fatalf("confidence after decay = %v", got)
	}

	// Revert the compile commit (product-domain git, inside the vault repo).
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("revert", "--no-edit", "HEAD")

	// Reconcile the reverted tree back into the index the same way the
	// watcher's boot pass does.
	reverted := reparse(t, root, "notes/subject.md")
	info, _ := os.Stat(filepath.Join(root, "notes", "subject.md"))
	if err := e.Indexer.Index(ctx, reverted, write.FileMeta{Mtime: info.ModTime(), Size: info.Size(), Blake3: "reverted"}); err != nil {
		t.Fatal(err)
	}

	if got := fmt.Sprint(reverted.Frontmatter["confidence"]); got != "0.5" {
		t.Fatalf("confidence after revert = %v, want 0.5", got)
	}
	row, err := s.GetNote(ctx, "notes/subject.md")
	if err != nil {
		t.Fatal(err)
	}
	if row.Confidence == nil || *row.Confidence != 0.5 {
		t.Fatalf("DB confidence after revert+reconcile = %v", row.Confidence)
	}
	_ = id
}

// TestConcurrentRunRejected — the advisory lock turns a concurrent second
// call into an explicit already-in-progress error.
func TestConcurrentRunRejected(t *testing.T) {
	e, s, _, ctx := testEngine(t)
	release, err := s.AcquireAdvisory(ctx, store.LockCompile)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = e.Run(ctx, Options{Now: now})
	if !cogerr.Is(err, cogerr.Conflict) {
		t.Fatalf("err = %v, want Conflict", err)
	}
}

// TestCitationRefresh — a stale note cited by a recently-updated note
// refreshes instead of decaying.
func TestCitationRefresh(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/theory.md", noteSpec{lastReinf: now.Add(-31 * 24 * time.Hour)})
	// A fresh entry citing it.
	writeSpec(t, root, "notes/citer.md", noteSpec{
		updated: now.Add(-time.Hour),
		body:    "Still building on [[theory]] today.\n",
	})
	r, err := e.Run(ctx, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Actions {
		if a.Note == "theory" && a.Kind == "refreshed" {
			return
		}
		if a.Note == "theory" && a.Kind == "decayed" {
			t.Fatal("cited note decayed instead of refreshing")
		}
	}
	t.Fatalf("no refresh recorded for the cited note: %v", kinds(r))
}

// TestLogAppended — a real run appends its report to the vault's log.md.
func TestLogAppended(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	writeSpec(t, root, "notes/a.md", noteSpec{lastReinf: now.Add(-31 * 24 * time.Hour)})
	if _, err := e.Run(ctx, Options{Now: now}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "decayed") {
		t.Fatalf("log.md missing the run report:\n%s", b)
	}
}

func gitCommitCount(t *testing.T, root string) int {
	t.Helper()
	cmd := exec.Command("git", "rev-list", "--count", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return 0 // no commits yet
	}
	n := 0
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}
