package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// A failed readiness capture must not turn the daemon poll into a hot loop.
// Handoff recovery is rare and the pending mission is durable, so a modest
// fixed delay is preferable to hammering a pane whose agent is still starting.
const pendingHandoffRetryDelay = 30 * time.Second

type pendingHandoffEntry struct {
	repoID   string
	key      string
	instance *session.Instance
}

// ResumePendingHandoffs retries takeover briefs only when mission-scoped evidence
// proves the prior attempt did not deliver. Missing and ambiguous evidence stays
// pending for inspection. It is deliberately driven by the ordinary daemon poll.
// A crash-restored OpReplacing row stays hidden from
// status settlement and this path performs the readiness check itself; a legacy
// settled row waits for RefreshStatuses to provide LiveReady first. The same
// target-before-op locks as handoff/send-prompt prevent a recovery paste from
// racing a newer mutation.
//
// Every settlement below persists AND publishes (#2782). The events plane matters
// more here than on the interactive path, not less: no client asked for this work,
// so not even the window that started the handoff has a reason to re-Snapshot
// afterwards — and the status poll is precisely what cannot report it, because it
// skips a fenced row and then takes the settled state as its own baseline. Without
// the publish, a row this loop finishes stays "working" on every open rail until
// something unrelated republishes it.
func (m *Manager) ResumePendingHandoffs() {
	m.mu.Lock()
	entries := make([]pendingHandoffEntry, 0, len(m.instances))
	for key, instance := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		entries = append(entries, pendingHandoffEntry{repoID: repoID, key: key, instance: instance})
	}
	m.mu.Unlock()

	for _, entry := range entries {
		mission := entry.instance.PendingHandoffMission()
		if mission == "" || entry.instance.UserKilled() {
			m.clearPendingHandoffRetry(entry.repoID, entry.instance)
			continue
		}
		// A recorded delivered verdict already proved the mission landed; all a
		// crash could have skipped is the bookkeeping settle (#4429). Retire it
		// without a resend — and without an operator attestation.
		if entry.instance.PendingHandoffMissionSettleable() {
			m.settleDeliveredPendingHandoff(entry, mission)
			continue
		}
		if entry.instance.StartupStateUnknown() || !entry.instance.PendingHandoffMissionAutoRetryable() {
			m.clearPendingHandoffRetry(entry.repoID, entry.instance)
			continue
		}
		if _, err := m.retryPendingHandoff(entry, mission, false); err != nil {
			m.warn().Printf("handoff %q: pending mission retry did not complete: %v", entry.instance.Title, err)
		}
	}
}

// settleDeliveredPendingHandoff retires a pending mission whose recorded
// verdict is PromptDelivered (#4429). The delivery itself was confirmed before
// the crash — the only unfinished business is the bookkeeping: settle the
// reconstructed replacement fence, clear the obligation, persist and publish.
// The same target-before-op locks as retryPendingHandoff keep it from racing a
// kill or a concurrent mutation.
func (m *Manager) settleDeliveredPendingHandoff(entry pendingHandoffEntry, mission string) {
	unlock := m.lockTarget(entry.repoID, entry.instance.Title)
	defer unlock()

	opLock := m.opLockFor(entry.key)
	if !opLock.TryLock() {
		return
	}
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[entry.key]
	_, killing := m.killsInFlight[entry.key]
	m.mu.Unlock()
	if killing || current != entry.instance || entry.instance.IsTearingDown() ||
		entry.instance.PendingHandoffMission() != mission ||
		!entry.instance.PendingHandoffMissionSettleable() {
		return
	}

	// The delivered verdict is a durable fact, so the mission and fence retire
	// even on a Lost/Dead row — restore still owns the runtime. But
	// ResolveStartupState also restores `started`, and a dead row must not
	// resurrect a live-runtime flag under it: there the startup-unknown marker
	// stays for restore to handle.
	switch entry.instance.GetLiveness() {
	case session.LiveLost, session.LiveDead, session.LiveArchived:
	default:
		entry.instance.ResolveStartupState()
	}
	if entry.instance.GetInFlightOp() == session.OpReplacing {
		if err := entry.instance.Transition(session.CommitHandoff()); err != nil {
			m.warn().Printf("handoff %q: delivered pending mission could not settle the replacement fence: %v",
				entry.instance.Title, err)
			return
		}
	}
	if !entry.instance.ClearPendingHandoffMission(mission) {
		m.warn().Printf("handoff %q: delivered pending mission changed before settlement", entry.instance.Title)
		return
	}
	if perr := m.persistSettlement(entry.repoID, entry.key, entry.instance); perr != nil {
		// The mutation committed in memory; only the durable write failed. The
		// record still carries the delivered verdict, so this pass retries it on
		// the next poll — the settle is idempotent.
		m.warn().Printf("handoff %q: delivered pending mission settled but the write did not persist: %v",
			entry.instance.Title, perr)
	}
	m.clearPendingHandoffRetry(entry.repoID, entry.instance)
	m.info().Printf("handoff %q: retired delivered pending mission", entry.instance.Title)
}

