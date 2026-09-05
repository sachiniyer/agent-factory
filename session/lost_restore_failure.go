package session

import (
	"fmt"
	"strings"
	"unicode"
)

const lostRestoreFailureMaxRunes = 512

// LostRestoreFailure is the durable terminal outcome of the daemon's automatic
// Lost-session restore loop. It stays on the session record until an explicit
// runtime replacement succeeds, so every client can distinguish an actively
// retrying Lost row from one the daemon has stopped retrying.
type LostRestoreFailure struct {
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
}

func (f LostRestoreFailure) valid() bool {
	return f.Attempts > 0 && strings.TrimSpace(f.Error) != ""
}

func cloneLostRestoreFailure(f LostRestoreFailure) *LostRestoreFailure {
	if !f.valid() {
		return nil
	}
	clone := f
	return &clone
}

func lostRestoreFailureFromData(f *LostRestoreFailure) LostRestoreFailure {
	if f == nil || !f.valid() {
		return LostRestoreFailure{}
	}
	clone := *f
	clone.Error = sanitizeLostRestoreError(clone.Error)
	return clone
}

func sanitizeLostRestoreError(raw string) string {
	clean := make([]rune, 0, lostRestoreFailureMaxRunes)
	spacePending := false
	truncated := false
	for _, r := range strings.ToValidUTF8(raw, "�") {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			spacePending = len(clean) > 0
			continue
		}
		if spacePending {
			if len(clean) >= lostRestoreFailureMaxRunes {
				truncated = true
				break
			}
			clean = append(clean, ' ')
			spacePending = false
		}
		if len(clean) >= lostRestoreFailureMaxRunes {
			truncated = true
			break
		}
		clean = append(clean, r)
	}
	if len(clean) == 0 {
		return "restore failed without a printable error"
	}
	if truncated {
		if len(clean) == lostRestoreFailureMaxRunes {
			clean = clean[:lostRestoreFailureMaxRunes-1]
		}
		clean = append(clean, '…')
	}
	return string(clean)
}

// Detail is the row wording shared by the TUI and API-facing callers. The web
// mirrors this structured projection from attempts+error.
func (f LostRestoreFailure) Detail() string {
	if !f.valid() {
		return ""
	}
	noun := "attempts"
	if f.Attempts == 1 {
		noun = "attempt"
	}
	return fmt.Sprintf("restore gave up after %d %s: %s", f.Attempts, noun, f.Error)
}

// SetLostRestoreFailure commits the automatic restore loop's terminal failure
// to the live session record. Persistence and client notification are owned by
// the daemon after this in-memory edge.
func (i *Instance) SetLostRestoreFailure(attempts int, restoreErr error) bool {
	if attempts <= 0 || restoreErr == nil {
		return false
	}
	// The daemon log keeps restoreErr verbatim for diagnosis. The durable copy is
	// client-facing: it lands in instances.json, API responses, and a one-line TUI
	// row, so collapse controls/whitespace and bound it before it crosses that
	// trust boundary.
	next := LostRestoreFailure{Attempts: attempts, Error: sanitizeLostRestoreError(restoreErr.Error())}
	if !next.valid() {
		return false
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lostRestoreFailure == next {
		return false
	}
	i.lostRestoreFailure = next
	i.touchLocked()
	return true
}

// ClearLostRestoreFailureAtObservation retires terminal state only when the
// positive liveness evidence still names the current runtime. Replacement
// invalidates the generation before its own bookkeeping, so a late predecessor
// answer cannot clear a successor's failure.
func (i *Instance) ClearLostRestoreFailureAtObservation(generation AgentObservationGeneration) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if generation.value == 0 || i.agentObservationGeneration.Load() != generation.value {
		return false
	}
	if !i.lostRestoreFailure.valid() {
		return false
	}
	i.lostRestoreFailure = LostRestoreFailure{}
	i.touchLocked()
	return true
}

// ClearLostRestoreFailure retires a predecessor runtime's terminal restore
// outcome once a replacement crosses its live boundary.
func (i *Instance) ClearLostRestoreFailure() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.lostRestoreFailure.valid() {
		return false
	}
	i.lostRestoreFailure = LostRestoreFailure{}
	i.touchLocked()
	return true
}

// LostRestoreFailureSnapshot returns a detached copy for row renderers and
// lifecycle bookkeeping.
func (i *Instance) LostRestoreFailureSnapshot() *LostRestoreFailure {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneLostRestoreFailure(i.lostRestoreFailure)
}

// ReconcileLostRestoreFailure mirrors the daemon's durable terminal state into
// an already-open TUI row. Cold-start materialization uses FromInstanceData;
// this is the same-session snapshot path where liveness may remain Lost.
func (i *Instance) ReconcileLostRestoreFailure(failure *LostRestoreFailure) bool {
	next := lostRestoreFailureFromData(failure)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lostRestoreFailure == next {
		return false
	}
	i.lostRestoreFailure = next
	i.touchLocked()
	return true
}
