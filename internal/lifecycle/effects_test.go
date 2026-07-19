package lifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/write"
)

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

// TestOneCommitPerRun -- a run with several actions is one revertible unit.
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

// TestRevertRunRestores -- reverting the run's commit and re-reconciling
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
		cmd := exec.CommandContext(t.Context(), "git", args...)
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

// TestConcurrentRunRejected -- the advisory lock turns a concurrent second
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

// TestCitationRefresh -- a stale note cited by a recently-updated note
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

// TestLogAppended -- a real run appends its report to the vault's log.md.
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
	cmd := exec.CommandContext(t.Context(), "git", "rev-list", "--count", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return 0 // no commits yet
	}
	n := 0
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

// TestPassiveRefreshBudgetExhausts is the centrepiece: citation keeps a note
// alive only so far past the last explicit reinforce.
//
// TestCitationRefresh runs exactly one compile pass and so cannot express this
// -- the unbounded behaviour is only visible across many. Before the budget
// existed this loop refreshed all eleven times and confidence never moved.
