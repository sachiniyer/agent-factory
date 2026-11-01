package task

import (
	"context"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The confirmation window for an unverified send (#6474). A send whose pane
// evidence was ambiguous — the conservative path withheld its remedy Enter —
// returns sent-unverified, and nothing ever revisited it: deliveries that
// demonstrably landed read as unconfirmed forever. SendConfirmation is the
// revisit. It promotes ONLY on positive evidence bound to this send, and a miss
// claims nothing, so the verdict stays sent-unverified.

var (
	// sendConfirmBudget bounds the re-check and sendConfirmPoll is its interval.
	// Claude and devin need two captures a tick apart to show a turn running, so
	// the budget covers several polls. Package vars so tests can compress them.
	sendConfirmBudget = 6 * time.Second
	sendConfirmPoll   = 500 * time.Millisecond
)

// SendConfirmation is the evidence watch for one send. Begin it BEFORE the
// send; call Confirm after the send reports sent-unverified.
//
// The evidence is the agent's own in-turn chrome appearing after the send began
// (TurnWatch), and it is bound to this send two ways. The pre-send frame seeds
// the watch, so a turn already running when the send started — an agent that
// was busy before our Enter — is excluded: the pane's own clock for it exceeds
// the send's age, and for the agents whose chrome carries no clock the watch
// must see the chrome ABSENT before a present frame counts. And a pane that was
// already showing in-turn chrome, or could not be read, makes the send
// ineligible outright rather than guessed at.
//
// What is deliberately NOT evidence: an empty composer (it is cleared by Esc,
// a user, or a dismissed picker as readily as by a submit), a pane that merely
// changed, and a pane that could not be read.
type SendConfirmation struct {
	target   ReadinessTarget
	sentAt   time.Time
	watch    *TurnWatch
	eligible bool
}

// BeginSendConfirmation captures the pre-send frame. sentAt is the send's
// attempt instant — the same boundary the daemon records for the send. An agent
// with no in-turn signature, an unreadable pane, or a pane already mid-turn
// yields a confirmation that never promotes.
func BeginSendConfirmation(ctx context.Context, target ReadinessTarget, sentAt time.Time) *SendConfirmation {
	c := &SendConfirmation{target: target, sentAt: sentAt}
	agent := target.ResolvedAgent()
	if !turnSignaturedAgent(agent) {
		return c
	}
	before, err := target.PreviewContent(ctx)
	if err != nil || submittedTurnContent(before, agent) {
		return c
	}
	c.watch = NewTurnWatch(agent, sentAt)
	c.watch.Observe(before, sentAt)
	c.eligible = true
	return c
}

// Confirm polls for the bound evidence for up to the confirmation budget and
// reports whether this send's turn began. It returns false on any miss.
func (c *SendConfirmation) Confirm(ctx context.Context) bool {
	if c == nil || !c.eligible {
		return false
	}
	deadline := time.Now().Add(sendConfirmBudget)
	for {
		if ctx.Err() != nil {
			return false
		}
		at := time.Now()
		if content, err := c.target.PreviewContent(ctx); err == nil && c.watch.Observe(content, at) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(sendConfirmPoll):
		}
	}
}

// BeginInstanceSendConfirmation is BeginSendConfirmation for a live session.
func BeginInstanceSendConfirmation(ctx context.Context, inst *session.Instance, sentAt time.Time) *SendConfirmation {
	return BeginSendConfirmation(ctx, instanceReadinessTarget{inst: inst}, sentAt)
}
