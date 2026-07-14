package write

import (
	"context"
	"fmt"
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
	id := uuid.NewString()
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
	id := uuid.NewString()

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
	bad := "---\nid: " + uuid.NewString() + "\ncategory: entry\n---\nmissing timestamps\n"
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
	id := uuid.NewString()
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
	content := noteContent(uuid.NewString(), "cognosis", "body\n")
	if err := p.Write(ctx, "entries/pm.md", content, "other-project"); !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("err = %v, want Validation", err)
	}
}

func TestConcurrentSamePathSerializes(t *testing.T) {
	p, s, _, ctx := testPipeline(t)
	id := uuid.NewString()
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
	targetID := uuid.NewString()
	if err := p.Write(ctx, "entries/target.md",
		noteContent(targetID, "", "the referenced capture\n"), ""); err != nil {
		t.Fatal(err)
	}
	srcID := uuid.NewString()
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
	id := uuid.NewString()
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
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if _, ok := vault.StageOf(rel); !ok {
			return nil
		}
		content, err := os.ReadFile(path)
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
