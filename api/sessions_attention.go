package api

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// setAttentionViaDaemon is the daemon seam, overridable in tests.
var setAttentionViaDaemon = routedSetAttention

func newSessionsAttentionCmd() *cobra.Command {
	var self, clear bool
	var reason string
	cmd := &cobra.Command{
		Use:   "attention [<title>]",
		Short: "Ask the user for attention, or clear the request",
		Long: `Mark a session as needing the user. The web rail shows a blinking yellow
dot on the row, and the reason in the row tooltip.

Agents run this with --self only when they are blocked on a decision or
question that only the user can answer, as the last step before they stop.
They do not run it when a task has simply finished.

The request clears when the user submits a line in the agent pane, dismisses
it from the session header in the web UI, or when the agent starts work again
after it went idle.
--clear removes it now.`,
		Example: `  af sessions attention --self --reason "Pick option A or B in the summary above"
  af sessions attention --self --clear
  af sessions attention my-session --clear`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Initialize(false)
			defer log.Close()

			if clear && reason != "" {
				return jsonError(fmt.Errorf("cannot combine --clear with --reason"))
			}
			var id, title, repoID string
			if self {
				if len(args) == 1 {
					return jsonError(fmt.Errorf("cannot combine --self with a <title> argument; --self targets the current session"))
				}
				data, err := resolveSelfSession()
				if err != nil {
					return jsonError(fmt.Errorf("--self must be run from inside an af session: %w", err))
				}
				id, title, repoID = data.ID, data.Title, sessionRepoID(data)
			} else {
				if len(args) == 0 {
					return jsonError(fmt.Errorf("a session <title> is required (or pass --self to target the current session)"))
				}
				var err error
				// The request goes to the TARGETED daemon (routedSetAttention), so the
				// scope is the one every other routed session verb uses: against a
				// remote daemon the client's cwd names a repo that exists here, not there.
				repoID, err = resolveRepoIDForLookup()
				if err != nil {
					return jsonError(err)
				}
				title = args[0]
			}

			resp, err := setAttentionViaDaemon(daemon.SetAttentionRequest{
				ID: id, Title: title, RepoID: repoID, Reason: reason, Clear: clear,
			})
			if err != nil {
				return jsonError(err)
			}
			return jsonOut(map[string]any{"ok": true, "id": resp.ID, "title": resp.Title, "attention": !clear})
		},
	}
	cmd.Flags().BoolVar(&self, "self", false, "Target the current session (resolved via whoami)")
	cmd.Flags().StringVar(&reason, "reason", "", "Short note that tells the user what you need")
	cmd.Flags().BoolVar(&clear, "clear", false, "Remove the attention request")
	return cmd
}
