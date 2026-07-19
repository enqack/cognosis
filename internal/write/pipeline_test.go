package write

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/embed/embedtest"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/vault"
)

const testTable = "embeddings_stub_stub_model"

func testPipeline(t *testing.T) (*Pipeline, *store.Store, string, context.Context) {
	t.Helper()
	s, _ := storetest.New(t)
	ctx := context.Background()
	stub := embedtest.New()
	if err := s.EnsureProvider(ctx, stub.Name(), stub.Model(),
		embed.TableSlug(stub.Name(), stub.Model()), stub.Dim, true); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	hist := vault.NewHistory(root)
	if err := hist.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	ix := &Indexer{Store: s, Provider: stub, Table: testTable}
	p := NewPipeline(ix, root, hist, nil)
	return p, s, root, ctx
}

func noteContent(id, project, body string) string {
	return `---
id: ` + id + `
category: entry
project: ` + project + `
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
` + body
}

func TestWriteLandsEverything(t *testing.T) {
	p, s, root, ctx := testPipeline(t)
	id := uuid.Must(uuid.NewV7()).String()
	content := noteContent(id, "cognosis", "The daemon reconciles the vault on boot.\n")

	if err := p.Write(ctx, "entries/first.md", content, "cognosis"); err != nil {
		t.Fatal(err)
	}

	// File on disk.
	if _, err := os.Stat(filepath.Join(root, "entries", "first.md")); err != nil {
		t.Fatal(err)
	}
	// Note row.
	n, err := s.GetNote(ctx, "entries/first.md")
	if err != nil {
		t.Fatal(err)
	}
	if n.Project != "cognosis" {
		t.Fatalf("project = %q", n.Project)
	}
	// Chunks + embeddings.
	if got, _ := s.CountChunks(ctx, "entries/first.md"); got != 1 {
		t.Fatalf("chunks = %d, want 1", got)
	}
	if got := countEmbeddings(t, s, ctx); got != 1 {
		t.Fatalf("embeddings = %d, want 1", got)
	}
	// One history commit for the write.
	lines, err := vault.NewHistory(root).Log(ctx, "entries/first.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("history commits = %d, want exactly 1", len(lines))
	}
}

