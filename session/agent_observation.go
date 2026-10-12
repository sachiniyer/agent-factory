package session

import (
	"sync"
	"time"
)

type agentObservationRuntime struct {
	mu         sync.Mutex
	generation AgentObservationGeneration
}

// AgentObservationGeneration identifies the concrete runtime that answered a
// SnapshotAgent call. Its value is deliberately opaque outside session;
// daemon-owned liveness bookkeeping may compare it through Instance, but cannot
// manufacture or advance it.
type AgentObservationGeneration struct {
	value uint64
}

type agentObservationTarget struct {
	server  AgentServer
	runtime *agentObservationRuntime
}

// HoldAgentObservationSettlement makes a daemon observation's transport read
// and every state conclusion derived from it one boundary. Automated prompt
// delivery takes the same per-instance fence around its final liveness check and
// submission, so it runs wholly before the snapshot or wholly after its apply.
// Manual delivery intentionally does not use this task-safety boundary.
func (i *Instance) HoldAgentObservationSettlement() func() {
	i.agentObservationSettlementMu.Lock()
	var once sync.Once
	return func() { once.Do(i.agentObservationSettlementMu.Unlock) }
}

// agentObservationTarget binds the server and its observation lock in the same
// i.mu section. Runtime replacement clears i.agentObservation alongside the
// derived server cache, so predecessor I/O and replacement I/O never share a
// lock merely because the Instance pointer survived the swap.
func (i *Instance) agentObservationTarget() agentObservationTarget {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agentObservation == nil {
		i.agentObservation = &agentObservationRuntime{
			generation: AgentObservationGeneration{value: i.agentObservationGeneration.Add(1)},
		}
	}
	return agentObservationTarget{
		server:  i.agentServerLocked(),
		runtime: i.agentObservation,
	}
}

// SnapshotAgent serializes pane observation with prompt delivery for one
// concrete runtime. It samples the operation axis and state epoch only AFTER it
// owns that runtime's observation lock, so a delivery that won the lock cannot
// advance the epoch and then have its post-delivery snapshot rejected with the
// older one. A target retired while this call waited is retried against its
// successor. A replacement during Snapshot is revalidated after transport I/O
// and retried too: callers perform transport-liveness side effects before some
// epoch-scoped applies, so returning a retired observation is not safe merely
// because its later state mutation would be rejected.
func (i *Instance) SnapshotAgent() (
	Observation,
	AgentServer,
	AgentObservationGeneration,
	InFlightOp,
	uint64,
	error,
) {
	for {
		target := i.agentObservationTarget()
		target.runtime.mu.Lock()
		i.mu.RLock()
		current := i.agentObservation == target.runtime
		op, epoch := i.inFlightOp, i.stateEpoch
		i.mu.RUnlock()
		if !current {
			target.runtime.mu.Unlock()
			continue
		}
		if op != OpNone {
			target.runtime.mu.Unlock()
			return Observation{}, target.server, target.runtime.generation, op, epoch, nil
		}
		obs, err := target.server.Snapshot()
		i.mu.RLock()
		current = i.agentObservation == target.runtime
		i.mu.RUnlock()
		target.runtime.mu.Unlock()
		if !current {
			continue
		}
		return obs, target.server, target.runtime.generation, op, epoch, err
	}
}

// AgentObservationCurrent reports whether generation still names the runtime
// whose transport produced an observation. Callers that use the result to
// mutate separately locked state must perform this comparison inside that
// state's critical section: replacement invalidates the generation before its
// own later bookkeeping reaches the same lock.
func (i *Instance) AgentObservationCurrent(generation AgentObservationGeneration) bool {
	return generation.value != 0 && i.agentObservationGeneration.Load() == generation.value
}

// NoReadinessProof is the readyEpoch a caller passes when it carries no
// pre-send idle proof at all — RPC sends, post-respawn deliveries. It is
// deliberately unmatchable: stateEpoch starts at 0 and only counts up, so
// the proof check in the record can never accept it (#5221 review).
const NoReadinessProof = ^uint64(0)

