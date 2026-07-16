package mcpserver

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/enqack/cognosis/internal/store"
)

// The /context endpoint backs `cognosis context inject`: the daemon (which
// owns the derived index and any in-flight lifecycle state) generates a
// truncated, project-scoped index for SessionStart injection; the CLI is just
// the authenticated transport. Sits behind the same bearer-token middleware
// as the MCP surface.

// contextPreamble frames the index that follows it. Without it the injection is
// a list of paths with no stated purpose — an agent can read it and still not
// know the vault is its own memory rather than project files to browse.
const contextPreamble = `# Cognosis

This project has a Cognosis vault — persistent memory across sessions, reachable through the
` + "`cognosis`" + ` MCP tools. It is not a file store to browse; it is where your own past findings live.

- Before deciding anything non-obvious, ` + "`query_knowledge`" + ` first — a past session may have already
  settled it, or already been wrong about it.
- When something durable surfaces (a decision, a gotcha, a dead end worth not re-walking),
  ` + "`write_note`" + ` it to ` + "`entries/`" + `. Capture in-session, not at the end.
- ` + "`compile_lifecycle`" + ` is the deliberate pass that reinforces, falsifies, and graduates. Nothing is
  inferred from mention alone.

The index below is what is already in the vault — paths only. Use ` + "`query_knowledge`" + ` to search it,
or ` + "`get_note`" + ` to read one in full.

`

// handleContext serves GET /context?project=<p>&budget=<tokens>.
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	budget := 2000
	if b := r.URL.Query().Get("budget"); b != "" {
		n, err := strconv.Atoi(b)
		if err != nil || n <= 0 {
			http.Error(w, "budget must be a positive integer", http.StatusBadRequest)
			return
		}
		budget = n
	}

	metas, err := s.store.ListNotes(r.Context(), project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), "context_inject", project, fmt.Sprintf("budget=%d", budget), nil)

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(renderContext(metas, project, budget)))
}

// renderContext builds the injected payload. Split from handleContext (which
// needs a live store) so the preamble, budget accounting, and truncation are
// reachable from a test without Postgres.
func renderContext(metas []store.NoteMeta, project string, budget int) string {
	var b strings.Builder
	// The preamble is the only place an agent is told what the vault is for.
	// A session starts cold, so this ships every session rather than once per
	// repo — no hook event offers once-per-repo semantics, and none could.
	//
	// It is exempt from the budget: fixed overhead that must reach every session,
	// not index content the caller is sizing. Budgeting it would mean a small
	// budget spends the whole allowance on framing and lists nothing — and
	// `context inject --budget 10` is asserted to stay small (see the platform
	// check). Measuring from base leaves the budget governing the index alone,
	// exactly as it did before the preamble existed.
	b.WriteString(contextPreamble)
	base := b.Len()
	if project != "" {
		fmt.Fprintf(&b, "# Cognosis knowledge index — project %s\n\n", project)
	} else {
		b.WriteString("# Cognosis knowledge index\n\n")
	}
	for _, m := range metas {
		line := fmt.Sprintf("- %s (%s, %s", m.Path, m.Category, m.Status)
		if m.Project != "" && project == "" {
			line += ", project " + m.Project
		}
		line += ", updated " + m.Updated.Format("2006-01-02") + ")\n"
		// Budget is tokens; ~4 chars per token is the standard approximation.
		if (b.Len()-base+len(line))/4 > budget {
			b.WriteString("- … (truncated to budget)\n")
			break
		}
		b.WriteString(line)
	}
	if len(metas) == 0 {
		b.WriteString("(vault is empty)\n")
	}
	return b.String()
}
