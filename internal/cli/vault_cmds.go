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
			forceLocal, _ := cmd.Flags().GetBool("force-local")
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return runRestore(cmd, cfg, args[0], at, forceLocal)
		},
	}
	restore.Flags().String("at", "", "git ref or commit hash to restore to")
	restore.Flags().Bool("force-local", false,
		"write the vault directly even when a daemon owns it (bypasses the daemon's per-path lock)")

	cmd.AddCommand(history, restore)
	return cmd
}

// runRestore lands a restore through whichever door is safe.
//
// The daemon and this command write the same files. Only the daemon's door
// (the restore_note tool) normalises the path and takes the per-path lock the
// pipeline and the lifecycle engine share, so a direct write races a compile
// pass over the same note and whichever lands second wins.
//
// Which door is safe is decided by asking Postgres who holds the instance
// lock, not by looking for a local PID file — a daemon on another host owns
// this vault's database and is invisible to the file. See probeDaemon.
func runRestore(cmd *cobra.Command, cfg *config.Config, path, at string, forceLocal bool) error {
	out := cmd.OutOrStdout()

	if forceLocal {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"warning: --force-local writes the vault directly, bypassing the daemon's per-path lock; "+
				"a concurrent compile or edit of this note can be lost")
		return restoreLocally(cmd, cfg, path, at)
	}

	switch probeDaemon(cmd.Context(), cfg) {
	case daemonAbsent:
		// Nothing else owns the vault, so the direct write is the safe write.
		return restoreLocally(cmd, cfg, path, at)

	case daemonPresent:
		msg, err := callDaemonTool(cmd.Context(), cfg, "restore_note", map[string]any{
			"path": path, "ref": at,
		})
		if err != nil {
			// Deliberately not falling back. A daemon owns this vault, so a
			// direct write here is the race — and the moment to be most careful
			// is when the thing that owns your data is already misbehaving.
			return fmt.Errorf("a daemon owns this vault and the restore could not be routed through it: %w\n"+
				"  stop the daemon, or re-run with --force-local to write directly anyway", err)
		}
		_, _ = fmt.Fprintf(out, "%s (via the running daemon)\n", strings.TrimSpace(msg))
		return nil

	case daemonUnknown:
		// Could not reach Postgres, so "no daemon" is unproven. Try the daemon's
		// HTTP door: if it answers, there is one and this is safe; if it does
		// not, refuse rather than guess.
		msg, err := callDaemonTool(cmd.Context(), cfg, "restore_note", map[string]any{
			"path": path, "ref": at,
		})
		if err == nil {
			_, _ = fmt.Fprintf(out, "%s (via the running daemon)\n", strings.TrimSpace(msg))
			return nil
		}
		return fmt.Errorf("cannot tell whether a daemon owns this vault: Postgres is unreachable and "+
			"the daemon did not answer (%v)\n"+
			"  re-run with --force-local to write directly, accepting that a running daemon could be "+
			"writing the same note", err)
	}
	// Unreachable: the switch covers every daemonOwnership value, and the
	// exhaustive linter keeps it that way. Go still needs a terminating
	// statement here since the switch has no default.
	return nil
}

// restoreLocally is the original direct path: write the file, commit, and let
// the daemon's watcher reindex it if one is running.
func restoreLocally(cmd *cobra.Command, cfg *config.Config, path, at string) error {
	if err := vault.NewHistory(cfg.KBPath).Restore(cmd.Context(), at, path); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"restored %s @ %s (the running daemon's watcher reindexes it)\n", path, at)
	return nil
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
		// Hard deletion writes Postgres rows, removes the file, rewrites log.md
		// and purges the path from git history — none of it under the per-path
		// lock the daemon's own writers share, and with no MCP equivalent to
		// route it through.
		//
		// Unlike `token` and `embeddings`, whose DB-direct writes are a
		// deliberate coordination medium a running daemon polls, nothing here is
		// meant to be observed by a live daemon: it would see a file vanish and
		// a history rewritten under it.
		if err := refuseIfDaemonOwns(ctx, s, "a hard delete"); err != nil {
			return err
		}
		hist := vault.NewHistory(cfg.KBPath)

		// Order matters, and it used to be wrong. The history rewrite is the
		// step that fails for environmental reasons — git refuses to rewrite a
		// dirty tree — and it ran last, after the row, the file and log.md were
		// already gone. A failure left the note erased from the vault but still
		// present in history: the opposite of what was asked for, and a state no
		// retry could reach.
		//
		// So the fragile step goes first. If it fails now, nothing has been
		// destroyed and the command can simply be run again.

		// 1. Vault history: every prior version of the note is purged.
		if err := hist.PurgePath(ctx, rel); err != nil {
			return err
		}
		// 2. The derived index: notes row cascades chunks, links, and every
		// provider embedding table.
		if err := s.DeleteNote(ctx, rel); err != nil {
			return err
		}
		// 3. The file itself.
		abs := filepath.Join(cfg.KBPath, filepath.FromSlash(rel))
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
		// 4. log.md tombstones: the append-only log yields to erasure here —
		// the one operation allowed to rewrite it.
		base := strings.TrimSuffix(filepath.Base(rel), ".md")
		if err := tombstoneLog(cfg.KBPath, base); err != nil {
			return err
		}
		// 5. Commit the removal and the tombstoned log. Without this the vault
		// repo is left dirty and the erasure only lands whenever something else
		// happens to commit — which, with the daemon stopped, may be never.
		// The purged content is already gone from history, so this commit
		// records the deletion and nothing more.
		if err := hist.CommitAll(ctx, "hard-delete: "+rel); err != nil {
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
