package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs the product-domain git the hook depends on, inside a temp repo.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func runHookPostCommit(t *testing.T) error {
	t.Helper()
	root := newRoot()
	root.SetArgs([]string{"hook", "post-commit"})
	return root.Execute()
}

// TestPostCommitCapturesInMarkedRepo — a commit in a marked repo lands as a
// structured vault entry; the daemon's reconciliation machinery owns indexing
// it (out-of-band by design).
func TestPostCommitCapturesInMarkedRepo(t *testing.T) {
	sandbox := t.TempDir()
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, filepath.Join(sandbox, k))
	}
	vaultDir := filepath.Join(sandbox, "XDG_DATA_HOME", "cognosis", "kb")
	if err := os.MkdirAll(filepath.Join(vaultDir, "entries"), 0o750); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(sandbox, "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cognosis-project"), []byte("hooked-project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "add the entry point")

	t.Chdir(repo)
	if err := runHookPostCommit(t); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(vaultDir, "entries"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v (%v), want exactly one commit capture", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(vaultDir, "entries", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	for _, want := range []string{"add the entry point", "main.go", "project: hooked-project", "category: entry"} {
		if !strings.Contains(content, want) {
			t.Fatalf("capture missing %q:\n%s", want, content)
		}
	}
}

// TestPostCommitUnmarkedIsSilent — no marker: exit 0, nothing written.
func TestPostCommitUnmarkedIsSilent(t *testing.T) {
	sandbox := t.TempDir()
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, filepath.Join(sandbox, k))
	}
	repo := filepath.Join(sandbox, "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")

	t.Chdir(repo)
	if err := runHookPostCommit(t); err != nil {
		t.Fatalf("unmarked hook must exit clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sandbox, "XDG_DATA_HOME", "cognosis", "kb", "entries")); err == nil {
		t.Fatal("unmarked hook wrote into the vault")
	}
}
