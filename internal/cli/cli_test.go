package cli

import (
	"testing"
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
		{"token", "create"}, {"token", "revoke"}, {"token", "list"},
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
