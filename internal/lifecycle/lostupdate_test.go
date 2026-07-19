package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// TestMoveDoesNotRevertAConcurrentEdit — move was the one rewrite path with no
// stale protection, and the worst one to lose it on: it writes the destination
// and deletes the source, so a concurrent edit is reverted *and* the note is
// archived out of retrieval, with both calls reporting success.
//
// rewriteLocked reads the file it is about to write, which for a move is the
// destination — and move has already refused if that exists. So its check
// always hit the not-exist arm and was skipped by construction. The source is
// what has to be compared.
func TestMoveDoesNotRevertAConcurrentEdit(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	e.Locks = write.NewPathLocks()

	const rel = "notes/doomed.md"
	body := "---\nid: 019f7000-0000-7000-8000-000000000002\ncategory: concept\n" +
		"confidence: 0.1\nmaturity: seed\ncreated: \"2026-07-12 09:00:00\"\n" +
		"updated: \"2026-07-12 09:00:00\"\nlast_reinforced: \"2026-07-12 09:00:00\"\n" +
		"reinforce_count: 0\nsources:\n  - \"[[x]]\"\n---\noriginal body\n"
	abs := filepath.Join(root, "notes", "doomed.md")
	if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := vault.ParseNote(rel, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Indexer.Index(ctx, n, write.FileMeta{Mtime: time.Now(), Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	// Somebody edits the note after the run parsed it.
	edited := strings.Replace(body, "original body", "edited body", 1)
	if err := os.WriteFile(abs, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	// The run, still holding its T0 copy, tries to archive it.
	err = e.move(ctx, n, "archive/doomed.md")
	if !errors.Is(err, ErrChangedDuringRun) {
		t.Fatalf("move err = %v, want ErrChangedDuringRun", err)
	}
	cur, rerr := os.ReadFile(abs)
	if rerr != nil {
		t.Fatalf("source note was deleted despite the skip: %v", rerr)
	}
	if !strings.Contains(string(cur), "edited body") {
		t.Errorf("the edit was reverted by a stale move:\n%s", cur)
	}
	if _, err := os.Stat(filepath.Join(root, "archive", "doomed.md")); err == nil {
		t.Error("the note was archived from stale content despite the skip")
	}
}

// TestCompileSkipsArchivalWithoutAbortingTheRun drives Compile, not move.
//
// That distinction is the whole point. Adding a source-digest check to `move`
// made ErrChangedDuringRun reachable from a function that could never return it
// before, while both archival call sites still treated any error as fatal. One
// concurrently-edited note then aborted the entire run: the report discarded
// along with writes that had already landed, no log.md entry, and every mutated
// file left uncommitted in vault history. The previous test called e.move
// directly and so could not see any of it.
//
// The interleave uses the real shared lock rather than a test-only hook: hold
// the note's lock, let the run block on it at the move, change the file, then
// release. That is precisely what a concurrent edit_note does.
func TestCompileSkipsArchivalWithoutAbortingTheRun(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	e.Locks = write.NewPathLocks()

	old := time.Now().AddDate(0, 0, -400).Format(vault.TimeLayout)
	mk := func(id, name string, conf float64) string {
		return "---\nid: " + id + "\ncategory: concept\nconfidence: " +
			strconv.FormatFloat(conf, 'f', 1, 64) + "\nmaturity: seed\n" +
			"created: \"" + old + "\"\nupdated: \"" + old + "\"\n" +
			"last_reinforced: \"" + old + "\"\nreinforce_count: 0\n" +
			"sources:\n  - \"[[x]]\"\n---\nbody of " + name + "\n"
	}
	doomed := mk("019f7000-0000-7000-8000-00000000000a", "doomed", 0.0)
	healthy := mk("019f7000-0000-7000-8000-00000000000b", "healthy", 0.5)
	for name, body := range map[string]string{"doomed": doomed, "healthy": healthy} {
		if err := os.WriteFile(filepath.Join(root, "notes", name+".md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		n, err := vault.ParseNote("notes/"+name+".md", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Indexer.Index(ctx, n, write.FileMeta{Mtime: time.Now(), Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
	}

	// Hold the doomed note's lock so the run blocks when it reaches the move.
	unlock := e.Locks.Lock("notes/doomed.md")

	type result struct {
		rep *Report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := e.Run(ctx, Options{Now: time.Now()})
		done <- result{rep, err}
	}()

	// Let the run reach the move and block, then land the concurrent edit.
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, "notes", "doomed.md"),
		[]byte(strings.Replace(doomed, "body of doomed", "edited body", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock()

	got := <-done
	if got.err != nil {
		t.Fatalf("one concurrently-edited note aborted the whole run: %v", got.err)
	}

	kinds := map[string]string{}
	for _, a := range got.rep.Actions {
		kinds[a.Note] = a.Kind
	}
	if kinds["doomed"] != "skipped" {
		t.Errorf("doomed reported %q, want skipped (actions: %+v)", kinds["doomed"], got.rep.Actions)
	}
	if _, err := os.Stat(filepath.Join(root, "archive", "doomed.md")); err == nil {
		t.Error("note archived from stale content despite the skip")
	}
	cur, err := os.ReadFile(filepath.Join(root, "notes", "doomed.md"))
	if err != nil {
		t.Fatalf("source deleted despite the skip: %v", err)
	}
	if !strings.Contains(string(cur), "edited body") {
		t.Errorf("the concurrent edit was reverted:\n%s", cur)
	}
}

// A skipped write must leave no trace of the actions it was carrying. One
// reinforce appends up to three (`reinforced`, `dispute-cleared`, `promoted`)
// before the write, so replacing only the last left `reinforced` standing above
// `skipped` — the report affirming something that never reached disk.
func TestReportReplaceSinceDropsEveryActionForTheNote(t *testing.T) {
	r := &Report{Actions: []Action{{"reinforced", "other", "kept"}}}
	mark := len(r.Actions)
	r.Actions = append(r.Actions,
		Action{"reinforced", "target", "0.7→0.8"},
		Action{"dispute-cleared", "target", ""},
		Action{"promoted", "target", "seed→developing"})

	r.replaceSince(mark, Action{"skipped", "target", "(changed during the run)"})

	if len(r.Actions) != 2 {
		t.Fatalf("actions = %+v, want the earlier note plus one skipped", r.Actions)
	}
	if r.Actions[0].Note != "other" || r.Actions[0].Kind != "reinforced" {
		t.Errorf("a previous note's action was clobbered: %+v", r.Actions[0])
	}
	if r.Actions[1].Kind != "skipped" {
		t.Errorf("target reports %q", r.Actions[1].Kind)
	}
}

// editOnNthSuppress is a Suppressor that rewrites the note's file the Nth time
// the engine announces a write. Supp is a real production seam, called inside
// rewriteLocked just before the staleness check, so this lands a concurrent
// edit at an exact point in the run rather than racing a sleep against it.
type editOnNthSuppress struct {
	abs      string
	from, to string
	n        int
	on       int
}

func (s *editOnNthSuppress) Suppress(string) {
	s.n++
	if s.n != s.on {
		return
	}
	// Read-modify-write, like a real editor. Writing canned bytes here would
	// clobber the write that just landed, which is the state the assertions
	// need to observe.
	cur, err := os.ReadFile(s.abs)
	if err != nil {
		return
	}
	_ = os.WriteFile(s.abs, []byte(strings.Replace(string(cur), s.from, s.to, 1)), 0o600)
}
func (s *editOnNthSuppress) Unsuppress(string) {}

// TestReinforceThenGraduateKeepsTheLandedWrite — caller-level, and the third
// distinct wrong-entry bug in this mechanism.
//
// One iteration can write twice: the `changed` block writes a reinforce, then
// the graduate block writes again. `mark` was captured once per note, so a skip
// on the *second* write truncated back past the first — deleting actions for a
// change already on disk, already indexed and already counted in changedFiles.
// log.md would then deny a confidence bump that happened, and an agent reading
// "not applied" would re-issue the reinforce and apply it twice.
//
// The edit has to land *between* the two writes. An earlier attempt held the
// shared lock for the whole run, which blocked the first write instead — so
// both were skipped, truncation was harmless, and the test passed with the bug
// present. Hooking the second Suppress makes the interleave exact.
func TestReinforceThenGraduateKeepsTheLandedWrite(t *testing.T) {
	e, _, root, ctx := testEngine(t)
	e.Locks = write.NewPathLocks()

	const rel = "notes/canon.md"
	abs := filepath.Join(root, "notes", "canon.md")
	now := time.Now().Format(vault.TimeLayout)
	body := "---\nid: 019f7000-0000-7000-8000-00000000000c\ncategory: concept\n" +
		"confidence: 0.9\nmaturity: stable\ncreated: \"" + now + "\"\nupdated: \"" + now + "\"\n" +
		"last_reinforced: \"" + now + "\"\nreinforce_count: 3\nsources:\n  - \"[[x]]\"\n---\ncanon body\n"
	if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := vault.ParseNote(rel, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Indexer.Index(ctx, n, write.FileMeta{Mtime: time.Now(), Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	// Edit on the second announced write: the reinforce lands, the graduate skips.
	e.Supp = &editOnNthSuppress{abs: abs, from: "canon body", to: "edited body", on: 2}

	rep, err := e.Run(ctx, Options{Now: time.Now(), Reinforce: []string{rel}, Graduate: []string{rel}})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	onDisk, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	reinforced := strings.Contains(string(onDisk), "reinforce_count: 4")
	if !reinforced {
		t.Fatalf("precondition: the first write should have landed before the edit:\n%s", onDisk)
	}
	kinds := map[string]bool{}
	for _, a := range rep.Actions {
		if a.Note == "canon" {
			kinds[a.Kind] = true
		}
	}
	if !kinds["reinforced"] {
		t.Errorf("the reinforce is on disk but the report dropped it — log.md would deny a change "+
			"that happened, and an agent would re-issue it: %+v", rep.Actions)
	}
	if !kinds["skipped"] {
		t.Errorf("the graduate was skipped but the report does not say so: %+v", rep.Actions)
	}
}
