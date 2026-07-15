// Package embedtest provides a deterministic stub Provider so unit tests
// never touch a live embedding server. Vectors are derived from a content
// hash: identical text always embeds identically, distinct text almost surely
// differs, and cosine ordering is stable across runs.
package embedtest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
)

// Stub is a deterministic embed.Provider. Dim defaults to 8 — small keeps
// fixtures readable; the schema takes any width.
type Stub struct {
	Dim       int
	ModelName string
	// Vectors, when set, pins exact vectors per input text (after prefix
	// stripping is NOT applied — the stub sees raw text).
	Vectors map[string][]float32
}

func New() *Stub { return &Stub{Dim: 8, ModelName: "stub-model"} }

func (s *Stub) Name() string  { return "stub" }
func (s *Stub) Model() string { return s.ModelName }

func (s *Stub) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = s.vec(t)
	}
	return out, nil
}

func (s *Stub) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return s.vec(text), nil
}

func (s *Stub) Health(context.Context) error { return nil }

func (s *Stub) Dimension(context.Context) (int, error) { return s.dim(), nil }

func (s *Stub) dim() int {
	if s.Dim <= 0 {
		return 8
	}
	return s.Dim
}

// vec hashes the text into a unit vector. Pinned vectors win when present.
func (s *Stub) vec(text string) []float32 {
	if v, ok := s.Vectors[text]; ok {
		return v
	}
	sum := sha256.Sum256([]byte(text))
	v := make([]float32, s.dim())
	var norm float64
	for i := range v {
		// Four bytes per component, wrapping over the digest.
		off := (i * 4) % (len(sum) - 4)
		bits := binary.LittleEndian.Uint32(sum[off : off+4])
		// Map the full uint32 range to roughly [-1, 1] without a narrowing
		// signed conversion (widening uint32->float64 only).
		f := (float64(bits) - float64(math.MaxUint32)/2) / (float64(math.MaxUint32) / 2)
		v[i] = float32(f)
		norm += f * f
	}
	if norm > 0 {
		n := float32(math.Sqrt(norm))
		for i := range v {
			v[i] /= n
		}
	}
	return v
}