// retryPendingHandoff is the single submission transaction for agent-only
// takeover missions. Automatic callers require positive mission-scoped
// non-delivery evidence and observe the retry delay. An explicit caller may
// override an ambiguous verdict after inspecting a known pane, but still uses
// the same pre-submission fence and settlement rules.
func (m *Manager) retryPendingHandoff(entry pendingHandoffEntry, mission string, explicit bool) (bool, error) {
	unlock := m.lockTarget(entry.repoID, entry.instance.Title)
	defer unlock()

	opLock := m.opLockFor(entry.key)
	if !opLock.TryLock() {
		return false, nil
	}
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[entry.key]
	_, killing := m.killsInFlight[entry.key]
	m.mu.Unlock()
	op := entry.instance.GetInFlightOp()
	retryable := entry.instance.PendingHandoffMissionAutoRetryable()
	if explicit {
		retryable = entry.instance.CanRetryPendingHandoffMissionDelivery()
	}
	if killing || current != entry.instance || entry.instance.IsTearingDown() ||
		(op != session.OpNone && op != session.OpReplacing) ||
		entry.instance.PendingHandoffMission() != mission || !retryable ||
		entry.instance.UserKilled() ||
		// Automatic recovery still requires a KNOWN startup state — it has no
		// operator inspection behind it. The explicit retry is the supported
		// exit for the unknown wedge (#4429): the send path's own readiness wait
		// re-establishes the pane proof the flag says is missing before the
		// composer is ever touched.
		(!explicit && entry.instance.StartupStateUnknown()) {
		return false, nil
	}

	switch entry.instance.GetLiveness() {
	case session.LiveLimitReached:
		// The incoming provider reached its wall before the crash-recovered
		// mission could land. Transfer the exact brief into the established limit
		// retry mechanism; manual/auto resume now owns it.
		entry.instance.SetPrompt(mission)
		if op == session.OpReplacing {
			resetAt, _ := entry.instance.LimitResetAt()
			if err := m.parkHandoffAtLimit(entry.instance, resetAt); err != nil {
				return false, err
			}
		}
		if !entry.instance.ClearPendingHandoffMission(mission) {
			return false, fmt.Errorf("pending mission changed while transferring it to limit retry")
		}
		perr := m.persistSettlement(entry.repoID, entry.key, entry.instance)
		m.clearPendingHandoffRetry(entry.repoID, entry.instance)
		return false, perr
	case session.LiveReady:
		// Positive readiness is the authorization to paste. LiveRunning is not:
		// startup output and an already-delivered mission both look Running, so
		// guessing from it would either lose the brief or duplicate it.
	default:
		// The replacement fence authorizes the same bypass it always did — the
		// send path's own readiness wait is the real gate. So does the explicit
		// arm: the operator's inspection is the attestation, and #4429 settles
		// the fence on ambiguous verdicts, so the pending mission checked above
		// is what marks the obligation now — including on startup-unknown rows,
		// where this send's readiness wait re-establishes the missing proof.
		if op == session.OpReplacing || explicit {
			break
		}
		return false, nil
	}

	if !explicit && !m.pendingHandoffRetryAllowed(entry.repoID, entry.instance) {
		return false, nil
	}
	delivery := handoffDelivery{
		repoID: entry.repoID, key: entry.key, title: entry.instance.Title,
		mission: mission, instance: entry.instance,
	}
	if err := m.beginHandoffMissionDelivery(delivery); err != nil {
		return false, err
	}
	status, err := task.WaitForReadyAndSendPromptWithStatus(context.Background(), entry.instance, mission)
	if evidenceErr := entry.instance.RecordPendingHandoffMissionDelivery(mission, status); evidenceErr != nil {
		return false, errors.Join(err, evidenceErr)
	}
	// A real pane observation — every verdict except could-not-confirm — proves
	// a runtime answers at this binding. That proof is what a startup-unknown
	// flag was missing, so the observation resolves it (#4429). The flag's own
	// marker then stays consistent with the row the send just exercised.
	if status != session.PromptCouldNotConfirm {
		entry.instance.ResolveStartupState()
	}
	if err = handoffDeliveryResultError(status, err); err != nil {
		var limitErr *task.LimitReachedError
		if errors.As(err, &limitErr) {
			entry.instance.SetPrompt(mission)
			if op == session.OpReplacing {
				if terr := m.parkHandoffAtLimit(entry.instance, limitErr.ResetAt); terr != nil {
					return false, errors.Join(err, terr)
				}
			} else {
				m.setLimitReached(entry.instance, limitErr.ResetAt)
			}
			if !entry.instance.ClearPendingHandoffMission(mission) {
				return false, fmt.Errorf("pending mission changed while parking its usage limit")
			}
			perr := m.persistSettlement(entry.repoID, entry.key, entry.instance)
			m.clearPendingHandoffRetry(entry.repoID, entry.instance)
			if perr != nil {
				return false, errors.Join(err, perr)
			}
			return false, nil
		}
		// Best-effort, unlike the settlement writes above (#2781): this raises a
		// suppression marker over a mission that was never delivered, so losing it
		// costs another recovery attempt, never a duplicate execution.
		if op == session.OpReplacing && errors.Is(err, task.ErrAgentReadiness) {
			entry.instance.MarkStartupStateUnknown()
			m.persistAndPublishInstance(entry.repoID, entry.instance)
			m.clearPendingHandoffRetry(entry.repoID, entry.instance)
		}
		if errors.Is(err, task.ErrPromptDelivery) {
			// An ambiguous verdict settles the replacement fence on the runtime
			// proof readiness already supplied — the swap is complete; only the
			// mission stays pending for the operator's confirm-or-retry (#4429).
			// Positive non-delivery keeps the fence: automatic recovery owns the
			// resend and the row stays inert until that lands or exhausts.
			if op == session.OpReplacing && status != session.PromptNotDelivered {
				if terr := entry.instance.Transition(session.CommitHandoff()); terr != nil {
					return false, &mutationCommittedError{err: fmt.Errorf(
						"the pending handoff mission's delivery stayed unconfirmed, but the replacement fence could not be settled: %w",
						errors.Join(err, terr))}
				}
			}
			if evidenceErr := m.persistSettlement(entry.repoID, entry.key, entry.instance); evidenceErr != nil {
				return false, errors.Join(err, evidenceErr)
			}
		}
		return false, err
	}
	if op == session.OpReplacing {
		if err := entry.instance.Transition(session.CommitHandoff()); err != nil {
			return true, &mutationCommittedError{err: fmt.Errorf(
				"delivered the pending handoff mission, but could not settle the replacement fence: %w", err)}
		}
	}
	if !entry.instance.ClearPendingHandoffMission(mission) {
		return true, &mutationCommittedError{err: fmt.Errorf("delivered the pending handoff mission, but its obligation changed before settlement")}
	}
	perr := m.persistSettlement(entry.repoID, entry.key, entry.instance)
	m.clearPendingHandoffRetry(entry.repoID, entry.instance)
	if perr != nil {
		return true, &mutationCommittedError{err: fmt.Errorf(
			"delivered the pending handoff mission, but could not persist its settlement: %w", perr)}
	}
	m.info().Printf("handoff %q: delivered pending mission", entry.instance.Title)
	return true, nil
}

func (m *Manager) pendingHandoffRetryAllowed(repoID string, instance *session.Instance) bool {
	key := stableSessionKey(repoID, instance)
	now := nowFunc()
	m.mu.Lock()
	defer m.mu.Unlock()
	if due := m.handoffRetryDue[key]; now.Before(due) {
		return false
	}
	m.handoffRetryDue[key] = now.Add(pendingHandoffRetryDelay)
	return true
}

func (m *Manager) clearPendingHandoffRetry(repoID string, instance *session.Instance) {
	m.mu.Lock()
	delete(m.handoffRetryDue, stableSessionKey(repoID, instance))
	m.mu.Unlock()
}
