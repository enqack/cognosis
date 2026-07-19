package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/enqack/cognosis/internal/cogerr"
)

const (
	// docPrefix / queryPrefix are nomic-embed-text's asymmetric task prefixes;
	// documents and queries must embed under different prefixes or retrieval
	// quality degrades. Applied only in this package.
	docPrefix   = "search_document: "
	queryPrefix = "search_query: "

	// batchSize bounds one /api/embed request.
	batchSize = 32
)

// Ollama embeds via a local (or remote) Ollama server's /api/embed endpoint.
type Ollama struct {
	baseURL string
	model   string
	http    *http.Client
}

// NewOllama returns a provider for the given base URL and pinned model tag.
// The long timeout absorbs cold model loads (10-30s on first request).
func NewOllama(baseURL, model string) *Ollama {
	return &Ollama{
		baseURL: baseURL,
		model:   model,
		http:    &http.Client{Timeout: 180 * time.Second},
	}
}

func (o *Ollama) Name() string  { return "ollama" }
func (o *Ollama) Model() string { return o.model }

func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))
		batch := make([]string, 0, end-start)
		for _, t := range texts[start:end] {
			batch = append(batch, docPrefix+t)
		}
		vecs, err := o.embed(ctx, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (o *Ollama) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := o.embed(ctx, []string{queryPrefix + text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// Health hits the version endpoint -- cheap reachability, not an embed round
// trip; a listening server that later fails to embed still fails loudly at
// first use.
func (o *Ollama) Health(ctx context.Context) error {
	const op = "embed.Ollama.Health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/api/version", nil)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return cogerr.Ef(op, cogerr.Unavailable, "ollama unreachable at %s: %v", o.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return cogerr.Ef(op, cogerr.Unavailable, "ollama at %s: version endpoint returned %s", o.baseURL, resp.Status)
	}
	return nil
}

func (o *Ollama) Dimension(ctx context.Context) (int, error) {
	return ProbeDimension(ctx, o)
}

func (o *Ollama) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	const op = "embed.Ollama.embed"
	body, err := json.Marshal(map[string]any{"model": o.model, "input": inputs})
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, cogerr.Ef(op, cogerr.Unavailable,
			"ollama at %s: %v (is ollama running with %s pulled?)", o.baseURL, err, o.model)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, cogerr.Ef(op, cogerr.Unavailable, "ollama /api/embed: %s: %s", resp.Status, e.Error)
	}

	var got struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	if len(got.Embeddings) != len(inputs) {
		return nil, cogerr.Ef(op, cogerr.Internal,
			"embedded %d inputs, got %d vectors", len(inputs), len(got.Embeddings))
	}
	if want, ok := KnownDimension(o.model); ok {
		for i, v := range got.Embeddings {
			if len(v) != want {
				return nil, cogerr.Ef(op, cogerr.Internal,
					"embedding %d has dim %d, want %d (wrong model?)", i, len(v), want)
			}
		}
	}
	return got.Embeddings, nil
}
