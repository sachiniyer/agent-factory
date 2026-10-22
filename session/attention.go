package session

import (
	"strings"
	"time"
)

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
// so a caller whose persist fails can put it back with RestoreAttention.
func (i *Instance) SetAttention(reason string, now time.Time) *Attention {
	i.mu.Lock()
	defer i.mu.Unlock()
	previous := i.attention
	i.attention = &Attention{Reason: NormalizeAttentionReason(reason), SetAt: now.UTC()}
	i.touchLocked()
	return previous
}

// ClearAttention removes the attention request. It returns the removed value
// and whether there was one to remove.
func (i *Instance) ClearAttention() (*Attention, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	previous := i.attention
	if previous == nil {
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
