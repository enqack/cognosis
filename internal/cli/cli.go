// Package cli owns the Cobra command tree. Commands are grouped by
// resource noun; daemon lifecycle stays top-level as the primary entrypoint.
package cli

import (
	"github.com/spf13/cobra"
)

// buildVersion is the link-time version (from main.version via Execute). It
// backs `cognosis --version` / `cognosis version` and the version the daemon
// reports to MCP clients. Defaults to "dev" for a plain `go build`.
var buildVersion = "dev"

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "cognosis",
		Short:         "Centralized, project-agnostic long-term memory service for MCP agents",
		Version:       buildVersion,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newStartCmd(),
		newStopCmd(),
		newStatusCmd(),
		newEmbeddingsCmd(),
		newSchemaCmd(),
		newNoteCmd(),
		newVaultCmd(),
		newTokenCmd(),
		newConfigCmd(),
		newContextCmd(),
		newHookCmd(),
		newVersionCmd(),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cognosis version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println(buildVersion)
			return nil
		},
	}
}

// Execute runs the CLI; main.go is the only caller. version is stamped at link
// time and threaded through to `--version` and the MCP server's advertised
// implementation version.
func Execute(version string) error {
	if version != "" {
		buildVersion = version
	}
	return newRoot().Execute()
}