func countEmbeddings(t *testing.T, s *store.Store, ctx context.Context) int {
	t.Helper()
	n, err := s.CountEmbeddings(ctx, testTable)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRewriteLeavesNoOrphans(t *testing.T) {
	p, s, _, ctx := testPipeline(t)
	id := uuid.Must(uuid.NewV7()).String()

	long := strings.Repeat("alpha bravo charlie. ", 20)
	multi := noteContent(id, "", "## One\n\n"+long+"\n\n## Two\n\n"+long+"\n")
	if err := p.Write(ctx, "entries/re.md", multi, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountChunks(ctx, "entries/re.md"); got != 2 {
		t.Fatalf("chunks = %d, want 2", got)
	}
	if got := countEmbeddings(t, s, ctx); got != 2 {
		t.Fatalf("embeddings = %d, want 2", got)
	}

	single := noteContent(id, "", "collapsed to one chunk\n")
	if err := p.Write(ctx, "entries/re.md", single, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountChunks(ctx, "entries/re.md"); got != 1 {
		t.Fatalf("chunks after rewrite = %d, want 1", got)
	}
	if got := countEmbeddings(t, s, ctx); got != 1 {
		t.Fatalf("embeddings after rewrite = %d, want 1 (orphans leaked)", got)
	}
}

func TestValidationNamesField(t *testing.T) {
	p, _, _, ctx := testPipeline(t)
	bad := "---\nid: " + uuid.Must(uuid.NewV7()).String() + "\ncategory: entry\n---\nmissing timestamps\n"
	err := p.Write(ctx, "entries/bad.md", bad, "")
	if !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("err = %v, want Validation", err)
	}
	if !strings.Contains(err.Error(), "created") {
		t.Fatalf("error does not name the missing field: %v", err)
	}
}

func TestPathRules(t *testing.T) {
	p, _, _, ctx := testPipeline(t)
	id := uuid.Must(uuid.NewV7()).String()
	content := noteContent(id, "", "body\n")
	for _, bad := range []string{
		"../escape.md", "outside/x.md", "log.md", "entries/nested.txt",
	} {
		if err := p.Write(ctx, bad, content, ""); !cogerr.Is(err, cogerr.Validation) {
			t.Errorf("path %q: err = %v, want Validation", bad, err)
		}
	}
}

func TestProjectMismatchRejected(t *testing.T) {
	p, _, _, ctx := testPipeline(t)
	content := noteContent(uuid.Must(uuid.NewV7()).String(), "cognosis", "body\n")
	if err := p.Write(ctx, "entries/pm.md", content, "other-project"); !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("err = %v, want Validation", err)
	}
}

func TestConcurrentSamePathSerializes(t *testing.T) {
	p, s, _, ctx := testPipeline(t)
	id := uuid.Must(uuid.NewV7()).String()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := noteContent(id, "", fmt.Sprintf("version %d\n", i))
			errs[i] = p.Write(ctx, "entries/race.md", content, "")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// Exactly one note row and one chunk survive; content is one of the runs.
	n, err := s.GetNote(ctx, "entries/race.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(n.Content, "version ") {
		t.Fatalf("content = %q", n.Content)
	}
	if got, _ := s.CountChunks(ctx, "entries/race.md"); got != 1 {
		t.Fatalf("chunks = %d", got)
	}
}

func TestLinksResolvedAndDanglingDropped(t *testing.T) {
	p, s, _, ctx := testPipeline(t)
	targetID := uuid.Must(uuid.NewV7()).String()
	if err := p.Write(ctx, "entries/target.md",
		noteContent(targetID, "", "the referenced capture\n"), ""); err != nil {
		t.Fatal(err)
	}
	srcID := uuid.Must(uuid.NewV7()).String()
	body := "links to [[target]] and to [[nonexistent-note]]\n"
	if err := p.Write(ctx, "entries/src.md", noteContent(srcID, "", body), ""); err != nil {
		t.Fatal(err)
	}
	src, err := s.GetNote(ctx, "entries/src.md")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CountLinks(ctx, src.ID); got != 1 {
		t.Fatalf("links = %d, want 1 (dangling dropped)", got)
	}
}

// TestCrashBetweenFileAndDBConverges — the file lands but the DB commit never
// happens (simulated by writing the file exactly as the pipeline would and
// stopping); boot reconciliation repairs the divergence.
func TestCrashBetweenFileAndDBConverges(t *testing.T) {
	p, s, root, ctx := testPipeline(t)
	id := uuid.Must(uuid.NewV7()).String()
	content := noteContent(id, "", "crashed before the DB commit\n")

	// Simulate the crash: file written, history committed, no DB write.
	abs := filepath.Join(root, "entries", "crashed.md")
	if err := vault.WriteFileAtomic(abs, []byte(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNote(ctx, "entries/crashed.md"); err == nil {
		t.Fatal("note should not be indexed yet")
	}

	// Boot reconciliation converges: reuse the pipeline's indexer through a
	// fresh walk of the vault, the same way the watcher does.
	if err := reconcileVault(ctx, p, root); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNote(ctx, "entries/crashed.md")
	if err != nil {
		t.Fatalf("reconciliation did not converge: %v", err)
	}
	if n.Content != "crashed before the DB commit\n" {
		t.Fatalf("content = %q", n.Content)
	}
	if got, _ := s.CountChunks(ctx, "entries/crashed.md"); got != 1 {
		t.Fatalf("chunks = %d, want 1 (reconciliation must chunk+embed too)", got)
	}
}

// reconcileVault is a minimal stand-in for the watcher's boot pass: index
// every stage file on disk through the shared core.
func reconcileVault(ctx context.Context, p *Pipeline, root string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	return fs.WalkDir(r.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(rel, ".md") {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := vault.StageOf(rel); !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f, err := r.Open(rel)
		if err != nil {
			return err
		}
		content, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		n, err := vault.ParseNote(rel, content)
		if err != nil {
			return err
		}
		return p.Indexer.Index(ctx, n, FileMeta{Mtime: info.ModTime(), Size: info.Size(), Blake3: "reconciled"})
	})
}

// noteContentNoID is noteContent with the id line omitted entirely — what an
// agent holding only the MCP tools can actually produce.
func noteContentNoID(body string) string {
	return `---
category: entry
project: cognosis
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
` + body
}

// TestOmittedIDIsMinted — the contract requires a UUIDv7 and the MCP surface
// offers no way to produce one, so every note written through it previously
// needed an out-of-band uuid generator. Omitting the id must now work, and must
// produce an id that satisfies the validator that rejected its absence.
func TestOmittedIDIsMinted(t *testing.T) {
	p, s, root, ctx := testPipeline(t)
	const rel = "entries/minted.md"

	if err := p.Write(ctx, rel, noteContentNoID("Body text.\n"), ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.GetNote(ctx, rel)
	if err != nil {
		t.Fatalf("note not indexed: %v", err)
	}
	if n.ID.Version() != 7 {
		t.Errorf("minted id is v%d, want v7 — the validator rejects anything else", n.ID.Version())
	}

	// The id must also reach the file: the vault is the source of truth, and a
	// note whose id exists only in the index would not survive a rebuild.
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "id: "+n.ID.String()) {
		t.Errorf("minted id absent from the file on disk:\n%s", b)
	}
}

// TestOmittedIDOnExistingPathReusesID is the load-bearing half.
//
// UpsertNote treats same-path-different-id as an eviction: it deletes the row
// and cascades every inbound link away. So minting unconditionally would make
// "omit the id" a way to silently destroy a note's inbound graph on every
// update — the same damage an atomic editor save used to cause, by a different
// route. The id must be reused, and the referrer's edge must survive.
func TestOmittedIDOnExistingPathReusesID(t *testing.T) {
	p, s, _, ctx := testPipeline(t)
	const target = "entries/target.md"

	if err := p.Write(ctx, target, noteContentNoID("First version.\n"), ""); err != nil {
		t.Fatal(err)
	}
	first, err := s.GetNote(ctx, target)
	if err != nil {
		t.Fatal(err)
	}

	// A second note points at it, so there is an inbound edge to lose.
	if err := p.Write(ctx, "entries/referrer.md",
		noteContentNoID("See [[target]] for detail.\n"), ""); err != nil {
		t.Fatal(err)
	}
	referrer, err := s.GetNote(ctx, "entries/referrer.md")
	if err != nil {
		t.Fatal(err)
	}
	if dsts, err := s.LinkDsts(ctx, referrer.ID); err != nil {
		t.Fatal(err)
	} else if len(dsts) != 1 {
		t.Fatalf("precondition: referrer should link to target, got %v", dsts)
	}

	// Overwrite the target, again without an id.
	if err := p.Write(ctx, target, noteContentNoID("Second version.\n"), ""); err != nil {
		t.Fatal(err)
	}
	second, err := s.GetNote(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("id changed on overwrite: %s -> %s; UpsertNote evicts on same-path-different-id "+
			"and cascades inbound links away", first.ID, second.ID)
	}
	dsts, err := s.LinkDsts(ctx, referrer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dsts) != 1 || dsts[0] != first.ID {
		// Verified against the un-fixed code: the edge is not dropped, it is
		// re-pointed at the newly minted id, because RepairReferrers resolves
		// wikilinks by basename after the write. So the visible symptom is id
		// churn rather than a missing edge — the note's identity changes on
		// every update, which is exactly the stability note ids exist to give,
		// and anything holding the old id now refers to a row that was evicted.
		t.Errorf("referrer no longer points at the original note id: got %v, want [%s]", dsts, first.ID)
	}
}

// An explicitly supplied id still wins, and a non-v7 one is still rejected —
// minting is a fallback for an absent value, not a relaxation of the contract.
func TestSuppliedIDIsHonouredAndStillValidated(t *testing.T) {
	p, s, _, ctx := testPipeline(t)

	want := uuid.Must(uuid.NewV7()).String()
	if err := p.Write(ctx, "entries/pinned.md", noteContent(want, "cognosis", "Body.\n"), ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNote(ctx, "entries/pinned.md")
	if err != nil {
		t.Fatal(err)
	}
	if n.ID.String() != want {
		t.Errorf("supplied id %s was replaced by %s", want, n.ID)
	}

	v4 := uuid.Must(uuid.NewRandom()).String()
	err = p.Write(ctx, "entries/v4.md", noteContent(v4, "cognosis", "Body.\n"), "")
	if !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("a v4 id was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "UUIDv7") {
		t.Errorf("rejection does not name the requirement: %v", err)
	}
}
