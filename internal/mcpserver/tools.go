package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
)

// Tool argument structs; json + jsonschema tags drive the generated input
// schemas.

type writeNoteArgs struct {
	Path    string `json:"path" jsonschema:"vault-relative path under entries/, notes/, reflections/, or archive/ (e.g. entries/2026-07-12-capture.md)"`
	Content string `json:"content" jsonschema:"full markdown file content including YAML frontmatter satisfying the contract (id, category, created, updated, ...). id must be a UUIDv7 (time-ordered) — v4 is rejected. An optional one-line summary: key is cached and returned with every retrieval hit."`
	Project string `json:"project,omitempty" jsonschema:"optional cross-check: must match the note's own frontmatter project tag when set"`
}

type queryArgs struct {
	Text             string `json:"text" jsonschema:"natural-language search text"`
	Project          string `json:"project,omitempty" jsonschema:"optional project filter"`
	TopK             int    `json:"top_k,omitempty" jsonschema:"number of results, default 8"`
	IncludeFalsified bool   `json:"include_falsified,omitempty" jsonschema:"include retained-but-invalidated (falsified) notes; default false"`
	IncludeArchived  bool   `json:"include_archived,omitempty" jsonschema:"include soft-deleted (faded/archived) notes; default false — archived concepts are shelved and stay out of ordinary retrieval"`
	PersonaFilter    string `json:"persona_filter,omitempty" jsonschema:"optional enabled persona id whose category bias reweights fused results (a lens, never a hard filter)"`
	AsOf             string `json:"as_of,omitempty" jsonschema:"optional YYYY-MM-DD HH:MM:SS — answer from what the KB believed at that instant: notes created later vanish, notes falsified later count as still believed. Content is always current; use vault history to recover past content."`
}

type listDecayingArgs struct {
	ThresholdDays int    `json:"threshold_days" jsonschema:"list decaying notes whose last reinforcement is at least this many days old"`
	Project       string `json:"project,omitempty" jsonschema:"optional project filter"`
}

type listNotesArgs struct {
	Project string `json:"project,omitempty" jsonschema:"optional project filter"`
}

type vaultHistoryArgs struct {
	Path  string `json:"path,omitempty" jsonschema:"optional vault-relative path to scope the history to; omit for the whole-vault commit log"`
	Limit int    `json:"limit,omitempty" jsonschema:"max commits to return, default 10"`
}

type restoreNoteArgs struct {
	Path string `json:"path" jsonschema:"vault-relative path to restore"`
	Ref  string `json:"ref" jsonschema:"commit hash to restore the file to (from vault_history)"`
}

