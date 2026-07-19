package vault

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCommitAllIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	h := NewHistory(dir)
	if err := h.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"entries", "notes", ".obsidian"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	// A real note plus the kinds of file other tools leave lying around.
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("entries/real.md", "a genuine note\n")
	write(".obsidian/workspace.json", `{"panes":1}`)
	write("history.md", "generated dashboard\n")
	write("scratch.md", "a stray file at the vault root\n")

	if err := h.CommitAll(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	files := committedFiles(t, dir)
	if !files["entries/real.md"] {
		t.Errorf("the note was not committed: %v", files)
	}
	for _, foreign := range []string{".obsidian/workspace.json", "history.md", "scratch.md"} {
		if files[foreign] {
			t.Errorf("%s was committed; the vault directory is shared and only Cognosis's own paths belong in its history", foreign)
		}
	}
}

// A change to nothing but foreign files must produce no commit at all. This is
// what stops a sweep manufacturing a commit whose message describes work it
// does not contain.
func TestCommitAllNoOpsOnForeignChurnAlone(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	h := NewHistory(dir)
	if err := h.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "entries"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entries", "real.md"), []byte("note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	before := commitCount(t, dir)

	if err := os.MkdirAll(filepath.Join(dir, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".obsidian", "workspace.json"), []byte(`{"panes":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "watch: entries/real.md edited out-of-band"); err != nil {
		t.Fatal(err)
	}
	if got := commitCount(t, dir); got != before {
		t.Errorf("commits %d -> %d: editor churn alone produced a commit, and its message names a note it does not contain", before, got)
	}
}

// Deleting a note must still be recorded: a pathspec `git add -A` stages
// removals inside those paths, and losing that would make erasure invisible.
func TestCommitAllRecordsDeletions(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	h := NewHistory(dir)
	if err := h.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "entries"), 0o750); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "entries", "doomed.md")
	if err := os.WriteFile(abs, []byte("note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "add"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "remove"); err != nil {
		t.Fatal(err)
	}
	if committedFiles(t, dir)["entries/doomed.md"] {
		t.Error("deletion was not recorded; the file is still present at HEAD")
	}
}

// EnsureRepo seeds .gitignore only when it creates the repo -- an existing
// vault must never be written to, since the early return is what makes this
// safe to call on every boot.
func TestEnsureRepoSeedsGitignoreOnlyOnCreate(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	h := NewHistory(dir)
	if err := h.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(dir, ".gitignore")
	b, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("no .gitignore seeded on create: %v", err)
	}
	for _, want := range []string{"history.md", ".obsidian/workspace.json"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("seeded .gitignore omits %s:\n%s", want, b)
		}
	}
	// The rest of .obsidian is real configuration.
	if strings.Contains(string(b), "\n.obsidian/\n") {
		t.Errorf("seeded .gitignore excludes all of .obsidian, discarding real user config:\n%s", b)
	}

	// Second call on an existing repo must not rewrite it.
	if err := os.WriteFile(gi, []byte("# operator edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(gi)
	if string(after) != "# operator edited\n" {
		t.Errorf("EnsureRepo overwrote an existing vault's .gitignore:\n%s", after)
	}
}

func committedFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	files := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			files[l] = true
		}
	}
	return files
}

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// TestCommitAllExcludesPreStagedForeignFiles -- the `git add` was already scoped,
// but the `git commit` was not, and git commits whatever is in the index. So a
// file staged by anything else in the vault repo -- a human running `git add`, an
// operation interrupted midway -- was swept into the next note's commit under a
// message describing only that note.
//
// The scoped add cannot prevent this: the foreign file is staged before
// CommitAll runs, so there is nothing for the add to exclude.
func TestCommitAllExcludesPreStagedForeignFiles(t *testing.T) {
	h, dir, ctx := testHistory(t)

	// Something else stages a file Cognosis does not own.
	foreign := filepath.Join(dir, "notes-for-me.txt")
	if err := os.WriteFile(foreign, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.git(ctx, "add", "--", "notes-for-me.txt"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "entries", "note.md"), []byte("note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "entries/note.md written"); err != nil {
		t.Fatal(err)
	}

	files, err := h.git(ctx, "show", "--name-only", "--format=", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files, "notes-for-me.txt") {
		t.Errorf("a pre-staged foreign file rode along in a note commit:\n%s", files)
	}
	if !strings.Contains(files, "entries/note.md") {
		t.Errorf("the note itself was not committed:\n%s", files)
	}

	// And it must still be staged afterwards: scoping the commit must leave the
	// other party's work alone, not silently discard it.
	staged, err := h.git(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(staged, "notes-for-me.txt") {
		t.Errorf("the foreign file left the index; scoping must not discard another party's staged work: %q", staged)
	}
}

// TestCommitAllDoesNotSwallowAConcurrentWrite -- vault file writes are not under
// gitIndexMu; only the git calls are. So another writer's file can land on disk
// between this commit's stage step and the commit itself.
//
// The commit must be a snapshot taken at the stage step, not a live read of the
// working tree. `git commit -- <paths>` is a partial commit and reads the
// worktree, which swallows the late file into *this* message and leaves its own
// writer with nothing to record -- the write is misattributed and its history
// entry is lost outright. Losing a version silently is worse than any churn,
// because `vault restore` is the thing that was supposed to recover it.
//
// The hook makes the interleaving deterministic. Every earlier test drove
// CommitAll start to finish, which is exactly why the partial-commit regression
// passed review and the suite.
func TestCommitAllDoesNotSwallowAConcurrentWrite(t *testing.T) {
	h, dir, ctx := testHistory(t)

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "entries", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "a0")
	write("b.md", "b0")
	if err := h.CommitAll(ctx, "base"); err != nil {
		t.Fatal(err)
	}

	// A's write is staged; B lands in the window before A's commit is written.
	write("a.md", "a1")
	testHookAfterStage = func() { write("b.md", "b1") }
	t.Cleanup(func() { testHookAfterStage = nil })
	if err := h.CommitAll(ctx, "write_note: entries/a.md"); err != nil {
		t.Fatal(err)
	}
	testHookAfterStage = nil

	files, err := h.git(ctx, "show", "--name-only", "--format=", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files, "b.md") {
		t.Errorf("a late write was swept into another note's commit:\n%s", files)
	}

	// The load-bearing half: B's content must still be pending, so B's own
	// CommitAll records it. A partial commit consumes it here and B's call then
	// finds nothing to do, losing the version with no error anywhere.
	if err := h.CommitAll(ctx, "write_note: entries/b.md"); err != nil {
		t.Fatal(err)
	}
	got, err := h.git(ctx, "show", "HEAD:entries/b.md")
	if err != nil {
		t.Fatalf("b.md never reached history -- the write was lost: %v", err)
	}
	if strings.TrimSpace(got) != "b1" {
		t.Errorf("b.md at HEAD = %q, want b1", strings.TrimSpace(got))
	}

	// And no owned path may be left dirty: that would block PurgePath.
	status, err := h.git(ctx, "status", "--porcelain", "--", "entries")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(status) != "" {
		t.Errorf("owned paths left dirty after commit: %q", status)
	}
}
