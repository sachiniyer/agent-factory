package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/log"
)

// limitRetriedMsg reports completion of an async usage-limit manual retry
// (#1146, `c`). On success the daemon cleared the limit and re-delivered the
// prompt; a non-nil err is surfaced in the error box. A non-empty note is a
// committed-but-not-a-resume outcome — the pending handoff mission queued
// into the scheduled limit retry — which surfaces as its own message.
type limitRetriedMsg struct {
	target sessionActionTarget
	err    error
	note   string
}

// handleLimitRetry is the explicit retry verb (#1146, `c`). It resumes a usage-
// limited session or, after the operator inspects its pane, retries an account
// or agent handoff whose first mission submission could not be confirmed. The
// daemon RPC may re-spawn and delivers a prompt, so it runs off the event loop
// like the kill/archive commands rather than freezing the TUI.
func (m *home) handleLimitRetry() (tea.Model, tea.Cmd) {
	selected := m.sidebar.RowVerbTarget()
	if selected == nil {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleNotice(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	canRetry := selected.CanRetryPendingManualAccountSwapDelivery() ||
		selected.CanRetryPendingAccountSwapNoticeDelivery() ||
		selected.CanRetryPendingHandoffMissionDelivery()
	canConfirm := selected.CanConfirmPendingHandoffDelivery() ||
		selected.CanConfirmPendingAccountSwapDelivery()
	if !selected.LimitReached() && !canRetry && !canConfirm {
		return m, m.handleNotice(fmt.Errorf("session '%s' is not blocked on a usage limit", selected.Title))
	}
	target := captureSessionActionTarget(selected, m.repoID)
	// A confirmable pending delivery gives `c` two real verbs — resend the
	// mission, or attest it already landed and retire it (#4429). The picker
	// makes that the operator's explicit choice; without it a confirmable row
	// would have `c` silently mean resend and the no-resend exit would exist
	// on the CLI and web but not here.
	if canConfirm {
		// The picker's first row is whichever resume `c` would run. On a row that
		// is only usage-limited that is the plain limit resume, which un-stalls
		// the agent and does not resend the pending mission, so it must not be
		// labelled as a resend.
		resend := handoffResolveNoResend
		switch {
		case selected.PendingHandoffMission() != "" && selected.LimitReached() && selected.CanRetryPendingHandoffMissionDelivery():
			// Gate on the admission predicate — the same bit the daemon
			// routes on and the web mirror checks — not on
			// StartupStateUnknown: within the crash window a startup-unknown
			// row at the wall with a "delivered" verdict is NOT admitted, and
			// labelling the refused verb "Queue…" would lie again. For every
			// admitted shape retryPendingHandoff's limit arm queues the
			// mission into the stored Prompt for the next resume: nothing is
			// resent now and nothing is restarted (#6800; #6928 review).
			resend = handoffResolveQueueLimit
		case selected.PendingHandoffMission() != "" && selected.LimitReached():
			// A pending handoff at the wall that is NOT admitted — a known
			// liveness, or a startup-unknown row whose verdict is delivered —
			// takes the plain limit resume: the daemon un-stalls the agent
			// and leaves the pending mission unsent.
			resend = handoffResolveResumeLimit
		case canRetry:
			resend = handoffResolveResendMission
		case selected.LimitReached():
			resend = handoffResolveResumeLimit
		}
		return m.openHandoffResolvePicker(selected, target, resend)
	}
	return m, m.resumeFromLimitCmd(target)
}

// resumeFromLimitCmd runs the daemon resume (re-spawn if needed + re-deliver the
// prompt + clear the limit state) off the event loop (#1146), mirroring
// restoreInstanceCmd.
func (m *home) resumeFromLimitCmd(target sessionActionTarget) tea.Cmd {
	resume := resumeFromLimitThroughDaemon
	return func() tea.Msg {
		resp, err := resume(target.resumeFromLimitRequest())
		if err != nil {
			if !apiclient.IsMutationCommitted(err) {
				log.ErrorLog.Printf("could not resume limited session %q: %v", target.title, err)
			}
			return limitRetriedMsg{target: target, err: err, note: resp.Reason}
		}
		return limitRetriedMsg{target: target, note: resp.Reason}
	}
}

// handleLimitRetried finalizes an async usage-limit retry. On success the daemon
// has already cleared the limit + set Running and persisted; clear the local row
// optimistically for instant feedback (the badge disappears without waiting for
// the next snapshot reconcile). A committed settlement warning clears the row
// too and remains visible as a completion message; a clean failure lands in the
// error box.
func (m *home) handleLimitRetried(msg limitRetriedMsg) (tea.Model, tea.Cmd) {
	committedWarning := msg.err != nil && apiclient.IsMutationCommitted(msg.err)
	if msg.err != nil && !committedWarning {
		// The resume re-delivers the pending prompt, so one that may have landed
		// is not reported as a failure to retry (#4824): a second `c` could
		// deliver the prompt twice. The limit badge stays until the snapshot
		// clears it.
		if mutationOutcomeUnknown(msg.err) {
			return m, m.handleError(mutationOutcomeError(
				fmt.Sprintf("resuming session '%s'", msg.target.title), "the session's pane", msg.err))
		}
		return m, m.handleError(fmt.Errorf("failed to resume session '%s': %w", msg.target.title, msg.err))
	}
	// A committed-but-not-a-resume outcome carries its queue note: the daemon
	// moved the pending mission into the scheduled limit retry, which means
	// the wall stays parked — the badge must not clear for it (#6928 review
	// MED).
	if inst := m.resolveSessionActionTarget(msg.target); inst != nil && msg.note == "" {
		inst.ClearLimitReached()
	}
	if committedWarning {
		return m, m.showTransientMessage(fmt.Sprintf("Retry for '%s' completed, with warning: %v", msg.target.title, msg.err))
	}
	if msg.note != "" {
		// The queue note is informational, not a failure: nothing was
		// restarted and nothing failed — the mission simply sends when the
		// agent resumes.
		return m, m.showTransientMessage(fmt.Sprintf("Retry for '%s': %s", msg.target.title, msg.note))
	}
	return m, nil
}
