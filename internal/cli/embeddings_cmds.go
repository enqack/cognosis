package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/migrate"
	"github.com/enqack/cognosis/internal/store"
)

// coordinator builds a CLI-side migration coordinator. Control actions are
// DB-direct (like token management): the state row is the coordination
// medium, and the running daemon's worker picks the work up.
func coordinator(cfg *config.Config, s *store.Store) *migrate.Coordinator {
	factory := func(name, model string) (embed.Provider, error) {
		if name != "ollama" {
			return nil, fmt.Errorf("unknown embedding provider %q (only ollama is wired)", name)
		}
		return embed.NewOllama(cfg.Embedding.URL, model), nil
	}
	return &migrate.Coordinator{Store: s, Factory: factory}
}

func newEmbeddingsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "embeddings", Short: "Embedding provider management"}

	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Zero-downtime provider migration (the daemon's worker does the back-fill)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			from, _ := cmd.Flags().GetString("from")
			to, _ := cmd.Flags().GetString("to")
			pause, _ := cmd.Flags().GetBool("pause")
			resume, _ := cmd.Flags().GetBool("resume")
			rollback, _ := cmd.Flags().GetBool("rollback")
			dryRun, _ := cmd.Flags().GetBool("dry-run")

			return withStore(cmd, func(ctx context.Context, s *store.Store) error {
				c := coordinator(cfg, s)
				switch {
				case pause:
					if err := c.Pause(ctx); err != nil {
						return err
					}
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "paused (lazy migration and dual-embedding continue)")
					return nil
				case resume:
					if err := c.Resume(ctx); err != nil {
						return err
					}
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "resumed")
					return nil
				case rollback:
					if err := c.Rollback(ctx); err != nil {
						return err
					}
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "rolled back; the half-migrated table stays for a later attempt (prune to drop it)")
					return nil
				}
				if from == "" || to == "" {
					return fmt.Errorf("--from and --to are required (as <name>/<model>), or one of --pause/--resume/--rollback")
				}
				plan, err := c.Start(ctx, from, to, dryRun)
				if err != nil {
					return err
				}
				if dryRun {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"dry run — would migrate %d chunks:\n  from %s/%s (dim %d, table %s)\n  to   %s/%s (dim %d, table %s)\nnothing written.\n",
						plan.ChunksTotal,
						plan.From.Name, plan.From.Model, plan.From.Dimension, plan.From.Table,
						plan.To.Name, plan.To.Model, plan.To.Dimension, plan.To.Table)
					return nil
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"migration started (%d chunks); the daemon's worker is back-filling — watch with `cognosis embeddings status`\n",
					plan.ChunksTotal)
				return nil
			})
		},
	}
	migrateCmd.Flags().String("from", "", "source provider as <name>/<model>")
	migrateCmd.Flags().String("to", "", "target provider as <name>/<model>")
	migrateCmd.Flags().Bool("pause", false, "pause the in-progress migration's back-fill")
	migrateCmd.Flags().Bool("resume", false, "resume a paused migration")
	migrateCmd.Flags().Bool("rollback", false, "roll back (immediate; half-migrated table kept)")
	migrateCmd.Flags().Bool("dry-run", false, "report the plan without writing anything")

	status := &cobra.Command{
		Use:   "status",
		Short: "Progress/ETA for the embedding migration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return withStore(cmd, func(ctx context.Context, s *store.Store) error {
				st, err := coordinator(cfg, s).GetStatus(ctx)
				if err != nil {
					if cogerr.Is(err, cogerr.NotFound) {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no migration in progress")
						return nil
					}
					return err
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), st.String())
				return nil
			})
		},
	}

	prune := &cobra.Command{
		Use:   "prune <name>/<model>",
		Short: "Drop a retired provider's embedding table (deliberate, explicit)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, model, err := migrate.ParseProviderRef(args[0])
			if err != nil {
				return err
			}
			return withStore(cmd, func(ctx context.Context, s *store.Store) error {
				// Safety: never the active provider, never a party to an
				// in-progress migration.
				if active, err := s.ActiveProvider(ctx); err == nil && active.Name == name && active.Model == model {
					return cogerr.Ef("cli.prune", cogerr.Validation,
						"%s/%s is the active provider; migrate away from it first", name, model)
				}
				if m, err := s.ActiveMigration(ctx); err == nil {
					if (m.FromName == name && m.FromModel == model) || (m.ToName == name && m.ToModel == model) {
						return cogerr.Ef("cli.prune", cogerr.Validation,
							"%s/%s is part of the in-progress migration; finish or roll it back first", name, model)
					}
				}
				if err := s.DropProvider(ctx, name, model); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pruned %s/%s (table dropped, registry row removed)\n", name, model)
				return nil
			})
		},
	}

	cmd.AddCommand(migrateCmd, status, prune)
	return cmd
}
