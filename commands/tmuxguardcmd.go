package commands

import (
	"github.com/sachiniyer/agent-factory/internal/tmuxguard"

	"github.com/spf13/cobra"
)

// tmuxGuardHookCmd is the native half of the af-owned Claude PreToolUse hook.
// It is hidden because it is a JSON-over-stdio integration point, not a human
// CLI surface. The generated plugin invokes the exact af binary that wrote it.
var tmuxGuardHookCmd = &cobra.Command{
	Use:    "hook-guard-tmux",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return tmuxguard.Run(cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(tmuxGuardHookCmd)
}
