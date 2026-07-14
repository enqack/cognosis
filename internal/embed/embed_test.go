package embed

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTableSlug(t *testing.T) {
	cases := []struct{ name, model, want string }{
		{"ollama", "nomic-embed-text:v1.5", "embeddings_ollama_nomic_embed_text_v1_5"},
		{"OpenAI", "text-embedding-3-small", "embeddings_openai_text_embedding_3_small"},
		{"x", "weird//model::tag", "embeddings_x_weird_model_tag"},
	}
	for _, c := range cases {
		if got := TableSlug(c.name, c.model); got != c.want {
			t.Errorf("TableSlug(%q,%q) = %q, want %q", c.name, c.model, got, c.want)
		}
	}
}

func TestKnownDimension(t *testing.T) {
	if d, ok := KnownDimension("nomic-embed-text:v1.5"); !ok || d != 768 {
		t.Fatalf("nomic v1.5 = %d,%v", d, ok)
	}
	if d, ok := KnownDimension("nomic-embed-text:v2-experimental"); !ok || d != 768 {
		t.Fatalf("bare-name fallback = %d,%v", d, ok)
	}
	if _, ok := KnownDimension("never-heard-of-it"); ok {
		t.Fatal("unknown model must miss the shortcut")
	}
}

// ollamaAvailable gates the live integration tests: they run when a local
// Ollama is reachable, and skip (never fail) when it isn't.
func ollamaAvailable(t *testing.T) string {
	t.Helper()
	url := os.Getenv("COGNOSIS_TEST_OLLAMA")
	if url == "" {
		url = "http://localhost:11434"
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url + "/api/version")
	if err != nil {
		t.Skipf("ollama not reachable at %s: %v", url, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("ollama at %s returned %s", url, resp.Status)
	}
	return url
}

func TestOllamaRoundTrip(t *testing.T) {
	url := ollamaAvailable(t)
	p := NewOllama(url, "nomic-embed-text:v1.5")
	ctx := context.Background()

	if err := p.Health(ctx); err != nil {
		t.Fatal(err)
	}
	docs, err := p.Embed(ctx, []string{"the daemon reconciles the vault", "postgres stores the derived index"})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || len(docs[0]) != 768 || len(docs[1]) != 768 {
		t.Fatalf("docs shape: %d vectors, dims %d/%d", len(docs), len(docs[0]), len(docs[1]))
	}
	q, err := p.EmbedQuery(ctx, "where is the index stored?")
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 768 {
		t.Fatalf("query dim = %d", len(q))
	}
	dim, err := p.Dimension(ctx)
	if err != nil || dim != 768 {
		t.Fatalf("dimension = %d, %v", dim, err)
	}
}

func TestOllamaUnreachableIsNamed(t *testing.T) {
	p := NewOllama("http://127.0.0.1:1", "nomic-embed-text:v1.5")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := p.Health(ctx)
	if err == nil {
		t.Fatal("health against a dead port must fail")
	}
	if got := err.Error(); !strings.Contains(got, "127.0.0.1:1") {
		t.Fatalf("error does not name the target: %s", got)
	}
}
