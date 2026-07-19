package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

// TestLifecycleAndPipelineShareOneLock — the cross-package lost update.
//
// lifecycle.rewrite writes vault files directly rather than through the
// Pipeline, so before this fix the two writers serialized against themselves
// and not against each other. Pipeline.Edit is a read-modify-write, which makes
// the gap consequential: Edit reads at T0, a compile run reinforces and writes
// at T1, Edit writes its stale copy at T2 and the reinforce is gone. Both
// report success, and reconciliation cannot repair it because the file and the
// index agree on the wrong value.
//
// The unit tests in write/pipeline_test.go structurally cannot catch this —
// they race Edit against Edit, which was always serialized. This races Edit
// against the lifecycle writer, which is the pair that was not.
func TestLifecycleAndPipelineShareOneLock(t *testing.T) {
	e, s, root, ctx := testEngine(t)

	locks := write.NewPathLocks()
	e.Locks = locks
	p := write.NewPipeline(e.Indexer, root, e.Hist, nil)
	p.Locks = locks // the wiring under test: one instance, both writers

	const rel = "notes/contested.md"
	id := "019f7000-0000-7000-8000-000000000001"
	body := "---\nid: " + id + "\ncategory: concept\nconfidence: 0.5\nmaturity: seed\n" +
		"created: \"2026-07-12 09:00:00\"\nupdated: \"2026-07-12 09:00:00\"\n" +
		"last_reinforced: \"2026-07-12 09:00:00\"\nreinforce_count: 0\n" +
		"sources:\n  - \"[[x]]\"\nmarker: original\n---\nbody text\n"
	if err := os.WriteFile(filepath.Join(root, "notes", "contested.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := vault.ParseNote(rel, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Indexer.Index(ctx, n, write.FileMeta{Mtime: time.Now(), Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	// Concurrently: lifecycle stamps canonical frontmatter, the agent edits the
	// body. Neither touches the other's bytes, so with one shared lock both
	// survive; with two, whichever writes last reverts the other.
	var wg sync.WaitGroup
	wg.Add(2)
	var lcErr, edErr error
	go func() {
		defer wg.Done()
		// Parsed from the ORIGINAL bytes and written later, exactly as Compile
		// does: vault.Walk once, rewrite much later. No re-read.
		stale, _ := vault.ParseNote(rel, []byte(body))
		stale.SetFM("confidence", "0.9")
		lcErr = e.rewrite(ctx, stale, rel)
	}()
	go func() {
		defer wg.Done()
		edErr = p.Edit(ctx, rel, "marker: original", "marker: edited")
	}()
	wg.Wait()
	if edErr != nil {
		t.Fatalf("edit: %v", edErr)
	}
	// Two legal outcomes, and the illegal one is silence. Either lifecycle won
	// the race and wrote first (both changes present), or the edit landed first
	// and lifecycle skipped rather than reverting it.
	staleSkip := errors.Is(lcErr, ErrChangedDuringRun)
	if lcErr != nil && !staleSkip {
		t.Fatalf("lifecycle rewrite: %v", lcErr)
	}

	got, err := os.ReadFile(filepath.Join(root, "notes", "contested.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one of these can be missing if the writers raced: the loser's
	// change is silently absent while its call reported success.
	// The edit must survive unconditionally: it is the write that had fresh
	// content, and reverting it is the defect under test.
	if !strings.Contains(string(got), "marker: edited") {
		t.Errorf("the edit was reverted by a stale lifecycle write:\n%s", got)
	}
	if !staleSkip && !strings.Contains(string(got), "confidence: 0.9") {
		t.Errorf("lifecycle reported success but its stamp is absent:\n%s", got)
	}
	if staleSkip && strings.Contains(string(got), "confidence: 0.9") {
		t.Errorf("lifecycle skipped yet its write landed anyway:\n%s", got)
	}
	_ = s
	_ = context.Background()
}
