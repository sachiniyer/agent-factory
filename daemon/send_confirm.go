package daemon

import (
	"context"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// beginSendConfirmation captures the pre-send frame the #6474 re-check needs to
// exclude a turn that was already running. Call it before the send.
func beginSendConfirmation(instance *session.Instance) *task.SendConfirmation {
	return task.BeginInstanceSendConfirmation(context.Background(), instance)
}

// promoteUnverifiedSend is #6474: a delivery that landed but could not be proven
// at the Enter should not read as unconfirmed forever. A sent-unverified send is
// re-checked for a bounded window and promoted to the existing delivered status
// ONLY on positive evidence bound to this send — the agent's turn began after
// it. An emptied composer, a changed pane, or an agent that was already busy is
// not evidence, and any miss leaves sent-unverified untouched. Other verdicts
// and errors pass through. The same status feeds a task run's last_run_status.
func (m *Manager) promoteUnverifiedSend(instance *session.Instance, status session.PromptDeliveryStatus, sendErr error, c *task.SendConfirmation) session.PromptDeliveryStatus {
	if sendErr != nil || status != session.PromptSentUnverified || !c.Confirm(context.Background()) {
		return status
	}
	m.info().Printf("prompt to %q: sent-unverified promoted to delivered; the agent's turn began after the send (#6474)", instance.Title)
	return session.PromptDelivered
}

// sendAttemptClock is the clock SendPromptWithEvidence stamps its attempt with,
// also binding the confirmation's evidence boundary to that same instant.
func sendAttemptClock(c *task.SendConfirmation) func() time.Time {
	return func() time.Time {
		at := nowFunc()
		c.BindAttempt(at)
		return at
	}
}
