package commands

import "github.com/spf13/cobra"

// legacyTmuxGuardHookCmd is a no-op compatibility seam for already-running
// Codex sessions that loaded the versioned pre-#2563 guard hook. Their retained
// script still invokes this hidden command after af upgrades. The guard itself
// remains removed; treating the legacy call as "no guard" prevents a stale hook
// from denying every Bash command (#2608).
var legacyTmuxGuardHookCmd = &cobra.Command{
	Use:    "hook-guard-tmux",
	Hidden: true,
	Args:   cobra.NoArgs,
	Run:    func(*cobra.Command, []string) {},
}

func init() {
	rootCmd.AddCommand(legacyTmuxGuardHookCmd)
}
