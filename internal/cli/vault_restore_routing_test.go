package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/vault"
)

// restoreFixture builds a two-version vault history for entries/routing.md and
// returns the vault dir plus the ref of the *first* version. The file is left
// holding the second version, so a successful restore is observable as the
// first version's text reappearing and a failed one as it not appearing.
func restoreFixture(t *testing.T) (dir, firstRef string) {
	t.Helper()
	ctx := context.Background()
	dir = t.TempDir()
	h := vault.NewHistory(dir)
	if err := h.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "entries"), 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		// The id is present because a restore re-validates the note against the
		// current contract before writing it -- unlike a write_note fixture, where
		// the daemon mints one.
		note := "---\nid: 01920000-0000-7000-8000-00000000c0de\n" +
			"category: entry\ncreated: \"2026-07-12 09:00:00\"\n" +
			"updated: \"2026-07-12 09:00:00\"\n---\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, "entries", "routing.md"), []byte(note), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("FIRST VERSION")
	if err := h.CommitAll(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	lines, err := h.Log(ctx, "entries/routing.md")
	if err != nil || len(lines) != 1 {
		t.Fatalf("log after v1 = %v, %v", lines, err)
	}
	firstRef = strings.Fields(lines[0])[0]

	write("SECOND VERSION")
	if err := h.CommitAll(ctx, "v2"); err != nil {
		t.Fatal(err)
	}
	return dir, firstRef
}

func noteBody(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "entries", "routing.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// restoreCmd is a bare cobra command carrying the context and captured streams
// runRestore needs, without going through config.Load or the root tree.
func restoreCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := &cobra.Command{Use: "restore"}
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetContext(t.Context())
	return cmd, &out, &errb
}

// TestRestoreRefusesWhenTheDaemonCannotBeReached -- a daemon owns the database
// but its MCP door does not answer, so the restore must refuse rather than fall
// back to a direct write. The moment the owner of your data is misbehaving is
// the moment a racing write is least defensible, and the refusal has to name
// --force-local or the operator has no way forward.
//
// The instance lock is held by the test itself, which is exactly what
// probeDaemon reads: no daemon process is needed to reproduce the branch.
// A private database (NewDB) keeps concurrent packages off that lock.
func TestRestoreRefusesWhenTheDaemonCannotBeReached(t *testing.T) {
	s, dsn := storetest.NewDB(t)
	release, err := s.AcquireAdvisory(context.Background(), store.LockInstance)
	if err != nil {
		t.Fatalf("instance lock held on a private database; nothing else can reach it: %v", err)
	}
	defer release()

	dir, firstRef := restoreFixture(t)
	// Port 1 on loopback: nothing listens, so the tool call cannot succeed.
	cfg := &config.Config{DSN: dsn, KBPath: dir, BindAddress: "127.0.0.1:1"}
	cmd, _, _ := restoreCmd(t)

	err = runRestore(cmd, cfg, "entries/routing.md", firstRef, false)
	if err == nil {
		t.Fatal("restore succeeded while the owning daemon was unreachable; a direct write here is the race the routing exists to avoid")
	}
	if !strings.Contains(err.Error(), "--force-local") {
		t.Errorf("refusal does not name the escape hatch, leaving the operator stuck: %v", err)
	}
	if got := noteBody(t, dir); !strings.Contains(got, "SECOND VERSION") {
		t.Errorf("the file was written despite the refusal:\n%s", got)
	}
}

// TestRestoreForceLocalWritesUnderALiveDaemonAndWarns -- --force-local is the
// documented way out of the refusal above, so it must actually write while a
// daemon owns the database, and it must say on stderr that it bypassed the
// per-path lock. A flag that warns but does not write, or writes but does not
// warn, is a worse outcome than not having it.
func TestRestoreForceLocalWritesUnderALiveDaemonAndWarns(t *testing.T) {
	s, dsn := storetest.NewDB(t)
	release, err := s.AcquireAdvisory(context.Background(), store.LockInstance)
	if err != nil {
		t.Fatalf("instance lock held on a private database; nothing else can reach it: %v", err)
	}
	defer release()

	dir, firstRef := restoreFixture(t)
	cfg := &config.Config{DSN: dsn, KBPath: dir, BindAddress: "127.0.0.1:1"}
	cmd, out, errb := restoreCmd(t)

	if err := runRestore(cmd, cfg, "entries/routing.md", firstRef, true); err != nil {
		t.Fatalf("--force-local refused to write: %v", err)
	}
	if got := noteBody(t, dir); !strings.Contains(got, "FIRST VERSION") {
		t.Errorf("--force-local did not restore the file:\n%s", got)
	}
	if !strings.Contains(errb.String(), "force-local") || !strings.Contains(errb.String(), "lock") {
		t.Errorf("stderr does not warn that the per-path lock was bypassed: %q", errb.String())
	}
	if strings.Contains(out.String(), "via the running daemon") {
		t.Errorf("--force-local claimed to have routed through the daemon: %q", out.String())
	}
}

// TestRestoreWritesDirectlyWithNoDaemon -- the unchanged case, pinned because it
// is the one the other two branches are measured against. Nothing holds the
// instance lock, so the direct write is safe by construction and must happen
// without the daemon warning. The lock-free premise only actually holds on a
// private database (NewDB): on the shared one, any concurrent package booting
// a daemon in a test makes the probe see daemonPresent.
func TestRestoreWritesDirectlyWithNoDaemon(t *testing.T) {
	s, dsn := storetest.NewDB(t)
	release, err := s.AcquireAdvisory(context.Background(), store.LockInstance)
	if err != nil {
		t.Fatalf("instance lock held on a private database; nothing else can reach it: %v", err)
	}
	release()

	dir, firstRef := restoreFixture(t)
	cfg := &config.Config{DSN: dsn, KBPath: dir, BindAddress: "127.0.0.1:1"}
	cmd, out, errb := restoreCmd(t)

	if err := runRestore(cmd, cfg, "entries/routing.md", firstRef, false); err != nil {
		t.Fatalf("restore with no daemon failed: %v", err)
	}
	if got := noteBody(t, dir); !strings.Contains(got, "FIRST VERSION") {
		t.Errorf("direct restore did not land:\n%s", got)
	}
	if strings.Contains(errb.String(), "force-local") {
		t.Errorf("warned about bypassing a lock nothing was holding: %q", errb.String())
	}
	if strings.Contains(out.String(), "via the running daemon") {
		t.Errorf("claimed to route through a daemon that does not exist: %q", out.String())
	}
}
