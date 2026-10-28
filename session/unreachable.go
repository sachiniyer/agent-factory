package session

import (
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// UnreachableData is the additive projection of a live probe outage (#5571
// W1.3): the daemon's remote observation stopped being answered at Since, and
// Error is the last transport failure, redacted and bounded at store time so it
// can ride API payloads, TUI detail lines, and refusal text alike.
//
// It is deliberately an OBSERVATION FLAG, not a liveness: an unanswered probe
// is not evidence about the agent, so the debounce preserves last-known
// liveness underneath (a Ready row stays Ready, a Lost row stays Lost) while
// the flag says the current answer cannot be re-confirmed. Lost+unreachable is
// therefore the honest pair — "marked Lost, and the mark cannot be re-checked
// right now" — rather than a contradiction.
type UnreachableData struct {
	Since time.Time `json:"since"`
	Error string    `json:"error"`
}

// valid mirrors the durable-state convention: a flag with no observed outage
// start or no recorded cause is not a flag at all.
func (u *UnreachableData) valid() bool {
	return u != nil && !u.Since.IsZero() && u.Error != ""
}

func unreachableSinceFromData(u *UnreachableData) time.Time {
	if !u.valid() {
		return time.Time{}
	}
	return u.Since
}

func unreachableErrFromData(u *UnreachableData) string {
	if !u.valid() {
		return ""
	}
	return u.Error
}

// SetUnreachable records an unanswered remote probe. The since-time anchors
// the FIRST failure of the episode — later failures refresh the error text but
// never move it, so the projected age always measures the whole outage rather
// than the last tick of it. The error text crosses every client boundary the
// daemon owns (instances.json-adjacent snapshots, TUI rows, web rails, refusal
// prose), so it is collapsed, bounded, and passed through the token-field
// backstop before it is stored — a remote-client error can embed a URL, and an
// access_token query parameter must never reach a projection (#5571 review).
func (i *Instance) SetUnreachable(err error) {
	if err == nil {
		return
	}
	clean := agentproto.RedactAccessTokenText(sanitizeLostRestoreError(err.Error()))
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.unreachableErr == clean && !i.unreachableSince.IsZero() {
		return
	}
	if i.unreachableSince.IsZero() {
		i.unreachableSince = time.Now()
	}
	i.unreachableErr = clean
	i.touchLocked()
}

// ClearUnreachable retires the flag on transport evidence that is not the
// observation-generation-gated answered probe: an attach streaming the pane,
// a lifecycle op replacing the runtime, a tombstone, an archived row — every
// unprobed tick breaks the run for the flag exactly as it does for the
// debounce's failure count.
func (i *Instance) ClearUnreachable() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.unreachableSince.IsZero() {
		return false
	}
	i.unreachableSince = time.Time{}
	i.unreachableErr = ""
	i.touchLocked()
	return true
}

// ClearUnreachableAtObservation retires the flag only while the answered probe
// still names the current runtime — the same generation gate
// ClearLostRestoreWaitAtObservation uses, so a stale predecessor answer cannot
// un-mark an outage observed against a replacement.
func (i *Instance) ClearUnreachableAtObservation(generation AgentObservationGeneration) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if generation.value == 0 || i.agentObservationGeneration.Load() != generation.value {
		return false
	}
	if i.unreachableSince.IsZero() {
		return false
	}
	i.unreachableSince = time.Time{}
	i.unreachableErr = ""
	i.touchLocked()
	return true
}

// unreachableDataLocked projects the live flag; callers hold i.mu. The pair is
// atomic by construction — the object exists only while it carries both halves.
func (i *Instance) unreachableDataLocked() *UnreachableData {
	if i.unreachableSince.IsZero() || i.unreachableErr == "" {
		return nil
	}
	return &UnreachableData{Since: i.unreachableSince, Error: i.unreachableErr}
}

// UnreachableSnapshot returns a detached copy for row renderers, lifecycle
// gating, and refusal text.
func (i *Instance) UnreachableSnapshot() *UnreachableData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.unreachableDataLocked()
}

// ReconcileUnreachable mirrors the daemon's flag into an already-open TUI
// projection row — the snapshot path that does not rebuild through
// FromInstanceData.
func (i *Instance) ReconcileUnreachable(u *UnreachableData) bool {
	since, errText := unreachableSinceFromData(u), unreachableErrFromData(u)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.unreachableSince.Equal(since) && i.unreachableErr == errText {
		return false
	}
	i.unreachableSince = since
	i.unreachableErr = errText
	i.touchLocked()
	return true
}
