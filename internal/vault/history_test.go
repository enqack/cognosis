package vault

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testHistory(t *testing.T) (*History, string, context.Context) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "entries"), 0o750); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h := NewHistory(root)
	if err := h.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	return h, root, ctx
}

func TestRestoreRoundTrip(t *testing.T) {
	h, root, ctx := testHistory(t)
	p := filepath.Join(root, "entries", "a.md")

	if err := os.WriteFile(p, []byte(restorableNote("version one")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "write v1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(restorableNote("version two")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "write v2"); err != nil {
		t.Fatal(err)
	}

	lines, err := h.Log(ctx, "entries/a.md")
	if err != nil || len(lines) != 2 {
		t.Fatalf("log = %v (%v)", lines, err)
	}
	// Oldest commit hash is the first field of the last line.
	oldRef := strings.Fields(lines[len(lines)-1])[0]

	// Byte-identical recovery of the prior version.
	if err := h.Restore(ctx, oldRef, "entries/a.md"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != restorableNote("version one") {
		t.Fatalf("restored content = %q (%v)", b, err)
	}

	// History moved forward: the restore is a third commit, not a rewrite.
	lines, err = h.Log(ctx, "entries/a.md")
	if err != nil || len(lines) != 3 {
		t.Fatalf("log after restore = %v (%v)", lines, err)
	}
}

func TestLogAllAndDashboard(t *testing.T) {
	h, root, ctx := testHistory(t)
	a := filepath.Join(root, "entries", "a.md")
	b := filepath.Join(root, "entries", "b.md")

	if err := os.WriteFile(a, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "add a"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "add b"); err != nil {
		t.Fatal(err)
	}

	commits, err := h.LogAll(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("LogAll = %d commits, want 2", len(commits))
	}
	// Newest first, with the touched path parsed.
	if commits[0].Subject != "add b" || len(commits[0].Paths) != 1 || commits[0].Paths[0] != "entries/b.md" {
		t.Fatalf("newest commit = %+v", commits[0])
	}

	// The dashboard lands at the vault root, is reserved (not indexed), and
	// carries a copy-paste restore command with the real short hash.
	if err := h.WriteDashboard(ctx); err != nil {
		t.Fatal(err)
	}
	dash, err := os.ReadFile(filepath.Join(root, "history.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !IsReserved("history.md") {
		t.Fatal("history.md should be a reserved generated file")
	}
	short := commits[0].Hash[:12]
	wantCmd := "cognosis vault restore entries/b.md --at " + short
	if !strings.Contains(string(dash), wantCmd) {
		t.Fatalf("dashboard missing restore command %q:\n%s", wantCmd, dash)
	}
}

func TestPurgePathErasesHistory(t *testing.T) {
	h, root, ctx := testHistory(t)
	secret := filepath.Join(root, "entries", "secret.md")
	keep := filepath.Join(root, "entries", "keep.md")
	for path, content := range map[string]string{
		secret: "the secret content\n",
		keep:   "unrelated survivor\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.CommitAll(ctx, "both"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("more secret content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "update secret"); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "remove secret file"); err != nil {
		t.Fatal(err)
	}
	if err := h.PurgePath(ctx, "entries/secret.md"); err != nil {
		t.Fatal(err)
	}

	// No commit references the path anymore…
	if lines, err := h.Log(ctx, "entries/secret.md"); err != nil || len(lines) != 0 {
		t.Fatalf("purged path still has history: %v (%v)", lines, err)
	}
	// …and the content is not recoverable from any object in the repo.
	out, err := h.git(ctx, "rev-list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range strings.Fields(out) {
		if content, err := h.Show(ctx, ref, "entries/secret.md"); err == nil {
			t.Fatalf("secret recoverable at %s: %q", ref, content)
		}
	}
	// The survivor's history is intact.
	if lines, err := h.Log(ctx, "entries/keep.md"); err != nil || len(lines) == 0 {
		t.Fatalf("survivor lost its history: %v (%v)", lines, err)
	}
	b, err := os.ReadFile(keep)
	if err != nil || string(b) != "unrelated survivor\n" {
		t.Fatalf("survivor content = %q (%v)", b, err)
	}
}

// restorableNote is a minimal note that satisfies the current contract, so
// restore tests exercise the real path rather than bare text a vault would
// never hold (and that reconciliation would refuse to index anyway).
// The id is a fixed literal, not freshly minted: a note keeps its id across
// versions, so a round-trip comparison must not see it change.
func restorableNote(body string) string {
	return "---\nid: 01920000-0000-7000-8000-0000000000b1\ncategory: entry\n" +
		"created: \"2026-07-12 09:00:00\"\nupdated: \"2026-07-12 09:00:00\"\n---\n" + body + "\n"
}

// TestRestoreRefusesContractViolation is the guard for a silent failure: a
// commit predating a contract rule (a v4 id, here) would otherwise restore
// "successfully", land on disk, and then be refused by reconciliation — a note
// that exists and cannot be retrieved, reported as success.
func TestRestoreRefusesContractViolation(t *testing.T) {
	h, root, ctx := testHistory(t)
	p := filepath.Join(root, "entries", "old.md")

	// A note as it would have been written before ids were required to be v7.
	legacy := "---\nid: " + uuid.Must(uuid.NewRandom()).String() + "\ncategory: entry\n" +
		"created: \"2026-07-12 09:00:00\"\nupdated: \"2026-07-12 09:00:00\"\n---\nlegacy\n"
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "legacy note"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(restorableNote("current")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "conforming note"); err != nil {
		t.Fatal(err)
	}

	lines, err := h.Log(ctx, "entries/old.md")
	if err != nil || len(lines) != 2 {
		t.Fatalf("log = %v (%v)", lines, err)
	}
	oldRef := strings.Fields(lines[len(lines)-1])[0]

	err = h.Restore(ctx, oldRef, "entries/old.md")
	if err == nil {
		t.Fatal("restore of a contract-violating commit succeeded; it would write a note the index refuses")
	}
	if !strings.Contains(err.Error(), "UUIDv7") {
		t.Errorf("error should name the violated rule, got: %v", err)
	}

	// The refusal must not have clobbered the current file.
	b, readErr := os.ReadFile(p)
	if readErr != nil || !strings.Contains(string(b), "current") {
		t.Errorf("refused restore modified the working file: %q (%v)", b, readErr)
	}
	// And the content is still reachable for inspection.
	if _, err := h.Show(ctx, oldRef, "entries/old.md"); err != nil {
		t.Errorf("Show should still read the old content: %v", err)
	}
}

// TestCommitAllIgnoresForeignFiles — the vault directory is shared with
// whatever the operator runs in it, and CommitAll used `git add -A`. On a real
// vault that put editor state into 22% of commits, and produced commits whose
// "watch: <note> edited out-of-band" subject named a note the commit did not
// contain.
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

// EnsureRepo seeds .gitignore only when it creates the repo — an existing
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
