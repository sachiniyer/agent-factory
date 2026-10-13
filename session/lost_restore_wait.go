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
	DeadlineWaits int    `json:"deadline_waits"`
	Error         string `json:"error"`
}

func (w LostRestoreWait) valid() bool {
	return w.Attempts > 0 && strings.TrimSpace(w.Error) != ""
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
func (i *Instance) SetLostRestoreWait(attempts, deadlineWaits int, restoreErr error) bool {
	if attempts <= 0 || deadlineWaits < 0 || restoreErr == nil {
		return false
	}
	// Same trust boundary as SetLostRestoreFailure: the durable copy lands in
	// instances.json, API responses, and a one-line TUI row, so collapse
	// controls/whitespace and bound it.
	next := LostRestoreWait{Attempts: attempts, DeadlineWaits: deadlineWaits, Error: sanitizeLostRestoreError(restoreErr.Error())}
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
