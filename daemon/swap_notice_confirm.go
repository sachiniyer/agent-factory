package daemon

import (
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// deliverAutomaticAccountNotice is the automatic swap's half of
// deliverManualAccountMission (#6767): the notice send must return a durable
// verdict on the committed transaction, and only PromptDelivered may retire
// the obligation. The pre-send begin boundary is what makes a crash safe — a
// daemon that dies mid-send reloads could-not-confirm, never the empty verdict
// that would let the scheduler resend a notice that may already have landed.
// Every error return leaves the pending swap and the parked limit intact for
// the verdict's owner: positive non-delivery belongs to the scheduler,
// ambiguity to the re-verification watch below or the operator.
func (m *Manager) deliverAutomaticAccountNotice(repoID, key string, instance *session.Instance, swap *autoAccountSwap, prompt string) error {
	previousStatus, previousAttempt, err := instance.BeginPendingAccountSwapNoticeDelivery(swap.from, swap.to)
	if err != nil {
		return err
	}
	if err := m.persistSettlement(repoID, key, instance); err != nil {
		// The send never began, so the verdict that admitted this attempt is
		// still true — restore it rather than leaving the could-not-confirm
		// fence a persisted send never justified.
		_ = instance.RestorePendingAccountSwapNoticeDelivery(swap.from, swap.to, previousStatus, previousAttempt)
		return err
	}
	var attemptedAt time.Time
	confirmation := beginSendConfirmation(instance)
	status, err := instance.SendPromptWithEvidence(prompt, sendAttemptClock(confirmation, &attemptedAt))
	status = m.promoteUnverifiedSend(instance, status, err, confirmation)
	// Bind the verdict to the transaction before it is read back: the record
	// write precedes the error return below, so even a failed send leaves the
	// exact verdict — and the attempt instant re-verification binds to —
	// durable beside the obligation it describes.
	recErr := instance.RecordPendingAccountSwapNoticeDelivery(swap.from, swap.to, status)
	return errors.Join(handoffDeliveryResultError(status, err), recErr)
}

// pendingSwapNoticeWatch is one committed account swap's notice awaiting
// re-verification of its ambiguous durable verdict — the swap-side twin of
// unverifiedSendWatch (#6767). The notice IS the obligation, so instead of
// promoting a task verdict the fired watch retires the pending swap itself:
// the same "the agent's turn began after the send" contract the bounded
// Confirm applies at send time, folded over the poll's frames for the turn
// that starts later (a queued draft answered under a long-running turn, a
// hand-pressed Enter). Guards under pausedMu alongside unverifiedSendWatches.
type pendingSwapNoticeWatch struct {
	attemptedAt time.Time
	expiresAt   time.Time
	agent       string
	watch       *task.TurnWatch
	fired       bool
}

// notePendingSwapNoticeEvidence feeds the pending-swap re-verification watch
// from the status poll (#6767). It runs inside the poll's pending-swap skip —
// the row stays owned by ResumeLimitedSessions for liveness and recovery, but
// its pane still answers the one question the durable verdict left open: did
// the agent take this notice. Arming is lazy and driven by the durable record
// alone — verdict ambiguous, attempt boundary non-zero, evidence window open
// — so a restarted daemon re-arms mid-window watches without a boot pass, and
// a verdict written by ANY arm (scheduled send, manual mission, operator
// retry) feeds here identically. Nothing weaker arms it: a cleared,
// delivered, or positively-missed verdict, a missing attempt boundary, and a
// row whose resume fence is held all pass through untouched.
func (m *Manager) notePendingSwapNoticeEvidence(repoID, key string, instance *session.Instance) {
	from, to, status, attemptedAt, ok := instance.PendingAccountSwapNoticeDelivery()
	skey := stableSessionKey(repoID, instance)
	if !ok || !ambiguousSwapNoticeVerdict(status) || attemptedAt.IsZero() || instance.StartupStateUnknown() {
		// Nothing to re-verify: a settled swap, a verdict automation owns, a
		// pre-field verdict with no boundary to bind, or a row whose runtime
		// binding is itself unproven — a turn on an unbound pane could be the
		// OUTGOING agent's and must never retire this notice.
		m.pausedMu.Lock()
		delete(m.pendingSwapNoticeWatches, skey)
		m.pausedMu.Unlock()
		return
	}
	now := nowFunc()
	expiresAt := attemptedAt.Add(sendReverifyWindow)
	if !now.Before(expiresAt) {
		// A turn this far after the send can no longer be attributed to it;
		// the verdict stands and the operator exits own the row.
		m.pausedMu.Lock()
		delete(m.pendingSwapNoticeWatches, skey)
		m.pausedMu.Unlock()
		return
	}
	if op, _ := instance.InFlightOpAndEpoch(); op != session.OpNone {
		return // the resume owns this tick's pane; feed again once it settles
	}
	agent := instance.ResolvedAgent()
	m.pausedMu.Lock()
	entry := m.pendingSwapNoticeWatches[skey]
	if entry == nil || !entry.attemptedAt.Equal(attemptedAt) {
		entry = &pendingSwapNoticeWatch{
			attemptedAt: attemptedAt,
			expiresAt:   expiresAt,
			agent:       agent,
			watch:       task.NewTurnWatch(agent, attemptedAt),
		}
		m.pendingSwapNoticeWatches[skey] = entry
	} else if entry.agent != agent {
		entry.agent = agent
		entry.watch = task.NewTurnWatch(agent, entry.attemptedAt)
	}
	m.pausedMu.Unlock()
	// Capture the pane the way the live-row poll does: SnapshotAgent owns the
	// observation fence, so a capture that raced a delivery cannot feed this
	// watch a frame the send's own writer already judged. The pending-swap
	// skip above keeps this the ONLY thing the capture feeds — liveness,
	// recovery, and every other consumer stay bypassed for this row.
	releaseObservationSettlement := instance.HoldAgentObservationSettlement()
	capturedAt := nowFunc()
	obs, _, _, observedOp, _, err := instance.SnapshotAgent()
	releaseObservationSettlement()
	if err != nil || observedOp != session.OpNone || !obs.Captured || capturedAt.Before(attemptedAt) {
		return
	}
	m.pausedMu.Lock()
	entry = m.pendingSwapNoticeWatches[skey]
	if entry == nil || !entry.attemptedAt.Equal(attemptedAt) {
		m.pausedMu.Unlock()
		return
	}
	if !entry.fired {
		if !entry.watch.Observe(obs.Content, capturedAt) {
			m.pausedMu.Unlock()
			return
		}
		entry.fired = true
	}
	m.pausedMu.Unlock()
	m.settlePendingSwapNotice(repoID, key, instance, from, to, attemptedAt)
}

// ambiguousSwapNoticeVerdict is the daemon's read of the swap-notice verdicts
// a turn may still resolve: the same list session.ambiguousHandoffDelivery
// keeps, duplicated here because that helper is not exported.
func ambiguousSwapNoticeVerdict(status session.PromptDeliveryStatus) bool {
	return status == session.PromptSentUnverified || status == session.PromptCouldNotConfirm
}

// settlePendingSwapNotice applies a fired re-verification watch: the swap's
// notice verdict becomes delivered on the pane evidence, the transaction
// retires, and the settlement is persisted and published — the same durability
// and announcement the send path's own success settles with. attemptedAt is
// the compare-and-set carried into the session: a newer send's verdict refuses
// evidence bound to the attempt before it (#6767).
func (m *Manager) settlePendingSwapNotice(repoID, key string, instance *session.Instance, from, to string, attemptedAt time.Time) {
	if err := instance.SettlePendingAccountSwapNoticeDelivery(from, to, attemptedAt); err != nil {
		m.pausedMu.Lock()
		delete(m.pendingSwapNoticeWatches, key)
		m.pausedMu.Unlock()
		return // the obligation moved on (retry, confirm, kill) — its owner keeps it
	}
	m.info().Printf("account swap notice for %q promoted to delivered; the agent's turn began after the send (#6767)", instance.Title)
	m.persistAndPublishInstance(repoID, instance)
	m.pausedMu.Lock()
	delete(m.pendingSwapNoticeWatches, key)
	m.pausedMu.Unlock()
}