type getNoteArgs struct {
	Path string `json:"path" jsonschema:"vault-relative note path"`
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func (s *Server) addTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "write_note",
		Description: "Persist a finding to the vault so it survives this session — decisions, gotchas, dead ends worth not re-walking, " +
			"anything a future session would waste time rediscovering. Raw capture goes in entries/. " +
			"Validates the frontmatter contract, versions the write, chunks, embeds, and indexes it atomically.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args writeNoteArgs) (*mcp.CallToolResult, any, error) {
		if args.Path == "" || args.Content == "" {
			return nil, nil, fmt.Errorf("path and content are required")
		}
		err := s.pipeline.Write(ctx, args.Path, args.Content, args.Project)
		s.audit(ctx, "write_note", args.Project, "path="+args.Path, err)
		if err != nil {
			return nil, nil, err
		}
		s.log.Info("write_note", "path", args.Path, "project", args.Project)
		return textResult("written: " + args.Path), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "query_knowledge",
		Description: "Search your own past findings before deciding something non-obvious — a previous session may have already " +
			"settled it, or already been wrong about it. Hybrid semantic + keyword + link-graph retrieval, RRF-fused. " +
			"Falsified notes are retained but excluded unless include_falsified.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args queryArgs) (*mcp.CallToolResult, any, error) {
		if args.Text == "" {
			return nil, nil, fmt.Errorf("text is required")
		}
		var bias map[string]float64
		if args.PersonaFilter != "" {
			p, err := s.personas.Get(args.PersonaFilter)
			if err != nil {
				return nil, nil, err
			}
			bias = p.Bias
		}
		var asOf *time.Time
		if args.AsOf != "" {
			t, err := vault.ParseTime(args.AsOf)
			if err != nil {
				return nil, nil, fmt.Errorf("as_of: %w", err)
			}
			asOf = &t
		}
		results, err := s.engine.Run(ctx, args.Text, query.Options{
			Project:          args.Project,
			TopK:             args.TopK,
			IncludeFalsified: args.IncludeFalsified,
			IncludeArchived:  args.IncludeArchived,
			CategoryBias:     bias,
			AsOf:             asOf,
		})
		s.audit(ctx, "query_knowledge", args.Project,
			fmt.Sprintf("text_len=%d top_k=%d include_falsified=%v include_archived=%v persona_filter=%s",
				len(args.Text), args.TopK, args.IncludeFalsified, args.IncludeArchived, args.PersonaFilter), err)
		if err != nil {
			return nil, nil, err
		}
		s.log.Info("query_knowledge", "results", len(results))

		// Tell the agent when suppressed history exists. Falsified notes are
		// retained deliberately, but the exclusion happens in SQL, so without
		// this an agent in an unusual context sees nothing and reinvents what
		// the vault already stopped believing. Best-effort: a failed count must
		// not fail the query it annotates.
		out := Format(results)
		if n, cerr := s.store.CountSuppressedFalsified(ctx, args.Text, store.Filter{
			Project:          args.Project,
			IncludeFalsified: args.IncludeFalsified,
		}); cerr == nil && n > 0 {
			out += fmt.Sprintf("\n(at least %d falsified note(s) also matched and were excluded — "+
				"pass include_falsified to see what was ruled out and why.)\n", n)
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_notes",
		Description: "Enumerate what the vault holds (path, category, status, project, updated) without content — the cheap way " +
			"to see coverage, or to find a path worth passing to get_note. To search by meaning rather than browse, use query_knowledge.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listNotesArgs) (*mcp.CallToolResult, any, error) {
		metas, err := s.store.ListNotes(ctx, args.Project)
		s.audit(ctx, "list_notes", args.Project, "", err)
		if err != nil {
			return nil, nil, err
		}
		if len(metas) == 0 {
			return textResult("No notes."), nil, nil
		}
		var b strings.Builder
		for _, m := range metas {
			fmt.Fprintf(&b, "- %s (%s, %s", m.Path, m.Category, m.Status)
			if m.Project != "" {
				fmt.Fprintf(&b, ", project %s", m.Project)
			}
			fmt.Fprintf(&b, ", updated %s)", m.Updated.Format("2006-01-02 15:04:05"))
			if m.Summary != "" {
				fmt.Fprintf(&b, " — %s", m.Summary)
			}
			b.WriteString("\n")
		}
		return textResult(b.String()), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_decaying",
		Description: "Surface decaying notes approaching staleness under the existing decay rules — the shortlist to feed compile_lifecycle's reinforce. Visibility only; shielded notes (paused, graduated, falsified) never appear.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listDecayingArgs) (*mcp.CallToolResult, any, error) {
		if args.ThresholdDays <= 0 {
			return nil, nil, fmt.Errorf("threshold_days must be positive")
		}
		cutoff := time.Now().AddDate(0, 0, -args.ThresholdDays)
		notes, err := s.store.ListDecaying(ctx, cutoff, args.Project)
		s.audit(ctx, "list_decaying", args.Project, fmt.Sprintf("threshold_days=%d", args.ThresholdDays), err)
		if err != nil {
			return nil, nil, err
		}
		if len(notes) == 0 {
			return textResult("No decaying notes past the threshold."), nil, nil
		}
		var b strings.Builder
		for _, d := range notes {
			// Both timestamps, because they answer different questions and
			// diverge exactly when it matters: last_reinforced moves on
			// passive citation refresh and on decay, while last asserted moves
			// only when an agent actually reinforced. A note whose "asserted"
			// is far older than its "reinforced" is one citations have been
			// carrying — the ones most worth a deliberate look.
			fmt.Fprintf(&b, "- %s (confidence %.1f, %s, last asserted %s, clock %s",
				d.Path, d.Confidence, d.Maturity, d.LastAsserted, d.LastReinforced)
			if d.Project != "" {
				fmt.Fprintf(&b, ", project %s", d.Project)
			}
			b.WriteString(")\n")
		}
		return textResult(b.String()), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_note",
		Description: "Read one note in full (frontmatter + body) once query_knowledge or list_notes has told you which path you want. " +
			"Retrieval returns matching chunks; this returns the whole file.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getNoteArgs) (*mcp.CallToolResult, any, error) {
		if args.Path == "" {
			return nil, nil, fmt.Errorf("path is required")
		}
		content, err := s.readNoteFile(args.Path)
		s.audit(ctx, "get_note", "", "path="+args.Path, err)
		if err != nil {
			return nil, nil, err
		}
		return textResult(content), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "vault_history",
		Description: "Read the auto-managed vault git history — the invisible recovery net behind every write and compile pass. " +
			"Omit path for the whole-vault commit log; pass a path to scope it. Each entry carries the commit hash to feed restore_note. " +
			"Lets you mediate a rollback (\"I changed that file at 2pm; want me to restore the 1:50pm version?\") without the operator touching a terminal.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args vaultHistoryArgs) (*mcp.CallToolResult, any, error) {
		hist := vault.NewHistory(s.vaultDir)
		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}
		var out string
		var err error
		if strings.TrimSpace(args.Path) != "" {
			var lines []string
			lines, err = hist.Log(ctx, args.Path)
			if err == nil {
				if len(lines) == 0 {
					out = "No history for " + args.Path
				} else {
					if len(lines) > limit {
						lines = lines[:limit]
					}
					out = "History for " + args.Path + " (newest first):\n" + strings.Join(lines, "\n")
				}
			}
		} else {
			var commits []vault.Commit
			commits, err = hist.LogAll(ctx, limit)
			if err == nil {
				out = formatCommits(commits)
			}
		}
		s.audit(ctx, "vault_history", "", fmt.Sprintf("path=%s limit=%d", args.Path, limit), err)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "restore_note",
		Description: "Restore a note to a prior state from the vault history (ref from vault_history). The restore is itself a new commit — history moves forward, never rewritten — and the running daemon reindexes the restored file. Use it to undo a bad edit the operator flagged.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args restoreNoteArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Path) == "" || strings.TrimSpace(args.Ref) == "" {
			return nil, nil, fmt.Errorf("path and ref are both required")
		}
		err := vault.NewHistory(s.vaultDir).Restore(ctx, args.Ref, args.Path)
		s.audit(ctx, "restore_note", "", "path="+args.Path+" ref="+args.Ref, err)
		if err != nil {
			return nil, nil, err
		}
		return textResult(fmt.Sprintf("restored %s to %s (reindexed live by the daemon)", args.Path, args.Ref)), nil, nil
	})

	s.addLifecycleTools(srv)
}

