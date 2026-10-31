package commands

import (
	"os"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/planusage"
	"github.com/spf13/cobra"
)

var internalCmd = &cobra.Command{
	Use:    "internal",
	Short:  "Commands af runs for itself",
	Hidden: true,
}

// planTapCmd is the Claude Code status line command af injects into the
// Claude sessions it launches. It exits 0 whatever happens.
var planTapCmd = &cobra.Command{
	Use:   "plan-tap",
	Short: "Record Claude plan usage from status line input, then run your own status line",
	Args:  cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		home, _ := config.GetConfigDir()
		userHome, _ := os.UserHomeDir()
		cfg, err := config.LoadConfig()
		planusage.Tap(os.Stdin, os.Stdout, planusage.TapEnv{
			Home: home, Getenv: os.Getenv, Now: time.Now(), UserHome: userHome,
			Record: err == nil && cfg != nil && cfg.Backlog.PlanUsage,
		})
	},
}

func init() {
	internalCmd.AddCommand(planTapCmd)
	rootCmd.AddCommand(internalCmd)
}
