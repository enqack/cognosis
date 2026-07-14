package mcpserver

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The /context endpoint backs `cognosis context inject`: the daemon (which
// owns the derived index and any in-flight lifecycle state) generates a
// truncated, project-scoped index for SessionStart injection; the CLI is just
// the authenticated transport. Sits behind the same bearer-token middleware
// as the MCP surface.

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

	var b strings.Builder
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
		if (b.Len()+len(line))/4 > budget {
			b.WriteString("- … (truncated to budget)\n")
			break
		}
		b.WriteString(line)
	}
	if len(metas) == 0 {
		b.WriteString("(vault is empty)\n")
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
