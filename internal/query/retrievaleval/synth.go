// Package retrievaleval measures retrieval quality and latency against
// falsifiable ground truth. It is a local-tier harness, not part of the
// request path and not run in CI: it produces recorded measurements for a
// human, and CI is the wrong place for numbers that jitter with HNSW graph
// construction or shared-runner noise.
package retrievaleval

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
)

// DefaultSpread is the calibrated noise-to-signal ratio. Chosen from the
// TestSpreadCalibration table (768 dim, 40 clusters, 2000 docs):
//
//	spread  top50_same_cluster  min_d   stddev_d
//	  0.25               0.892  0.0499   0.14388
//	  1.00               0.892  0.4229   0.08237
//	  3.00               0.628  0.7822   0.03898
//	  5.00               0.152  0.8437   0.03646
//
// 1.0 puts nearest neighbours at cosine distance ~0.42 (similarity ~0.58) and
// far pairs near 1.15, which is the range real nomic-embed-text embeddings
// occupy, with a distance spread (0.082) well above the uniform-random floor
// (~0.036, the 5.0 row). It also sits mid-band, so it is not near a cliff.
//
// Note that top50_same_cluster is flat from 0.25 to 2.0 and so is a weak
// discriminator on its own — min_d and stddev_d are what separate those rows.
// The authoritative check that this corpus can detect ANN degradation is the
// empirical recall curve in the Phase 5 capacity tests, not this table.
const DefaultSpread = 1.0

// Synth is a deterministic embed.Provider producing *clustered* unit vectors.
//
// It exists because neither available alternative can measure ANN recall:
//   - embedtest.Stub is 8-dim by contract. At that width an HNSW graph is
//     effectively degenerate and recall reads ~1.0 at any ef_search, so the
//     measurement cannot discriminate a good setting from a broken one.
//   - Ollama/nomic-embed-text gives the true distribution but cannot run in
//     CI, takes minutes to embed a corpus, and would invalidate every recorded
//     artifact on a model bump.
//
// Cluster structure is the point, not decoration. Uniform-random 768-dim
// vectors are the adversarial case for ANN — every pair is near-orthogonal and
// distances concentrate — which makes measured recall a pessimistic bound
// rather than an estimate of production behavior, where topically-related
// notes cluster strongly.
type Synth struct {
	Dim      int     // vector width; 768 to match nomic-embed-text
	Clusters int     // number of latent topics
	Seed     int64   // centre generation
	Spread   float64 // noise-to-signal ratio; the difficulty knob

	// Labels pins the cluster for specific texts, mirroring embedtest.Stub's
	// Vectors pin-map. The corpus builder needs it: it picks a cluster first,
	// then draws that cluster's vocabulary into the chunk text — which changes
	// the text, and so would change a hash-derived cluster. Pinning keeps the
	// geometry (which centre the vector sits near), the pseudo-relevance label,
	// and the lexical content all naming the same cluster. Without it the three
	// silently disagree and ClusterPrecision measures nothing.
	Labels map[string]int

	centres [][]float32
}

// NewSynth builds a generator with pre-computed cluster centres.
//
// Spread is what makes the harness discriminating and must be calibrated, not
// guessed: too tight and exact-KNN top-k is entirely one cluster, so recall is
// trivially 1.0 at any setting and a broken harness is indistinguishable from
// a healthy index; too wide and the corpus degenerates toward uniform-random.
// Calibrate with CalibrateSpread.
func NewSynth(dim, clusters int, seed int64, spread float64) *Synth {
	s := &Synth{Dim: dim, Clusters: clusters, Seed: seed, Spread: spread}
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // reproducibility is the requirement, not unpredictability
	s.centres = make([][]float32, clusters)
	for c := range s.centres {
		v := make([]float32, dim)
		for i := range v {
			v[i] = float32(rng.NormFloat64())
		}
		normalize(v)
		s.centres[c] = v
	}
	return s
}

func (s *Synth) Name() string  { return "synth" }
func (s *Synth) Model() string { return fmt.Sprintf("synth-d%d-c%d", s.Dim, s.Clusters) }

func (s *Synth) Health(context.Context) error           { return nil }
func (s *Synth) Dimension(context.Context) (int, error) { return s.Dim, nil }

func (s *Synth) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = s.vec(t)
	}
	return out, nil
}

func (s *Synth) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return s.vec(text), nil
}

