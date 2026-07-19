package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/config"
)

// resolveProjectMarker walks up from cwd looking for a .cognosis-project
// marker file (one line: the project tag). Found -> tag; not found -> "".
// The marker travels with the repo, so cloning to a new machine or path
// needs no config update.
func resolveProjectMarker() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, ".cognosis-project"))
		if err == nil {
			return strings.TrimSpace(string(b)), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "context", Short: "Session-context helpers"}

	inject := &cobra.Command{
		Use:   "inject",
		Short: "Print a truncated, project-scoped index for a SessionStart hook",
		Long: "The marker gates the hook entirely: without a .cognosis-project marker (and no\n" +
			"explicit --project) this exits 0 silently, never contacting the daemon -- a stopped\n" +
			"daemon must not block sessions in repos that have nothing to do with Cognosis.\n" +
			"In marked repos the failure mode is loud: daemon unreachable within 2s -> nonzero exit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, _ := cmd.Flags().GetString("project")
			budget, _ := cmd.Flags().GetInt("budget")

			if project == "" {
				marker, found := resolveProjectMarker()
				if !found {
					return nil // unmarked repo: silent no-op, daemon never contacted
				}
				project = marker
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			token, err := localToken(cfg)
			if err != nil {
				return err
			}

			u := url.URL{
				Scheme: "http",
				Host:   cfg.BindAddress,
				Path:   "/context",
				RawQuery: url.Values{
					"project": {project},
					"budget":  {strconv.Itoa(budget)},
				}.Encode(),
			}
			client := &http.Client{Timeout: 2 * time.Second}
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, u.String(), nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := client.Do(req)
			if err != nil {
				// Fail loud, block session start: a context-less session that
				// looks normal is worse than one that visibly fails.
				return fmt.Errorf("cognosis daemon unreachable at %s (is it running?): %w", cfg.BindAddress, err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), string(body))
			return nil
		},
	}
	inject.Flags().String("project", "", "project tag (resolved from .cognosis-project when omitted)")
	inject.Flags().Int("budget", 2000, "token budget for the injected index")
	cmd.AddCommand(inject)
	return cmd
}

// localToken resolves the bearer token: COGNOSIS_TOKEN env wins, else the
// daemon's auto-minted local token file.
func localToken(cfg *config.Config) (string, error) {
	if t := strings.TrimSpace(os.Getenv("COGNOSIS_TOKEN")); t != "" {
		return t, nil
	}
	b, err := os.ReadFile(cfg.Paths().TokenFile())
	if err != nil {
		return "", fmt.Errorf("no token: set COGNOSIS_TOKEN or start the daemon once (it mints %s)", cfg.Paths().TokenFile())
	}
	return strings.TrimSpace(string(b)), nil
}
