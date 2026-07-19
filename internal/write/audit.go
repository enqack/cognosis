package write

import (
	"context"

	"github.com/enqack/cognosis/internal/vault"
)

// GraphAudit is the result of comparing the link graph the index holds against
// the one its own note content implies.
type GraphAudit struct {
	Notes   int      // notes examined
	Edges   int      // edges the index holds
	Missing int      // edges the content implies that the index does not have
	Extra   int      // edges the index holds that the content does not imply
	Sample  []string // a few offending paths, for a status line that can be acted on
}

// OK reports whether the index agrees with its content.
func (g GraphAudit) OK() bool { return g.Missing == 0 && g.Extra == 0 }

// AuditGraph re-derives every note's outbound links and diffs them against the
// stored edges.
//
// This is the one form of index corruption nothing else notices. Notes, chunks
// and embeddings can all be correct while the graph is missing edges, because
// links are resolved once at index time and are not re-derived afterwards: a
// note indexed before its target loses that edge, and reconciliation skips it
// forever after since its content hash never changes. An editor's atomic save
// used to cause exactly this — measured at 7 edges down to 6 — while every
// health check reported ok.
//
// It re-derives through the indexer's own resolveLinks rather than a private
// copy. A separate implementation would eventually disagree with the real one,
// and then the audit would report on a graph nobody builds.
//
// The comparison is index-against-index: content from the notes table versus
// edges in the links table. That is the right scope, because the failure being
// hunted leaves content correct and only loses edges. Divergence between the
// vault and the index is a different question, and reconciliation already owns
// it.
func (ix *Indexer) AuditGraph(ctx context.Context) (GraphAudit, error) {
	var g GraphAudit
	notes, err := ix.Store.AllReferrers(ctx)
	if err != nil {
		return g, err
	}
	const maxSample = 5
	for _, r := range notes {
		stage, ok := vault.StageOf(r.Path)
		if !ok {
			continue
		}
		g.Notes++

		n := &vault.Note{Path: r.Path, Stage: stage, Frontmatter: r.Frontmatter, Body: r.Body}
		want, err := ix.resolveLinks(ctx, n)
		if err != nil {
			return g, err
		}
		have, err := ix.Store.LinkDsts(ctx, r.ID)
		if err != nil {
			return g, err
		}
		g.Edges += len(have)

		// Compare destinations only. resolveLinks already drops dangling refs
		// and self-links, so anything it returns should have an edge.
		haveSet := make(map[string]bool, len(have))
		for _, id := range have {
			haveSet[id.String()] = true
		}
		wantSet := make(map[string]bool, len(want))
		missing := 0
		for _, l := range want {
			wantSet[l.Dst.String()] = true
			if !haveSet[l.Dst.String()] {
				missing++
			}
		}
		extra := 0
		for _, id := range have {
			if !wantSet[id.String()] {
				extra++
			}
		}
		g.Missing += missing
		g.Extra += extra
		if (missing > 0 || extra > 0) && len(g.Sample) < maxSample {
			g.Sample = append(g.Sample, r.Path)
		}
	}
	return g, nil
}
