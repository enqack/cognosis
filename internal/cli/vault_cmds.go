package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
)

func newVaultCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "vault", Short: "Vault history and recovery"}

	history := &cobra.Command{
		Use:   "history <path>",
		Short: "Show a note's version history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			lines, err := vault.NewHistory(cfg.KBPath).Log(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(lines) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "no history for %s\n", args[0])
				return nil
			}
			for _, l := range lines {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), l)
			}
			return nil
		},
	}

	restore := &cobra.Command{
		Use:   "restore <path>",
		Short: "Restore a note to a prior version (a restore is itself a new commit)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			at, _ := cmd.Flags().GetString("at")
			if at == "" {
				return fmt.Errorf("--at <ref> is required (a commit hash from `cognosis vault history`)")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if err := vault.NewHistory(cfg.KBPath).Restore(cmd.Context(), at, args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "restored %s @ %s (the running daemon's watcher reindexes it)\n", args[0], at)
			return nil
		},
	}
	restore.Flags().String("at", "", "git ref or commit hash to restore to")

	cmd.AddCommand(history, restore)
	return cmd
}

func newNoteCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "note", Short: "Note administration"}
	del := &cobra.Command{
		Use:   "delete <path>",
		Short: "Hard-delete a note everywhere — irreversible",
		Long: "Genuine erasure: removes the file, purges the derived index (chunks, links, every\n" +
			"embedding table via cascade), rewrites log.md mentions to tombstones, and erases the\n" +
			"path from vault history. For cases where 'excluded from retrieval' isn't sufficient.\n" +
			"Postgres WAL segments and any operator-taken backups are outside Cognosis's reach —\n" +
			"purging those is on you.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			yes, _ := cmd.Flags().GetBool("yes")
			if !hard {
				return fmt.Errorf("soft-delete (archival) happens through compile_lifecycle; pass --hard to acknowledge genuine erasure")
			}
			if !yes {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Hard-delete %s everywhere? This cannot be undone — not by re-index, not by history. [y/N] ", args[0])
				reader := bufio.NewReader(cmd.InOrStdin())
				line, _ := reader.ReadString('\n')
				if strings.TrimSpace(strings.ToLower(line)) != "y" {
					return fmt.Errorf("aborted")
				}
			}
			return hardDelete(cmd, args[0])
		},
	}
	del.Flags().Bool("hard", false, "required: acknowledge this is genuine erasure")
	del.Flags().Bool("yes", false, "skip interactive confirmation (scripting)")
	cmd.AddCommand(del)
	return cmd
}

func hardDelete(cmd *cobra.Command, rel string) error {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if _, ok := vault.StageOf(rel); !ok {
		return cogerr.Ef("cli.hardDelete", cogerr.Validation, "%q is not a vault note path", rel)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return withStore(cmd, func(ctx context.Context, s *store.Store) error {
		// 1. The derived index: notes row cascades chunks, links, and every
		// provider embedding table.
		if err := s.DeleteNote(ctx, rel); err != nil {
			return err
		}
		// 2. The file itself.
		abs := filepath.Join(cfg.KBPath, filepath.FromSlash(rel))
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
		// 3. log.md tombstones: the append-only log yields to erasure here —
		// the one operation allowed to rewrite it.
		base := strings.TrimSuffix(filepath.Base(rel), ".md")
		if err := tombstoneLog(cfg.KBPath, base); err != nil {
			return err
		}
		// 4. Vault history: every prior version of the note is purged.
		if err := vault.NewHistory(cfg.KBPath).PurgePath(ctx, rel); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"hard-deleted %s: file, index, embeddings, log.md mentions, history.\n"+
				"Outside Cognosis's reach: Postgres WAL segments and any backups (pg_dump,\n"+
				"filesystem snapshots) may still hold this content — purge those yourself.\n", rel)
		return nil
	})
}

// tombstoneLog replaces log.md lines mentioning the erased note with a
// tombstone, preserving the audit shape of the log without the content.
func tombstoneLog(vaultDir, basename string) error {
	logPath := filepath.Join(vaultDir, "log.md")
	b, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	needle := "[[" + basename + "]]"
	lines := strings.Split(string(b), "\n")
	changed := false
	for i, l := range lines {
		if strings.Contains(l, needle) {
			lines[i] = "- [hard-deleted " + basename + "]"
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return vault.WriteFileAtomic(logPath, []byte(strings.Join(lines, "\n")))
}
