package task

import (
	"github.com/spf13/cobra"
)

// TaskCmd is a hidden parent command that only exposes the "run" subcommand
// used internally by the scheduler (systemd/launchd).
var TaskCmd = &cobra.Command{
	Use:    "task",
	Short:  "Internal task runner (used by scheduler)",
	Hidden: true,
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run an automated task (called by scheduler)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunTask(args[0])
	},
}

func init() {
	TaskCmd.AddCommand(runCmd)
}
