package session

import (
	"fmt"
	"strings"
)

// LostRestoreWait is the durable non-terminal state of the daemon's automatic
// Lost-session restore loop while it is waiting on the environment: the last
// failure was a resource limit external to the session (a full disk, an
// exhausted fd table, a memory or PID ceiling), so the loop keeps a capped
// backoff armed instead of spending the bounded structural budget (#5295). It
// stays on the session record until the restore succeeds, a structural failure
// retires the wait, or a runtime replacement lands — the same edges
// LostRestoreFailure uses — so every client can tell a session af is still
// automatically retrying from one it has stopped on.
type LostRestoreWait struct {
	Attempts int `json:"attempts"`
	// DeadlineWaits is the bounded deadline class's share of Attempts at the
	// last arm. It is durable for the same reason the wait is: the class's
	// give-up bound must survive a daemon restart, or every restart would
	// grant a permanently wedged probe another full share of free waits
	// (#5295 review).
	DeadlineWaits int `json:"deadline_waits"`
	// TotalAttempts is the episode's cumulative attempt count at the last
	// arm — distinct from Attempts, which is only the environmental streak.
	// Without it a restart would seed the cumulative counter from the streak
	// alone and under-report a mixed episode's real try count (#5295 review).
	TotalAttempts int    `json:"total_attempts"`
	Error         string `json:"error"`
}

func (w LostRestoreWait) valid() bool {
	return w.Attempts > 0 && strings.TrimSpace(w.Error) != ""
}

// LostRestoreAttempts is the durable copy of a restore episode's retry
// accounting — the three counters lostRestoreState holds in memory: the
// structural Recover streak (the #3347 budget), the preserve-push streak, and
// the cumulative attempt count across both classes (#5295 review). Unlike the
// wait, which describes the CURRENT claim and must clear the moment a failure
// goes structural, the accounting describes the EPISODE: it survives a cleared
// wait, a structural charge, and the runtime replacement/confirmation window,
// so a daemon restart in any of those windows cannot reconstruct a fresh zero
// streak for an episode that is in fact nearly exhausted. It retires only at
// true episode-end edges: a confirmed replacement, a self-healed runtime, a
// deliberate budget reset, or a terminal give-up (which records its own count).
type LostRestoreAttempts struct {
	// Structural is consecutiveFailures — Recover failures charged to the
	// bounded budget since the episode began.
	Structural int `json:"structural"`
	// Preserve is preserveFailureAttempts — preserve-push failures charged
	// to their own bounded budget.
	Preserve int `json:"preserve"`
	// Total is totalAttempts — every failure recorded this episode across
	// all classes. It is the count "restored after N attempts" reports.
	Total int `json:"total"`
}

func (a LostRestoreAttempts) valid() bool {
	return a.Structural > 0 || a.Preserve > 0 || a.Total > 0
}

func cloneLostRestoreAttempts(a LostRestoreAttempts) *LostRestoreAttempts {
	if !a.valid() {
		return nil
	}
	clone := a
	return &clone
}

func lostRestoreAttemptsFromData(a *LostRestoreAttempts) LostRestoreAttempts {
	if a == nil || !a.valid() {
		return LostRestoreAttempts{}
	}
	clone := *a
	if clone.Structural < 0 {
		clone.Structural = 0
	}
	if clone.Preserve < 0 {
		clone.Preserve = 0
	}
	if clone.Total < 0 {
		clone.Total = 0
	}
	return clone
}

// SetLostRestoreAttempts converges the durable record to the episode's
// current accounting, refreshed at every arm and charge. An all-zero value
// clears the record — episode-end edges write the empty tuple so the record
// cannot outlive the episode it counted.
func (i *Instance) SetLostRestoreAttempts(attempts LostRestoreAttempts) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !attempts.valid() {
		if !i.lostRestoreAttempts.valid() {
			return false
		}
		i.lostRestoreAttempts = LostRestoreAttempts{}
		i.touchLocked()
		return true
	}
	if i.lostRestoreAttempts == attempts {
		return false
	}
	i.lostRestoreAttempts = attempts
	i.touchLocked()
	return true
}

// ClearLostRestoreAttempts retires the durable accounting at an episode-end
// edge. Deliberately NOT called by noteRuntimeReplaced: a death inside the
// confirmation window continues the same episode, so the counts must survive
// the swap to keep charging it correctly.
func (i *Instance) ClearLostRestoreAttempts() bool {
	return i.SetLostRestoreAttempts(LostRestoreAttempts{})
}

// LostRestoreAttemptsSnapshot returns a detached copy for restart seeding and
// diagnostics.
func (i *Instance) LostRestoreAttemptsSnapshot() *LostRestoreAttempts {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneLostRestoreAttempts(i.lostRestoreAttempts)
}

func cloneLostRestoreWait(w LostRestoreWait) *LostRestoreWait {
	if !w.valid() {
		return nil
	}
	clone := w
	return &clone
}

