package cli

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/auth"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/daemon"
	"github.com/enqack/cognosis/internal/embed"
	"github.com/enqack/cognosis/internal/lifecycle"
	"github.com/enqack/cognosis/internal/mcpserver"
	"github.com/enqack/cognosis/internal/migrate"
	"github.com/enqack/cognosis/internal/persona"
	"github.com/enqack/cognosis/internal/query"
	"github.com/enqack/cognosis/internal/store"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/watch"
	"github.com/enqack/cognosis/internal/write"
)

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the Cognosis daemon (fails fatally if dependencies are unreachable)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			foreground, _ := cmd.Flags().GetBool("foreground")
			if !foreground {
				pid, err := daemon.Daemonize(cmd.Context(), cfg.Paths())
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cognosis daemon starting (pid %d); check `cognosis status`\n", pid)
				return nil
			}

			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			ctx, cancel := daemon.SignalContext(cmd.Context())
			defer cancel()

			prov := embed.NewOllama(cfg.Embedding.URL, cfg.Embedding.Model)
			table := embed.TableSlug(prov.Name(), prov.Model())
			hist := vault.NewHistory(cfg.KBPath)

			// The factory builds provider clients from registry identities —
			// the query fan-out and the migration worker both construct
			// providers through it, so a migration to a new model needs no
			// restart.
			factory := func(name, model string) (embed.Provider, error) {
				if name != "ollama" {
					return nil, fmt.Errorf("unknown embedding provider %q (only ollama is wired)", name)
				}
				return embed.NewOllama(cfg.Embedding.URL, model), nil
			}

			w := watch.New(cfg, log)
			w.MakeIndexer = func(s *store.Store) *write.Indexer {
				coord := &migrate.Coordinator{Store: s, Factory: factory, Log: log}
				return &write.Indexer{Store: s, Provider: prov, Table: table, TargetsFn: coord.EmbedTargets}
			}

			return daemon.Run(ctx, cfg, log, daemon.Options{
				Reconciler: w,
				Runners:    []daemon.Runner{w},
				Embedder:   prov,
				MakeRunners: func(s *store.Store) ([]daemon.Runner, error) {
					// Zero-config local posture: mint the auto token on first
					// start so local clients can authenticate immediately.
					if err := auth.EnsureLocalToken(cmd.Context(), s, cfg.Paths().TokenFile()); err != nil {
						return nil, err
					}
					coord := &migrate.Coordinator{Store: s, Factory: factory, Log: log}
					ix := &write.Indexer{Store: s, Provider: prov, Table: table, TargetsFn: coord.EmbedTargets}
					pipeline := write.NewPipeline(ix, cfg.KBPath, hist, w)
					engine := &query.Engine{
						Store:   s,
						Factory: factory,          // registry-driven legs: migration fallback read
						Lazy:    coord.LazyEnsure, // touch-migration on query hits
					}
					if err := persona.Seed(cfg.Paths().PersonasDir()); err != nil {
						return nil, err
					}
					personas := &persona.Registry{
						Dir:     cfg.Paths().PersonasDir(),
						Enabled: cfg.EnabledPersonaIDs(),
						Log:     log,
					}
					lc := &lifecycle.Engine{
						Store: s, Indexer: ix, VaultDir: cfg.KBPath,
						Hist: hist, Supp: w, Log: log, Query: engine,
						// The *same* locks the pipeline uses, not a second set.
						// Both write the same vault files, and a compile run can
						// overlap an agent's edit_note on the same note.
						Locks: pipeline.Locks,
					}
					srv, err := mcpserver.NewTLS(cfg.BindAddress, cfg.KBPath, log, pipeline, engine, s, lc, personas, cfg.TLS)
					if err != nil {
						return nil, err // non-loopback bind: refuse to start
					}
					srv.Migrations = coord
					srv.Version = buildVersion
					return []daemon.Runner{srv, &migrate.Worker{C: coord}}, nil
				},
			})
		},
	}
	cmd.Flags().Bool("foreground", false, "run in the foreground instead of daemonizing")
	return cmd
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if err := daemon.Stop(cfg.Paths(), 10*time.Second); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "stopped")
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report daemon health: process, Postgres, embedding provider, schema currency",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			checks := daemon.Status(cmd.Context(), cfg)
			unhealthy := false
			for _, c := range checks {
				mark := "ok"
				if !c.OK {
					mark = "FAIL"
					unhealthy = true
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-10s %-4s %s\n", c.Name, mark, c.Detail)
			}
			if unhealthy {
				return fmt.Errorf("one or more checks failed")
			}
			return nil
		},
	}
}
