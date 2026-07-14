package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/config"
	"github.com/enqack/cognosis/internal/store"
)

func newSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "schema", Short: "Derived-index schema management"}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Current schema version and pending migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			dsn, err := store.ResolveDSN(cfg.DSN)
			if err != nil {
				return err
			}
			st, err := store.Status(dsn)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "version: %d\ndirty: %v\nlatest: %d\npending: %d\ncurrent: %v\n",
				st.Version, st.Dirty, st.Latest, st.Pending, st.Current())
			return nil
		},
	})
	return cmd
}
