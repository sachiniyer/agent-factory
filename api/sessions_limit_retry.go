package api

import (
	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// resumeFromLimitViaDaemon is the same daemon action the TUI's `c` key and the
// web's Retry button call. Keeping the CLI seam at the request boundary makes
// the daemon the sole owner of respawn, prompt delivery, and liveness changes.
var resumeFromLimitViaDaemon = daemon.ResumeFromLimit

var sessionsRetryLimitCmd = &cobra.Command{
	Use:   "retry-limit <title>",
	Short: "Retry a usage-limit resume or inspected handoff",
	Long: `Retry a session parked at a provider usage-limit wall, or explicitly retry
a handoff whose mission delivery could not be confirmed.

The daemon runs the same recovery action as the TUI's c key and the web's Retry
button: it re-spawns an exited agent when necessary, re-delivers the pending
prompt (or "continue" for an interactive session with no stored prompt), and
clears the limit state after delivery succeeds.

Before retrying an unconfirmed handoff, inspect its pane: the first submission
may already have landed, and this command is the operator's explicit decision to
send the pending mission again. The command fails when neither recovery
obligation exists. Use 'af sessions list' to find sessions carrying the [limit]
badge; the TUI and web expose Retry handoff for an unconfirmed handoff.

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
		err = resumeFromLimitViaDaemon(daemon.ResumeFromLimitRequest{Title: title, RepoID: repoID})
		warning := ""
		if err != nil && apiclient.IsMutationCommitted(err) {
			warning = err.Error()
		} else if err != nil {
			return jsonError(err)
		}

		output := map[string]any{"ok": true, "title": title}
		if warning != "" {
			output["warning"] = warning
		}
		return jsonOut(output)
	},
}
