package api

import (
	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// resumeFromLimitViaDaemon is the same daemon action the TUI's `c` key and the
// web's Retry button call. Keeping the CLI seam at the request boundary makes
// the daemon the sole owner of respawn, prompt delivery, and liveness changes.
var resumeFromLimitViaDaemon = daemon.ResumeFromLimit

var sessionsRetryLimitCmd = &cobra.Command{
	Use:   "retry-limit <title>",
	Short: "Retry a session blocked at a usage limit",
	Long: `Retry a session that is parked at a provider usage-limit wall.

The daemon runs the same recovery action as the TUI's c key and the web's Retry
button: it re-spawns an exited agent when necessary, re-delivers the pending
prompt (or "continue" for an interactive session with no stored prompt), and
clears the limit state after delivery succeeds.

The command fails if the session is not currently blocked on a usage limit.
Use 'af sessions list' to find sessions carrying the [limit] badge.

Example:
  af sessions retry-limit fix-auth`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		title := args[0]
		if err := resumeFromLimitViaDaemon(daemon.ResumeFromLimitRequest{Title: title, RepoID: repoID}); err != nil {
			return jsonError(err)
		}

		return jsonOut(map[string]any{"ok": true, "title": title})
	},
}
