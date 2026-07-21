package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/enqack/cognosis/internal/config"
)

// newTelemetryCmd groups read-only views over the daemon's own log. Only
// retrieval lives here today; the group exists because more subsystems log
// countable events and "where do I see what the daemon has been doing"
// deserves one answer. Deliberately not a tuning surface: it reads, it never
// sets -- retrieval tuning stays harness-only (query.Tuning).
func newTelemetryCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "telemetry", Short: "Read-only series from the daemon log"}
	cmd.AddCommand(newTelemetryQueryCmd())
	return cmd
}

func newTelemetryQueryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query [logfile ...]",
		Short: "Per-leg retrieval counts as a rolling CSV series",
		Long: `Parses query_knowledge events from the daemon log into one CSV row per
event (stdout) plus a run summary (stderr). With no arguments it reads the
live daemon.log; "-" reads stdin.

Derived columns:
  and_count         the keyword leg's AND candidate count -- what decides
                    whether the OR fallback fires (empty when the event
                    predates the attrs that make it derivable)
  graph_min_unique  max(0, fused-vector-fts), a lower bound on candidates
                    only the graph leg surfaced
  roll_*            trailing-window rates over the events that carry the
                    field, so old-format lines do not dilute them`,
		RunE: func(cmd *cobra.Command, args []string) error {
			window, _ := cmd.Flags().GetInt("window")
			// A window below 1 would silently blank every rolling column
			// (each add trims the samples straight back to empty), which
			// reads as "no data" rather than "bad flag". Refuse it loudly.
			if window < 1 {
				return fmt.Errorf("--window must be >= 1, got %d", window)
			}

			if len(args) == 0 {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				args = []string{cfg.Paths().LogFile()}
			}
			for _, path := range args {
				if path == "-" {
					if err := runTelemetryQuery(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), window); err != nil {
						return err
					}
					continue
				}
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				err = runTelemetryQuery(f, cmd.OutOrStdout(), cmd.ErrOrStderr(), window)
				_ = f.Close()
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().Int("window", 10, "rolling window, in events")
	return cmd
}
