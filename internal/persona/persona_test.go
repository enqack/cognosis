package persona

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/embed/embedtest"
	"github.com/enqack/cognosis/internal/store/storetest"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

func seededRegistry(t *testing.T, enabled ...string) *Registry {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "personas")
	if err := Seed(dir); err != nil {
		t.Fatal(err)
	}
	return &Registry{Dir: dir, Enabled: enabled}
}

func TestSeedIsIdempotentAndShipsDeepThoughts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "personas")
	if err := Seed(dir); err != nil {
		t.Fatal(err)
	}
	if err := Seed(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deep-thoughts.md")); err != nil {
		t.Fatal("seed did not ship the first inhabitant")
	}
	// Seeding never overwrites an existing dir (user files are theirs).
	custom := filepath.Join(dir, "custom.md")
	if err := os.WriteFile(custom, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Seed(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(custom); string(b) != "mine" {
		t.Fatal("seed clobbered a user file")
	}
}

// TestListIsMetadataOnly — the tier-1 payload stays O(personas), never
// O(file content): no voice-guide text leaks into discovery.
func TestListIsMetadataOnly(t *testing.T) {
	r := seededRegistry(t, "deep-thoughts")
	metas := r.List()
	if len(metas) != 1 {
		t.Fatalf("metas = %d", len(metas))
	}
	m := metas[0]
	if m.ID != "deep-thoughts" || m.Description == "" {
		t.Fatalf("meta = %+v", m)
	}
	// The full file is ~2KB of voice guide; metadata must be a fraction.
	size := len(m.ID) + len(m.Name) + len(m.Description) + len(strings.Join(m.RespondsTo, ","))
	if size > 300 {
		t.Fatalf("tier-1 metadata is %d bytes — content is leaking into discovery", size)
	}
	if strings.Contains(m.Description, "Gentle Setup") {
		t.Fatal("voice-guide body leaked into metadata")
	}
}

func TestGetReturnsFullBody(t *testing.T) {
	r := seededRegistry(t, "deep-thoughts")
	p, err := r.Get("deep-thoughts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Body, "Gentle Setup") {
		t.Fatal("tier-2 fetch missing the voice guide")
	}
	if p.Bias["cursed-knowledge"] != 1.3 {
		t.Fatalf("bias = %v", p.Bias)
	}
}

// TestDisabledPersonaUnavailable — disabled means gone from discovery and
// invocation, while its file stays in place for reactivation.
func TestDisabledPersonaUnavailable(t *testing.T) {
	r := seededRegistry(t) // nothing enabled
	if metas := r.List(); len(metas) != 0 {
		t.Fatalf("disabled persona listed: %+v", metas)
	}
	if _, err := r.Get("deep-thoughts"); !cogerr.Is(err, cogerr.NotFound) {
		t.Fatalf("get disabled = %v, want NotFound", err)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "deep-thoughts.md")); err != nil {
		t.Fatal("file should remain in place while disabled")
	}
}

func TestWriteReflection(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	stub := embedtest.New()
	table := embed.TableSlug(stub.Name(), stub.Model())
	if err := s.EnsureProvider(ctx, stub.Name(), stub.Model(), table, stub.Dim, true); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	hist := vault.NewHistory(root)
	if err := hist.EnsureRepo(ctx); err != nil {
		t.Fatal(err)
	}
	pipeline := write.NewPipeline(&write.Indexer{Store: s, Provider: stub, Table: table}, root, hist, nil)
	r := seededRegistry(t, "deep-thoughts")

	rel, err := r.WriteReflection(ctx, pipeline, "deep-thoughts",
		"Ported the persona subsystem into the daemon.",
		"> A comedic blockquote body about daemons and destiny.", "",
		"Persona subsystem ported.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, "reflections/") {
		t.Fatalf("rel = %q", rel)
	}
	n, err := s.GetNote(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	if n.Category != "reflection" {
		t.Fatalf("category = %q", n.Category)
	}

	// The embedded chunk must be the description, never the styled body.
	if got, _ := s.CountChunks(ctx, rel); got != 1 {
		t.Fatalf("chunks = %d", got)
	}

	// Disabled persona rejected.
	if _, err := r.WriteReflection(ctx, pipeline, "nonexistent", "desc", "body", "", ""); !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("disabled persona: %v", err)
	}
	// Empty description rejected (it's the embeddable text).
	if _, err := r.WriteReflection(ctx, pipeline, "deep-thoughts", "  ", "body", "", ""); !cogerr.Is(err, cogerr.Validation) {
		t.Fatalf("empty description: %v", err)
	}
}
