package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/lifecycle"
)

type compileArgs struct {
	Reinforce []string          `json:"reinforce,omitempty" jsonschema:"note ids or vault-relative paths to reinforce (explicit, justified re-assertion)"`
	Falsify   map[string]string `json:"falsify,omitempty" jsonschema:"note id/path -> reason it was determined false (terminal; retained but frozen)"`
	Dispute   map[string]string `json:"dispute,omitempty" jsonschema:"note id/path -> reason it is contested (live, keeps decaying; a later reinforce clears it)"`
	Supersede map[string]string `json:"supersede,omitempty" jsonschema:"falsified note id/path -> the replacement note id/path"`
	Graduate  []string          `json:"graduate,omitempty" jsonschema:"stable note ids/paths to canonize in place (stops decaying)"`
	Verify    bool              `json:"verify,omitempty" jsonschema:"retrieval-augmented pass: surface the notes most related to each falsify/graduate target as advisory related-context lines before the move is final"`
	DryRun    bool              `json:"dry_run,omitempty" jsonschema:"report what would happen without writing anything"`
}

type listPersonasArgs struct{}

type getPersonaArgs struct {
	ID string `json:"id" jsonschema:"persona id from list_personas"`
}

type writeReflectionArgs struct {
	Persona     string `json:"persona" jsonschema:"an enabled persona id (see list_personas)"`
	Description string `json:"description" jsonschema:"dry, literal one-sentence summary of the event — this is what gets embedded; the styled body is never indexed"`
	Content     string `json:"content" jsonschema:"the reflection body, written in the persona's voice"`
	Project     string `json:"project,omitempty" jsonschema:"optional project tag"`
	Summary     string `json:"summary,omitempty" jsonschema:"optional one-liner cached and returned with retrieval hits (defaults to nothing; the description already covers embedding)"`
}

func (s *Server) addLifecycleTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "compile_lifecycle",
		Description: "Record what you learned about knowledge you ALREADY have — that it held up, that it was wrong, " +
			"that it is now contested. Reach for this instead of write_note whenever an existing note is the subject: " +
			"finding a note's claim false is falsify, not a new note saying the opposite; doubting it is dispute, which " +
			"keeps it decaying until a later reinforce clears it. Writing a fresh note instead leaves the old claim " +
			"live and retrievable, so retrieval returns both and neither is marked. " +
			"reinforce/falsify/dispute/graduate are agent-justified inputs; decay and archival are automatic. " +
			"Nothing is inferred from mention alone; dry_run previews without writing.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args compileArgs) (*mcp.CallToolResult, any, error) {
		report, err := s.lifecycle.Run(ctx, lifecycle.Options{
			Reinforce: args.Reinforce,
			Falsify:   args.Falsify,
			Dispute:   args.Dispute,
			Supersede: args.Supersede,
			Graduate:  args.Graduate,
			Verify:    args.Verify,
			DryRun:    args.DryRun,
		})
		s.audit(ctx, "compile_lifecycle", "",
			fmt.Sprintf("reinforce=%d falsify=%d dispute=%d graduate=%d dry_run=%v",
				len(args.Reinforce), len(args.Falsify), len(args.Dispute), len(args.Graduate), args.DryRun), err)
		if err != nil {
			return nil, nil, s.toolError(req, err)
		}
		s.log.Info("compile_lifecycle", "actions", len(report.Actions), "dry_run", args.DryRun)
		return textResult(report.String()), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_personas",
		Description: "Lightweight persona discovery: id, name, one-sentence description, and chain hints per enabled persona. Cheap enough to call just to check what's available.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listPersonasArgs) (*mcp.CallToolResult, any, error) {
		metas := s.personas.List()
		if len(metas) == 0 {
			return textResult("No personas enabled."), nil, nil
		}
		var b strings.Builder
		for _, m := range metas {
			fmt.Fprintf(&b, "- %s (%s): %s", m.ID, m.Name, m.Description)
			if len(m.RespondsTo) > 0 {
				fmt.Fprintf(&b, " [responds_to: %s]", strings.Join(m.RespondsTo, ", "))
			}
			b.WriteString("\n")
		}
		return textResult(b.String()), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_persona",
		Description: "Fetch one persona's full voice guide (structure, checklist) — call only once you've decided that persona fits the moment.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getPersonaArgs) (*mcp.CallToolResult, any, error) {
		if args.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}
		p, err := s.personas.Get(args.ID)
		if err != nil {
			return nil, nil, s.toolError(req, err)
		}
		return textResult(p.Body), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_migration_status",
		Description: "Progress of the embedding-provider migration (backfill/lazy split, ETA, paused state) — check before large write batches; pause/resume/rollback are operator CLI actions.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listPersonasArgs) (*mcp.CallToolResult, any, error) {
		if s.Migrations == nil {
			return textResult("Migration subsystem not wired."), nil, nil
		}
		st, err := s.Migrations.GetStatus(ctx)
		s.audit(ctx, "get_migration_status", "", "", err)
		if err != nil {
			if cogerr.Is(err, cogerr.NotFound) {
				return textResult("No migration in progress."), nil, nil
			}
			return nil, nil, s.toolError(req, err)
		}
		return textResult(st.String()), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_reflection",
		Description: "Write a persona-authored reflection into reflections/. The description (not the styled body) is what gets embedded.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args writeReflectionArgs) (*mcp.CallToolResult, any, error) {
		if args.Persona == "" || args.Content == "" {
			return nil, nil, fmt.Errorf("persona and content are required")
		}
		rel, err := s.personas.WriteReflection(ctx, s.pipeline, args.Persona, args.Description, args.Content, args.Project, args.Summary)
		s.audit(ctx, "write_reflection", args.Project, "persona="+args.Persona+" path="+rel, err)
		if err != nil {
			return nil, nil, s.toolError(req, err)
		}
		s.log.Info("write_reflection", "persona", args.Persona, "path", rel)
		return textResult("written: " + rel), nil, nil
	})
}
