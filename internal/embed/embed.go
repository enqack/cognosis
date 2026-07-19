// Package embed defines the embedding-provider boundary. The provider is a
// behavioral contract (interface); batch plumbing over it is structural and
// generic-friendly. Task prefixes for asymmetric retrieval models are applied
// here and nowhere else.
package embed

import (
	"context"
	"strings"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Provider is one embedding backend (local Ollama today; remote later).
// Documents and queries embed through different task prefixes, so both
// directions live on the interface rather than a single Embed call.
type Provider interface {
	// Name identifies the provider (e.g. "ollama") for the registry.
	Name() string
	// Model is the exact pinned model tag; changing it means a re-embed.
	Model() string
	// Embed returns one vector per input document text.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQuery embeds a single retrieval query.
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	// Health is a lightweight reachability check -- never a full embed round
	// trip; startup uses it as a fatal gate.
	Health(ctx context.Context) error
	// Dimension reports the vector width, probing the provider if needed.
	Dimension(ctx context.Context) (int, error)
}

// knownDimensions shortcuts the live probe for recognized models; unknown
// models fall back to embedding a throwaway string and measuring.
var knownDimensions = map[string]int{
	"nomic-embed-text":      768,
	"nomic-embed-text:v1.5": 768,
}

// KnownDimension returns the registered width for a model tag, trying the
// exact tag first and then the bare model name.
func KnownDimension(model string) (int, bool) {
	if d, ok := knownDimensions[model]; ok {
		return d, true
	}
	base, _, _ := strings.Cut(model, ":")
	d, ok := knownDimensions[base]
	return d, ok
}

// TableSlug derives the per-provider embedding table name from provider name
// and model tag: lowercase, every non-alphanumeric run collapsed to one
// underscore. The table is provisioned at runtime once the dimension is known.
func TableSlug(name, model string) string {
	sanitize := func(s string) string {
		var b strings.Builder
		lastUnderscore := false
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
				lastUnderscore = false
			} else if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
		return strings.Trim(b.String(), "_")
	}
	return "embeddings_" + sanitize(name) + "_" + sanitize(model)
}

// ProbeDimension resolves a provider's vector width: known-model shortcut
// first, then a live single-string embed.
func ProbeDimension(ctx context.Context, p Provider) (int, error) {
	if d, ok := KnownDimension(p.Model()); ok {
		return d, nil
	}
	vecs, err := p.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		return 0, cogerr.E("embed.ProbeDimension", cogerr.Unavailable, err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return 0, cogerr.Ef("embed.ProbeDimension", cogerr.Internal,
			"probe returned %d vectors", len(vecs))
	}
	return len(vecs[0]), nil
}
