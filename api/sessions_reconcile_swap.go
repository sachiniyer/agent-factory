package api

import (
	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
)

// reconcileSwapViaDaemon is the same daemon action the HTTP route exposes: the
// fenced session's in-flight swap record names WHICH swap the host is asked
// about, and the fence lifts exactly when the answer is definite (#7793).
var reconcileSwapViaDaemon = routedReconcileOffBoxSwap

var sessionsReconcileSwapCmd = &cobra.Command{
	Use:   "reconcile-swap <title>",
	Short: "Re-ask a fenced session's host for its swap verdict",
	Long: `Re-ask the agent-server of a session fenced by an indeterminate off-box
account swap for the recorded swap's terminal verdict.

A daemon that died between sending a swap request and reconciling its response
leaves the session fenced behind a durable in-flight record: the request may
still commit on the host, so nothing is admitted until the host says how THAT
swap ended. This command re-runs the same reconcile the lost-response path
uses — the agent-server's terminal record for the recorded swap id, plus the
serialized discard and a re-read when the first read cannot attribute the
answer. It never sends the swap request again.

The fence lifts only on a definite verdict:

- committed — the host provably runs the new account: the row records it and
  the outgoing scope's write-back and removal run exactly as the live swap
  path does;
- rolled back or never ran — the host provably kept the old account: the
  staged scope is dropped and the row stays on the old account.

An indeterminate answer (unreachable host, a pre-identity agent-server that
cannot attribute an outcome, or a stage layout that proves nothing) leaves
the fence and the record in place — pane liveness alone never clears them.
'af sessions kill' remains the way to remove the row outright.

A daemon or agent-server too old for the verb refuses loudly rather than
guess at a credential identity.

Example:
  af sessions reconcile-swap fix-auth`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		scope, err := resolveLookupScope()
		if err != nil {
			return jsonError(err)
		}
		title := args[0]
		resp, err := reconcileSwapViaDaemon(daemon.ReconcileOffBoxSwapRequest{Title: title, RepoID: scope.RepoID})
		warning := ""
		if err != nil && apiclient.IsMutationCommitted(err) {
			warning = err.Error()
		} else if err != nil {
			return jsonError(scope.nameMiss(title, err))
		}

		output := map[string]any{"ok": true, "title": title}
		if resp.Account != "" {
			output["account"] = resp.Account
		}
		if warning != "" {
			output["warning"] = warning
		}
		return jsonOut(output)
	},
}