// SendPromptWithEvidence starts the attempt timestamp only after any pane
// snapshot already in flight has completed, then records the delivery verdict
// before another snapshot can begin. readyEpoch carries the caller's own
// pre-send readiness proof as the state epoch that proof was earned under —
// a wait that just confirmed the composer idle is observed idleness a
// task-run rebind may keep as its stillness edge, but only while the epoch
// still matches at the record: a send landing inside the wait→submit gap
// invalidates the proof it precedes. Pass NoReadinessProof to record the
// attempt with no idle proof at all (#5221 review). Returns the epoch
// the attempt stamped: the fence any post-submit evidence (the in-turn
// chrome watch) must be raised on, captured inside the record's own lock so
// a send interleaving afterwards cannot hand the caller a newer boundary's
// generation (#5221 review). sendEpoch is 0 when the attempt never recorded.
func (i *Instance) SendPromptWithEvidence(prompt string, now func() time.Time, readyEpoch uint64) (PromptDeliveryStatus, uint64, error) {
	for {
		target := i.agentObservationTarget()
		target.runtime.mu.Lock()
		i.mu.RLock()
		current := i.agentObservation == target.runtime
		i.mu.RUnlock()
		if !current {
			target.runtime.mu.Unlock()
			continue
		}
		attemptedAt := now()
		status, err := SendPromptWithStatus(target.server, prompt)
		sendEpoch, _ := i.recordPromptAttemptForObservation(status, attemptedAt, target.runtime, readyEpoch)
		target.runtime.mu.Unlock()
		return status, sendEpoch, err
	}
}

// PaneObservationSlack bounds the lateness a capture may carry past one
// declared cadence before the interval between them counts as unobserved —
// a hole (failed probes, a stalled serial walk) in which churn could hide.
// The slack must stay under the shortest completion arm's grace: a gap that
// could hide a whole churn→quiet cycle may never read as observed silence,
// so the horizon is one cadence plus this slack rather than a multiple of
// the cadence — capping the cadence input would have left the multiplied
// horizon uncapped (#5221 review). Ten seconds keeps ordinary scheduler and
// transport jitter inside the streak while a longer stall rebases it. The
// daemon reuses it as the round-trip freshness bound: a snapshot whose
// response lands past this long after the request left carries a depiction
// too stale to stand as current pane evidence.
const PaneObservationSlack = 10 * time.Second

// paneObservationHorizonCap bounds the recency a single capture may claim:
// whatever the caller's declared spacing, one capture proves the pane looked
// one way at ONE instant — it cannot vouch a longer watch than the shortest
// completion arm's window, so an operator-set interval of 30 minutes leaves
// the expiry at forty seconds even though the sample sequence itself keeps
// accumulating across on-schedule captures (#5221 review).
const paneObservationHorizonCap = 30 * time.Second

