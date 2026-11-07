package api

import (
	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// resumeFromLimitViaDaemon is the same daemon action the TUI's `c` key and the
// web's Retry button call. Keeping the CLI seam at the request boundary makes
// the daemon the sole owner of respawn, prompt delivery, and liveness changes.
var resumeFromLimitViaDaemon = routedResumeFromLimit

// confirmHandoffDeliveryViaDaemon is the --delivered half (#4429): the
// operator's attestation that the pending mission already landed, retiring the
// obligation without a resend. A separate seam — not a field on the resume
// request — so a pre-verb daemon refuses loudly instead of resending.
var confirmHandoffDeliveryViaDaemon = routedConfirmHandoffDelivery

var sessionsRetryLimitDelivered bool

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
send the pending mission again. When the pane shows the incoming agent ALREADY
acting on its mission, use --delivered instead: it retires the pending
obligation and clears the leftover operation/startup flags WITHOUT sending the
mission a second time.

The command fails when neither recovery obligation exists. Use 'af sessions
list' to find sessions carrying the [limit] badge; the TUI and web expose
Retry handoff and Mark delivered for an unconfirmed handoff.

A startup-unknown agent handoff parked at its limit is not resent: the daemon
queues the mission into the session's stored prompt for the next limit resume.
That committed transfer exits 0 with a "note" field naming how the send
happens — the scheduled retry when limit_auto_resume is on, or an explicit
'c'/'retry-limit' when it is off (the default). The TUI offers the same verb
as "Queue for limit resume" and the web as the Queue for limit resume button.

The output's conversation object says whether the agent serving the session
afterwards can read its previous conversation: carried=true when the retry
resumed it (the committed account swap's carry, the recorded conversation id,
or a worktree-scoped resume), or carried=false with carry_failure naming why
it starts fresh — including a retry that had no recorded conversation id to
resume and came up clean rather than guessing at the account's latest. For a
sandbox session the answer is whatever the sandbox's own recovery reported:
carried=true only when the in-place relaunch it performed resumed this
session's own conversation, and carry_failure otherwise — a re-provision, a
fresh start, or an older sandbox build that sent no report.

Example:
  af sessions retry-limit fix-auth
  af sessions retry-limit fix-auth --delivered`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repoID, err := resolveRepoIDForLookup()
		if err != nil {
			return jsonError(err)
		}

		title := args[0]
		var conversation session.HandoffConversation
		var note string
		if sessionsRetryLimitDelivered {
			var resp daemon.ConfirmHandoffDeliveryResponse
			resp, err = confirmHandoffDeliveryViaDaemon(daemon.ConfirmHandoffDeliveryRequest{Title: title, RepoID: repoID})
			conversation = resp.Conversation
		} else {
			var resp daemon.ResumeFromLimitResponse
			resp, err = resumeFromLimitViaDaemon(daemon.ResumeFromLimitRequest{Title: title, RepoID: repoID})
			conversation = resp.Conversation
			note = resp.Reason
		}
		warning := ""
		if err != nil && apiclient.IsMutationCommitted(err) {
			warning = err.Error()
		} else if err != nil {
			return jsonError(err)
		}

		output := map[string]any{"ok": true, "title": title, "conversation": conversation}
		if sessionsRetryLimitDelivered {
			output["delivered"] = true
		}
		if note != "" {
			// A committed-but-not-a-resume outcome — the daemon queued the
			// pending mission for the scheduled limit retry and answered OK
			// with the queue note. Surface it so the CLI does not print a bare
			// "ok" for a verb that restarted nothing (#6928 review MED).
			output["note"] = note
		}
		if warning != "" {
			output["warning"] = warning
		}
		return jsonOut(output)
	},
}

func init() {
	sessionsRetryLimitCmd.Flags().BoolVar(&sessionsRetryLimitDelivered, "delivered", false,
		"Mark the pending handoff mission as delivered and retire it WITHOUT resending — use after inspecting the pane and confirming the incoming agent already received it")
}