// formatCommits renders the whole-vault history for the vault_history tool.
func formatCommits(commits []vault.Commit) string {
	if len(commits) == 0 {
		return "No history yet."
	}
	var b strings.Builder
	b.WriteString("Recent revertable states (newest first):\n")
	for _, c := range commits {
		short := c.Hash
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(&b, "- %s  %s  %s\n", short, c.When, c.Subject)
		for _, p := range c.Paths {
			fmt.Fprintf(&b, "    %s\n", p)
		}
	}
	return b.String()
}

// Format renders retrieval results as markdown — agents consume text better
// than JSON.
func Format(results []query.Result) string {
	if len(results) == 0 {
		return "No results."
	}
	var b strings.Builder
	for i, r := range results {
		heading := ""
		if r.HeadingPath != "" {
			heading = " › " + r.HeadingPath
		}
		fmt.Fprintf(&b, "### %d. %s%s (%s, score %.4f)\n\n", i+1, r.Path, heading, r.Category, r.Score)
		if r.Summary != "" {
			fmt.Fprintf(&b, "*%s*\n\n", r.Summary)
		}
		fmt.Fprintf(&b, "%s\n\n", snippet(r.Content))
	}
	return b.String()
}

// snippet caps content at 700 chars, preferring to cut at a newline unless
// that would drop more than half the budget.
func snippet(s string) string {
	const snippetMax = 700
	if len(s) <= snippetMax {
		return s
	}
	cut := s[:snippetMax]
	if i := strings.LastIndexByte(cut, '\n'); i >= snippetMax/2 {
		cut = cut[:i]
	}
	return cut + "\n…"
}