// NotePaneCaptureObserved stamps pane evidence at the completion of a
// successful pane capture (#5221 review). expectedInterval is the spacing
// THIS capture declares — the honest cadence until the next one — computed
// by the caller from its own schedule plus sweep-position drift, so the
// stamped paneObservationDueAt already carries the worst case the next gap
// can legitimately span. SEQUENCE membership is judged by that stored due
// horizon alone: a slower cadence arriving later — the live poll's 1s tick
// handing off to the backstop's 30s probe on attach — may NOT extend a
// window the previous capture never vouched for, and a faster one inside it
// stays continuous. Only a gap past dueAt — a poll the caller meant to run
// and lost — re-anchors: consecutive captures on a slow operator cadence
// are point samples, not a continuous watch, but they keep accumulating
// completion evidence instead of re-anchoring every poll (#5221 review).
// Separately, the RECENCY a capture vouches (paneObservationExpiresAt)
// never exceeds paneObservationHorizonCap, however slow the cadence —
// stillness may be proven by samples, but no sample may claim to have
// watched the gap it sits after. The return reports the re-anchor —
// per-frame state that pairs captures across the hole (a turn watcher's
// absence clock, a previous-frame latch) must reset with the streak, the
// same reset a failed capture forces (#5221 review).
//
// observedEpoch fences the stamp the same way the churn and stillness writes
// are fenced: a capture that completed BEFORE a prompt boundary or lifecycle
// change landed carries no knowledge of the pane under the new state, and a
// send racing the post-snapshot apply would otherwise let the pre-prompt
// frame keep the silent arm "fully observed" through a window whose whole
// turn happened between captures (#5221 review). A mismatched stamp is
// dropped whole — the next in-epoch capture re-anchors the streak.
func (i *Instance) NotePaneCaptureObserved(at time.Time, expectedInterval time.Duration, observedEpoch uint64) bool {
	if at.IsZero() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != observedEpoch {
		return false
	}
	reanchored := i.lastPaneCaptureAt.IsZero() || at.After(i.paneObservationDueAt)
	if reanchored {
		i.paneObservationStreakAt = at
		// A stillness candidate open across the hole never saw the pane
		// either — its "continuous" window would borrow unobserved time.
		i.taskRunStillCandidateAt = time.Time{}
	}
	// The DENSE watch — the run of captures each within one horizon cap of
	// the last — re-anchors on any over-cap gap AND on a missed poll: a gap
	// past the declared due horizon is unobserved time however short it is
	// in absolute terms, and "thirty seconds of silence" is a claim about a
	// watched pane, so the churn it could hide resets the clock the same
	// way (#5221 review). Point samples on a slow cadence keep the loose
	// sequence above, but neither kind of hole may carry a stale churn
	// stamp into the quiet arm's window.
	if reanchored || i.paneObservationWatchAt.IsZero() ||
		at.After(i.lastPaneCaptureAt.Add(paneObservationHorizonCap+PaneObservationSlack)) {
		i.paneObservationWatchAt = at
	}
	i.lastPaneCaptureAt = at
	// Two horizons, two claims (#5221 review): expiresAt is the RECENCY a
	// single capture may vouch for — bounded at the floor cap no matter how
	// slow the declared cadence, so an over-long interval can never make two
	// identical captures read as one continuous watch. dueAt is when the
	// NEXT on-schedule capture is due on the caller's honest (uncapped)
	// cadence — only a gap past that, a poll the caller meant to run and
	// lost, breaks the sample sequence's evidence: an operator's 30-minute
	// interval keeps consecutive samples accumulating completion evidence
	// rather than re-anchoring every poll.
	i.paneObservationExpiresAt = at.Add(min(expectedInterval, paneObservationHorizonCap) + PaneObservationSlack)
	i.paneObservationDueAt = at.Add(expectedInterval + PaneObservationSlack)
	return reanchored
}

// NotePaneCaptureFailed breaks the observation streak on a probe the daemon
// attempted and lost — a confirmed failed capture, or a snapshot whose
// transport errored before any frame could answer. Skipping the stamp is not
// enough: two successful captures straddling one failure can sit inside the
// gap horizon (35s apart under the paused path's 40s slack), leaving the old
// streak intact while the failed frame in between could have hidden active
// churn (#5221 review). Clearing lastPaneCaptureAt makes the next success
// re-anchor a fresh streak from its own instant; clearing the expiry keeps a
// stale streak from staying live through the blind window. An open stillness
// candidate dies too: the still run owes CONTINUOUS sight, and the blind
// interval inside it could hide the churn that proves it was a pause (#5221
// review). observedEpoch fences the clear like the stamp above — a probe
// outcome about a state the instance already left writes nothing.
func (i *Instance) NotePaneCaptureFailed(observedEpoch uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != observedEpoch {
		return
	}
	i.lastPaneCaptureAt = time.Time{}
	i.paneObservationStreakAt = time.Time{}
	i.paneObservationExpiresAt = time.Time{}
	i.paneObservationDueAt = time.Time{}
	i.paneObservationWatchAt = time.Time{}
	i.taskRunStillCandidateAt = time.Time{}
}
