package vault

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testHistory(t *testing.T) (*History, string, context.Context) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "entries"), 0o755); err != nil {
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

	if err := os.WriteFile(p, []byte("version one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "write v1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("version two\n"), 0o644); err != nil {
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
	if err != nil || string(b) != "version one\n" {
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

	if err := os.WriteFile(a, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.CommitAll(ctx, "add a"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("two\n"), 0o644); err != nil {
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
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.CommitAll(ctx, "both"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("more secret content\n"), 0o644); err != nil {
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
