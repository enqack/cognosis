package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Read or persist configuration values"}

	get := &cobra.Command{
		Use:   "get <key>",
		Short: "Print the effective value for a key (env > file > default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			v, err := cfg.Get(args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), v)
			return nil
		},
	}

	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Persist a value to config.yaml",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return cfg.Set(args[0], args[1])
		},
	}

	cmd.AddCommand(get, set)
	return cmd
}
