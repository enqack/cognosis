package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/auth"
	"github.com/enqack/cognosis/internal/cogerr"
)

// TestCommandTreeComplete asserts the full command tree exists (shape before
// behavior).
func TestCommandTreeComplete(t *testing.T) {
	root := newRoot()

	want := [][]string{
		{"start"}, {"stop"}, {"status"},
		{"embeddings", "migrate"}, {"embeddings", "status"}, {"embeddings", "prune"},
		{"schema", "status"},
		{"note", "delete"},
		{"vault", "history"}, {"vault", "restore"},
		{"token", "create"}, {"token", "revoke"}, {"token", "list"}, {"token", "prune"},
		{"config", "get"}, {"config", "set"},
		{"context", "inject"},
	}

	for _, path := range want {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Errorf("command %v missing: %v", path, err)
			continue
		}
		if got := cmd.Name(); got != path[len(path)-1] {
			t.Errorf("Find(%v) resolved to %q", path, got)
		}
	}
}

// Every command in the tree is implemented for real; the stub-phase test
// retired with the last stub.

// TestTokenCreateRejectsReservedName — `token create local` used to succeed,
// which pushed the daemon onto a fallback name and made the documented remedy
// `cognosis token revoke local` revoke the operator's token rather than the
// daemon's. The rejection lives in cobra's Args, not RunE, which is what lets
// this run with no database: RunE calls withStore immediately.
func TestTokenCreateRejectsReservedName(t *testing.T) {
	root := newRoot()
	root.SetArgs([]string{"token", "create", auth.LocalTokenName})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	if err == nil {
		t.Fatal("token create local succeeded; it would hijack the daemon's own name")
	}
	if !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("kind = %v, want Validation", err)
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("error does not say why: %v", err)
	}
}

// TestTokenCreateRejectsMalformedName covers the charset rule at the CLI
// boundary. The name is emitted as an unquoted token=<name> log attribute, so a
// space or quote would break parsing of the attribution attribute itself.
func TestTokenCreateRejectsMalformedName(t *testing.T) {
	for _, bad := range []string{"my token", "Desktop", "a=b", ""} {
		root := newRoot()
		root.SetArgs([]string{"token", "create", bad})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		if err := root.Execute(); err == nil {
			t.Errorf("token create %q succeeded", bad)
		}
	}
}
