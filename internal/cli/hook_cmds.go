package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/vault"
)

// Opt-in git commit capture: a repo's post-commit hook shells out to
// `cognosis hook post-commit`, which records the commit as a structured
// entries/ note. The note is written directly into the vault — the sanctioned
// out-of-band path: the running daemon's watcher (or the next boot
// reconciliation) indexes and versions it like any hand-edit.

func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hook", Short: "Repo-side hook entry points (all marker-gated)"}

	postCommit := &cobra.Command{
		Use:   "post-commit",
		Short: "Record the current repo's latest commit as a vault entry (call from .git/hooks/post-commit)",
		Long: "Marker-gated like every hook: without a .cognosis-project marker above the working\n" +
			"directory this exits 0 silently. Opt-in per repo — install hooks/post-commit.sh into\n" +
			".git/hooks/ only where commit capture is wanted.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, found := resolveProjectMarker()
			if !found {
				return nil // unmarked repo: silent no-op
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			hash, subject, files, err := latestCommit()
			if err != nil {
				// A hook must not break the commit it observes.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cognosis hook post-commit: %v (commit not captured)\n", err)
				return nil
			}

			now := time.Now()
			rel := fmt.Sprintf("entries/commit-%s-%s.md", now.Format("2006-01-02"), hash[:12])
			content := fmt.Sprintf(`---
id: %s
category: entry
project: %s
created: %q
updated: %q
summary: %s
---
## Commit %s

%s

Files:
%s`,
				uuid.NewString(), project,
				now.Format(vault.TimeLayout), now.Format(vault.TimeLayout),
				yamlEscapeLine("commit "+hash[:12]+": "+subject),
				hash[:12], subject, files)

			abs := filepath.Join(cfg.KBPath, filepath.FromSlash(rel))
			if err := vault.WriteFileAtomic(abs, []byte(content)); err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cognosis hook post-commit: %v (commit not captured)\n", err)
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "captured %s -> %s\n", hash[:12], rel)
			return nil
		},
	}
	cmd.AddCommand(postCommit)
	return cmd
}

// latestCommit reads the hooked repo's newest commit — read-only, run by the
// operator's own git invocation.
func latestCommit() (hash, subject, files string, err error) {
	out, err := exec.Command("git", "log", "-1", "--format=%H%n%s").Output()
	if err != nil {
		return "", "", "", fmt.Errorf("git log: %w", err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(lines) < 2 || len(lines[0]) < 12 {
		return "", "", "", fmt.Errorf("unexpected git log output")
	}
	hash, subject = lines[0], lines[1]

	fout, err := exec.Command("git", "show", "--name-only", "--format=", "HEAD").Output()
	if err != nil {
		return "", "", "", fmt.Errorf("git show: %w", err)
	}
	var b strings.Builder
	for _, f := range strings.Split(strings.TrimSpace(string(fout)), "\n") {
		if f != "" {
			b.WriteString("- " + f + "\n")
		}
	}
	return hash, subject, b.String(), nil
}

// yamlEscapeLine quotes a summary line when YAML would misparse it bare.
func yamlEscapeLine(s string) string {
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`") {
		return fmt.Sprintf("%q", s)
	}
	return s
}