func lostRestoreWaitFromData(w *LostRestoreWait) LostRestoreWait {
	if w == nil || !w.valid() {
		return LostRestoreWait{}
	}
	clone := *w
	if clone.DeadlineWaits < 0 {
		clone.DeadlineWaits = 0
	}
	if clone.TotalAttempts < clone.Attempts {
		// Rows written before the field existed carry zero; the cumulative
		// count can never be smaller than the streak inside it.
		clone.TotalAttempts = clone.Attempts
	}
	clone.Error = sanitizeLostRestoreError(clone.Error)
	return clone
}

// Detail is the row wording shared by the TUI and API-facing callers. The web
// mirrors this structured projection from attempts+error.
func (w LostRestoreWait) Detail() string {
	if !w.valid() {
		return ""
	}
	noun := "environmental failures"
	if w.Attempts == 1 {
		noun = "environmental failure"
	}
	return fmt.Sprintf("restore is waiting on the environment (%d %s): %s", w.Attempts, noun, w.Error)
}

// SetLostRestoreWait commits the automatic restore loop's environmental wait
// to the live session record. Persistence and client notification are owned by
// the daemon after this in-memory edge.
func (i *Instance) SetLostRestoreWait(attempts, deadlineWaits, totalAttempts int, restoreErr error) bool {
	if attempts <= 0 || deadlineWaits < 0 || totalAttempts < attempts || restoreErr == nil {
		return false
	}
	// Same trust boundary as SetLostRestoreFailure: the durable copy lands in
	// instances.json, API responses, and a one-line TUI row, so collapse
	// controls/whitespace and bound it.
	next := LostRestoreWait{Attempts: attempts, DeadlineWaits: deadlineWaits, TotalAttempts: totalAttempts, Error: sanitizeLostRestoreError(restoreErr.Error())}
	if !next.valid() {
		return false
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lostRestoreWait == next {
		return false
	}
	i.lostRestoreWait = next
	i.touchLocked()
	return true
}

// ClearLostRestoreWaitAtObservation retires the wait only when the positive
// liveness evidence still names the current runtime. Replacement invalidates
// the generation before its own bookkeeping, so a late predecessor answer
// cannot clear a successor's wait.
func (i *Instance) ClearLostRestoreWaitAtObservation(generation AgentObservationGeneration) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if generation.value == 0 || i.agentObservationGeneration.Load() != generation.value {
		return false
	}
	if !i.lostRestoreWait.valid() {
		return false
	}
	i.lostRestoreWait = LostRestoreWait{}
	i.touchLocked()
	return true
}

// ClearLostRestoreWait retires the environmental wait once a restore attempt
// goes through, a structural failure takes over the accounting, or a
// replacement crosses its live boundary.
func (i *Instance) ClearLostRestoreWait() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.lostRestoreWait.valid() {
		return false
	}
	i.lostRestoreWait = LostRestoreWait{}
	i.touchLocked()
	return true
}

// LostRestoreWaitSnapshot returns a detached copy for row renderers and
// lifecycle bookkeeping.
func (i *Instance) LostRestoreWaitSnapshot() *LostRestoreWait {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneLostRestoreWait(i.lostRestoreWait)
}

// ReconcileLostRestoreWait mirrors the daemon's durable wait state into an
// already-open TUI row. Cold-start materialization uses FromInstanceData;
// this is the same-session snapshot path where liveness may remain Lost.
func (i *Instance) ReconcileLostRestoreWait(wait *LostRestoreWait) bool {
	next := lostRestoreWaitFromData(wait)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lostRestoreWait == next {
		return false
	}
	i.lostRestoreWait = next
	i.touchLocked()
	return true
}

// MarkLostRestoreDeadlineSpent records that the bounded deadline class already
// spent its environmental share this episode and charged the structural
// budget. Unlike the wait — which must clear the moment the failure is the
// session's own — this marker survives that clear, so a daemon restart in the
// structural-backoff window cannot re-grant the wedged probe a fresh share of
// free waits (#5295 review). It retires at the same episode-end edges the wait
// does.
func (i *Instance) MarkLostRestoreDeadlineSpent() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lostRestoreDeadlineSpent {
		return false
	}
	i.lostRestoreDeadlineSpent = true
	i.touchLocked()
	return true
}

// LostRestoreDeadlineSpent reports whether the marker is set — the daemon
// reads it before its own state lock to seed the bounded share across
// restarts.
func (i *Instance) LostRestoreDeadlineSpent() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.lostRestoreDeadlineSpent
}

// ClearLostRestoreDeadlineSpent retires the marker at an episode-end edge: a
// landed preserve push, a force-replaced sandbox, a confirmed replacement, or
// a terminal give-up (#5295 review). Deliberately NOT cleared at the first
// answered observation: confirmation needs a sustained run of answers, and a
// runtime that dies inside that window continues the same episode — retiring
// the marker early would let a restart re-grant the spent share.
func (i *Instance) ClearLostRestoreDeadlineSpent() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.lostRestoreDeadlineSpent {
		return false
	}
	i.lostRestoreDeadlineSpent = false
	i.touchLocked()
	return true
}
