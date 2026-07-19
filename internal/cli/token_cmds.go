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
			Args:  cobra.ExactArgs(1),
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
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-8s created %s, %s\n",
							t.Name, state, t.CreatedAt.Format("2006-01-02 15:04:05"), last)
					}
					return nil
				})
			},
		},
	)
	return cmd
}