// ClusterOf is the free pseudo-relevance label: the latent topic a text
// belongs to. Used by ClusterPrecision to score rankers for which no exact
// ground truth exists (the keyword leg), where brute-force KNN cannot serve.
func (s *Synth) ClusterOf(text string) int {
	if s.Clusters <= 0 {
		return 0
	}
	if c, ok := s.Labels[text]; ok {
		return c
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	// Modulus is a positive int, so the result always fits back in an int.
	return int(h.Sum64() % uint64(s.Clusters)) //nolint:gosec // bounded by Clusters
}

// vec places text near its cluster centre with deterministic per-text noise.
//
// Noise is scaled by 1/sqrt(Dim) so Spread is dimension-independent: a vector
// of Dim iid N(0, sigma) components has expected norm sigma*sqrt(Dim), so
// without the scaling the usable Spread band shifts with every change to Dim.
// With it, Spread is the ratio of noise norm to centre norm — Spread=1.0 means
// the noise is as large as the signal — and the same value means the same
// thing at 8, 768, or 3072 dimensions.
func (s *Synth) vec(text string) []float32 {
	c := s.ClusterOf(text)
	// Seed the noise from the text so the same text always embeds identically
	// — the Provider contract embedtest.Stub also honors.
	h := fnv.New64a()
	_, _ = h.Write([]byte("noise:" + text))
	rng := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec // not cryptographic

	sigma := s.Spread / math.Sqrt(float64(s.Dim))
	v := make([]float32, s.Dim)
	centre := s.centres[c]
	for i := range v {
		v[i] = centre[i] + float32(rng.NormFloat64()*sigma)
	}
	normalize(v)
	return v
}

func normalize(v []float32) {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return
	}
	n := float32(math.Sqrt(norm))
	for i := range v {
		v[i] /= n
	}
}

// DistinctVectors counts unique vectors among the embeddings of texts.
//
// This guard is not hypothetical. The first run of this experiment used a
// generator whose randomness was accidentally hoisted out of the per-row loop,
// producing 20 000 rows holding 1 distinct vector. Every distance tied, recall
// read as noise, and the result appeared to *falsify* the hypothesis under
// test. A degenerate corpus yields confident, plausible, wrong numbers — so
// corpus construction asserts non-degeneracy before anything is measured.
func DistinctVectors(vecs [][]float32) int {
	seen := make(map[string]struct{}, len(vecs))
	buf := make([]byte, 4)
	for _, v := range vecs {
		key := make([]byte, 0, len(v)*4)
		for _, x := range v {
			binary.LittleEndian.PutUint32(buf, math.Float32bits(x))
			key = append(key, buf...)
		}
		seen[string(key)] = struct{}{}
	}
	return len(seen)
}

// SpreadReport describes how discriminating a corpus is at a given Spread.
type SpreadReport struct {
	Spread float64
	// TopKSameCluster is the mean fraction of an exact top-k that shares the
	// query's cluster. Near 1.0 means clusters are too tight to discriminate;
	// near 1/Clusters means the structure has washed out to uniform-random.
	TopKSameCluster              float64
	MinDist, MaxDist, StdDevDist float64
}

// CalibrateSpread reports cluster tightness across candidate Spread values so
// the caller can pick one deliberately. Pure in-memory cosine math — no
// database, no index — so it is cheap to re-run when Dim or Clusters change.
func CalibrateSpread(dim, clusters int, seed int64, spreads []float64,
	corpus, queries, k int) []SpreadReport {
	out := make([]SpreadReport, 0, len(spreads))
	for _, sp := range spreads {
		s := NewSynth(dim, clusters, seed, sp)
		docs := make([][]float32, corpus)
		docCluster := make([]int, corpus)
		for i := range docs {
			t := fmt.Sprintf("doc-%d", i)
			docs[i] = s.vec(t)
			docCluster[i] = s.ClusterOf(t)
		}
		var sameSum, minD, maxD, sumD, sumSq float64
		minD, maxD = math.Inf(1), math.Inf(-1)
		n := 0
		for q := range queries {
			qt := fmt.Sprintf("query-%d", q)
			qv := s.vec(qt)
			qc := s.ClusterOf(qt)
			type hit struct {
				d float64
				c int
			}
			hits := make([]hit, corpus)
			for i := range docs {
				d := cosineDist(qv, docs[i])
				hits[i] = hit{d, docCluster[i]}
				minD, maxD = math.Min(minD, d), math.Max(maxD, d)
				sumD += d
				sumSq += d * d
				n++
			}
			partialSortByDist(hits, k, func(h hit) float64 { return h.d })
			same := 0
			for i := 0; i < k && i < len(hits); i++ {
				if hits[i].c == qc {
					same++
				}
			}
			sameSum += float64(same) / float64(k)
		}
		mean := sumD / float64(n)
		out = append(out, SpreadReport{
			Spread:          sp,
			TopKSameCluster: sameSum / float64(queries),
			MinDist:         minD,
			MaxDist:         maxD,
			StdDevDist:      math.Sqrt(sumSq/float64(n) - mean*mean),
		})
	}
	return out
}

func cosineDist(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return 1 - dot // both operands are unit vectors
}

// partialSortByDist puts the k smallest elements first (selection sort; k is
// small and this is calibration-only code).
func partialSortByDist[T any](xs []T, k int, key func(T) float64) {
	if k > len(xs) {
		k = len(xs)
	}
	for i := range k {
		best := i
		for j := i + 1; j < len(xs); j++ {
			if key(xs[j]) < key(xs[best]) {
				best = j
			}
		}
		xs[i], xs[best] = xs[best], xs[i]
	}
}
