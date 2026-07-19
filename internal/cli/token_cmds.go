package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/auth"
	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/store"
)

func withStore(cmd *cobra.Command, fn func(ctx context.Context, s *store.Store) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dsn, err := store.ResolveDSN(cfg.DSN)
	if err != nil {
		return err
	}
	s, err := store.Connect(cmd.Context(), dsn)
	if err != nil {
		return err
	}
	defer s.Close()

	// Migrate only when no daemon owns this database.
	//
	// This ran unconditionally, so any store-using CLI command — including
	// read-only ones like `token list` — could apply a schema migration to a
	// database a live daemon was serving from. A daemon migrates at startup, so
	// when one is present the migration is redundant as well as unwelcome:
	// skipping it is both safer and no less correct.
	if daemonOwns(cmd.Context(), s) != daemonPresent {
		if err := store.Migrate(dsn); err != nil {
			return err
		}
	}
	return fn(cmd.Context(), s)
}

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Bearer-token management"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "create <name>",
			Short: "Create a token; printed once, only its Argon2id hash is stored",
			// Validated in Args, not RunE: RunE calls withStore immediately, so
			// validation placed there could not be tested without Postgres.
			Args: func(_ *cobra.Command, args []string) error {
				if err := cobra.ExactArgs(1)(nil, args); err != nil {
					return err
				}
				return auth.ValidateTokenName(args[0])
			},
			RunE: func(cmd *cobra.Command, args []string) error {
				return withStore(cmd, func(ctx context.Context, s *store.Store) error {
					plaintext, id, hash, err := auth.Generate()
					if err != nil {
						return err
					}
					if err := s.CreateToken(ctx, id, args[0], hash); err != nil {
						return err
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"%s\n\nThis token is shown once and stored only as a hash — save it now.\n", plaintext)
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "revoke <name>",
			Short: "Revoke a token, effective on the next request",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withStore(cmd, func(ctx context.Context, s *store.Store) error {
					if err := s.RevokeToken(ctx, args[0]); err != nil {
						return err
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "revoked: %s\n", args[0])
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List tokens (names and metadata, never secrets)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return withStore(cmd, func(ctx context.Context, s *store.Store) error {
					tokens, err := s.ListTokens(ctx)
					if err != nil {
						return err
					}
					for _, t := range tokens {
						state := "live"
						if t.RevokedAt != nil {
							state = "revoked " + t.RevokedAt.Format("2006-01-02 15:04:05")
						}
						last := "never used"
						if t.LastUsedAt != nil {
							last = "last used " + t.LastUsedAt.Format("2006-01-02 15:04:05")
						}
						// A short id disambiguates rows sharing a name: names are
						// unique only among live tokens, so a rotated client
						// leaves several revoked rows called the same thing.
						//
						// The *tail*, and neither of the two tempting alternatives.
						// A UUIDv7 is 48 bits of millisecond timestamp, 12 bits
						// of rand_a, then 62 bits of rand_b:
						//   - the first 8 hex characters are the timestamp's high
						//     half, so they change only every ~65 seconds and read
						//     identically for everything minted in one sitting —
						//     exactly the rows this column exists to separate;
						//   - rand_a looks like the first random field and is not.
						//     google/uuid fills it with a sub-millisecond sequence
						//     counter ("rand_a (12 bit seq)"), so it is more
						//     timestamp, and 12 bits collides visibly at ~64 rows.
						// rand_b is the only genuine entropy. Ordering comes from
						// the created_at sort, never from the digits shown.
						id := t.ID.String()
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-8s %s created %s, %s\n",
							t.Name, state, id[len(id)-8:],
							t.CreatedAt.Format("2006-01-02 15:04:05"), last)
					}
					return nil
				})
			},
		},
		newTokenPruneCmd(),
	)
	return cmd
}

// newTokenPruneCmd deletes revoked tokens nothing references. No confirmation
// prompt: withStore commands are non-interactive and script-driven, matching
// `embeddings prune`. --dry-run stands in, and shares its predicate with the
// delete so the preview cannot drift from the action.
func newTokenPruneCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete revoked tokens no audit row references",
		Long: "Deletes revoked tokens that nothing in audit_log points at. Referenced tokens are " +
			"kept by design — the audit trail joins to them — so a revoked token remaining after " +
			"a prune means it was used, not that the prune failed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(cmd, func(ctx context.Context, s *store.Store) error {
				out := cmd.OutOrStdout()
				revoked, err := s.CountRevokedTokens(ctx)
				if err != nil {
					return err
				}
				var names []string
				if dryRun {
					names, err = s.PrunableTokens(ctx)
				} else {
					names, err = s.PruneRevokedTokens(ctx)
				}
				if err != nil {
					return err
				}
				if len(names) == 0 {
					_, _ = fmt.Fprintln(out, "nothing to prune")
					return nil
				}
				verb := "pruned"
				if dryRun {
					verb = "would prune"
				}
				_, _ = fmt.Fprintf(out, "%s %d revoked tokens:\n", verb, len(names))
				for _, n := range names {
					_, _ = fmt.Fprintf(out, "  %s\n", n)
				}
				if kept := revoked - len(names); kept > 0 {
					_, _ = fmt.Fprintf(out, "%d revoked tokens kept (referenced by audit_log)\n", kept)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be deleted, delete nothing")
	return cmd
}
