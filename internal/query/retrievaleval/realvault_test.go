package retrievaleval

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
)

// ollamaTable is the embedding table every real-vault sweep queries: the Ollama
// nomic-embed-text:v1.5 provider table carried in the dump.
const ollamaTable = "embeddings_ollama_nomic_embed_text_v1_5"

// realVault is the shared connection bundle for a real-vault sweep: the store,
// the Ollama vector provider, an Engine wired to it, and a pgxpool for the query
// set. Every gated real-vault test builds one via realVaultSetup so the ~20-line
// connect-and-wire dance lives in exactly one place.
type realVault struct {
	ctx  context.Context
	s    *store.Store
	prov *embed.Ollama
	e    *query.Engine
	pool *pgxpool.Pool
}

// realVaultSetup gates on COGNOSIS_GRAPHTUNE_DSN (an isolated dump, never the live
// DB; skipped in CI), connects the store, checks the Ollama vector provider, wires
// an Engine, and opens a pgxpool. Pool cleanup is registered on t. Fatal on any
// connection error.
func realVaultSetup(t *testing.T) *realVault {
	t.Helper()
	dsn := os.Getenv("COGNOSIS_GRAPHTUNE_DSN")
	if dsn == "" {
		t.Skip("set COGNOSIS_GRAPHTUNE_DSN to an isolated real-vault dump")
	}
	ctx := context.Background()
	s, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	prov := embed.NewOllama(envOr("OLLAMA_URL", "http://localhost:11434"),
		envOr("OLLAMA_MODEL", "nomic-embed-text:v1.5"))
	if err := prov.Health(ctx); err != nil {
		t.Fatalf("ollama health: %v", err)
	}
	e := &query.Engine{Store: s, Providers: []query.ProviderLeg{{Provider: prov, Table: ollamaTable}}}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &realVault{ctx: ctx, s: s, prov: prov, e: e, pool: pool}
}

// summaryQueries returns every note summary in path order -- the standard
// real-vault query set. Fatal if the dump has none.
func (rv *realVault) summaryQueries(t *testing.T) []string {
	t.Helper()
	rows, err := rv.pool.Query(rv.ctx, "select summary from notes where summary <> '' order by path")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	rows.Close()
	if len(out) == 0 {
		t.Fatal("no note summaries to query with")
	}
	return out
}

// projectedQuery is a note summary paired with the note's project, for the
// context-prior sweeps that key off the querying context's project.
type projectedQuery struct {
	text    string
	project string
}

// summaryQueriesWithProject is summaryQueries plus each note's project.
func (rv *realVault) summaryQueriesWithProject(t *testing.T) []projectedQuery {
	t.Helper()
	rows, err := rv.pool.Query(rv.ctx,
		"select summary, coalesce(project,'') from notes where summary <> '' order by path")
	if err != nil {
		t.Fatal(err)
	}
	var out []projectedQuery
	for rows.Next() {
		var q projectedQuery
		if err := rows.Scan(&q.text, &q.project); err != nil {
			t.Fatal(err)
		}
		out = append(out, q)
	}
	rows.Close()
	if len(out) == 0 {
		t.Fatal("no note summaries to query with")
	}
	return out
}
