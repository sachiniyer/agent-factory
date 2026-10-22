package session

import (
	"errors"
	"strings"
	"time"
)

// ErrAttentionNotLive rejects an attention request on a session whose agent is
// not running or waiting: Lost, Dead, Archived, or stopped at a usage limit.
var ErrAttentionNotLive = errors.New("attention applies only to a live agent")

// MaxAttentionReasonLen bounds the reason an agent attaches to an attention
// request. The reason is shown in a row tooltip, not a document.
const MaxAttentionReasonLen = 500

// Attention is an agent's explicit request for the user, set with
// `af sessions attention --self`. It clears when the user types into the
// agent pane, opens the agent tab in the web UI, or when the agent starts work
// again after it went idle.
type Attention struct {
	Reason string    `json:"reason,omitempty"`
	SetAt  time.Time `json:"set_at"`
	// SeenIdle records that the agent went idle after it asked. The agent asks
	// while it is still running, so only a Running edge after an idle one is
	// the user's answer starting new work.
	SeenIdle bool `json:"seen_idle,omitempty"`
}

func cloneAttention(a *Attention) *Attention {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}

// NormalizeAttentionReason trims the reason and caps its length in runes.
func NormalizeAttentionReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if r := []rune(reason); len(r) > MaxAttentionReasonLen {
		reason = string(r[:MaxAttentionReasonLen])
	}
	return reason
}

// Attention returns a copy of the session's attention request, or nil.
func (i *Instance) Attention() *Attention {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneAttention(i.attention)
}

// HasAttention reports whether the agent asked for the user.
func (i *Instance) HasAttention() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.attention != nil
}

// SetAttention records an attention request and returns the previous value,
// so a caller whose persist fails can put it back with RestoreAttention. It
// checks liveness under the same lock that liveness changes take, so a
// transition either lands first and is refused here, or lands after and clears
// the request.
func (i *Instance) SetAttention(reason string, now time.Time) (*Attention, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	switch i.liveness {
	case LiveArchived, LiveLost, LiveDead, LiveLimitReached:
		return nil, ErrAttentionNotLive
	}
	previous := i.attention
	i.attention = &Attention{Reason: NormalizeAttentionReason(reason), SetAt: now.UTC()}
	// A status decision captured before the request must not mark it seen idle
	// or clear it; the poll re-observes on its next tick.
	i.stateEpoch++
	i.touchLocked()
	return previous, nil
}

// ClearAttention removes the attention request. A non-zero setAt clears it
// only while it is still the request set at that time, so a client that saw an
// older request cannot erase a newer one. It returns the removed value and
// whether there was one to remove.
func (i *Instance) ClearAttention(setAt time.Time) (*Attention, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	previous := i.attention
	if previous == nil || (!setAt.IsZero() && !previous.SetAt.Equal(setAt)) {
		return nil, false
	}
	i.attention = nil
	i.touchLocked()
	return previous, true
}

// RestoreAttention puts back a value that SetAttention or ClearAttention
// replaced, after the caller could not persist the change.
func (i *Instance) RestoreAttention(a *Attention) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.attention = cloneAttention(a)
	i.touchLocked()
}

// noteAttentionLivenessLocked advances an attention request across a liveness
// change. Caller holds i.mu for writing.
func (i *Instance) noteAttentionLivenessLocked() {
	if i.attention == nil {
		return
	}
	switch i.liveness {
	case LiveReady:
		i.attention.SeenIdle = true
	case LiveRunning:
		if i.attention.SeenIdle {
			i.attention = nil
		}
	case LivenessUnset:
	default:
		i.attention = nil
	}
}
