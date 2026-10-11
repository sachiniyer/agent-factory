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

// SendPromptWithEvidence starts the attempt timestamp only after any pane
// snapshot already in flight has completed, then records the delivery verdict
// before another snapshot can begin.
func (i *Instance) SendPromptWithEvidence(prompt string, now func() time.Time) (PromptDeliveryStatus, error) {
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
		i.recordPromptAttemptForObservation(status, attemptedAt, target.runtime)
		target.runtime.mu.Unlock()
		return status, err
	}
}

// paneObservationSlack bounds the lateness a capture may carry past one
// declared cadence before the interval between them counts as unobserved —
// a hole (failed probes, a stalled serial walk) in which churn could hide.
// The slack must stay under the shortest completion arm's grace: a gap that
// could hide a whole churn→quiet cycle may never read as observed silence,
// so the horizon is one cadence plus this slack rather than a multiple of
// the cadence — capping the cadence input would have left the multiplied
// horizon uncapped (#5221 review). Ten seconds keeps ordinary scheduler and
// transport jitter inside the streak while a longer stall rebases it.
const paneObservationSlack = 10 * time.Second

// NotePaneCaptureObserved stamps pane evidence at the completion of a
// successful pane capture (#5221 review). expectedInterval is the expiry
// reserve THIS capture declares — the honest spacing until the next one —
// computed by the caller from its own schedule plus sweep-position drift, so
// the stamped paneObservationExpiresAt already carries the worst case the
// next gap can legitimately span. Continuity is judged by that stored expiry
// alone: a slower cadence arriving later — the live poll's 1s tick handing
// off to the backstop's 30s probe on attach — may NOT extend a window the
// previous capture never vouched for, and a faster one inside it stays
// continuous (#5221 review). An unobserved interval is not silence, however
// short the wall-clock hole: a starved streak merely holds the run until
// observation resumes; the release arms measure observed silence, never
// elapsed wall time. The return reports the re-anchor — per-frame state that
// pairs captures across the hole (a turn watcher's absence clock, a
// previous-frame latch) must reset with the streak, the same reset a failed
// capture forces (#5221 review).
func (i *Instance) NotePaneCaptureObserved(at time.Time, expectedInterval time.Duration) bool {
	if at.IsZero() {
		return false
	}
	maxGap := expectedInterval + paneObservationSlack
	i.mu.Lock()
	defer i.mu.Unlock()
	reanchored := i.paneObservationExpiresAt.IsZero() || at.After(i.paneObservationExpiresAt)
	if reanchored {
		i.paneObservationStreakAt = at
		// A stillness candidate open across the hole never saw the pane
		// either — its "continuous" window would borrow unobserved time.
		i.taskRunStillCandidateAt = time.Time{}
	}
	i.lastPaneCaptureAt = at
	i.paneObservationExpiresAt = at.Add(maxGap)
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
// review).
func (i *Instance) NotePaneCaptureFailed() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.lastPaneCaptureAt = time.Time{}
	i.paneObservationStreakAt = time.Time{}
	i.paneObservationExpiresAt = time.Time{}
	i.taskRunStillCandidateAt = time.Time{}
}
