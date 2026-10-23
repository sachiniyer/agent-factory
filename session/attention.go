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

// attentionAnswerIdle is how long the agent must stay idle before a Running
// edge counts as the user's answer. A shorter still spell is the agent pausing
// inside its own turn — one quiet poll between `attention --self` and its
// closing message — and the output after it is not new work.
const attentionAnswerIdle = 10 * time.Second

// Attention is an agent's explicit request for the user, set with
// `af sessions attention --self`. It clears when the user types into the
// agent pane, opens the agent tab in the web UI, or when the agent starts work
// again after it was idle for attentionAnswerIdle.
type Attention struct {
	Reason string    `json:"reason,omitempty"`
	SetAt  time.Time `json:"set_at"`
	// SeenIdle records that the agent went idle after it asked, and IdleSince
	// when. The agent asks while it is still running, so only a Running edge
	// after a long enough idle spell is the user's answer starting new work. A
	// record without IdleSince predates it and counts as long enough.
	SeenIdle  bool      `json:"seen_idle,omitempty"`
	IdleSince time.Time `json:"idle_since,omitzero"`
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

// AttentionUndo is what a caller whose persist failed hands to
// RestoreAttention: the replaced value and the state epoch just after the change.
type AttentionUndo struct {
	previous *Attention
	epoch    uint64
}

// SetAttention records an attention request and returns how to undo it, for a
// caller whose persist fails. It checks liveness and the kill tombstone under
// the same lock that liveness changes take, so a transition either lands first
// and is refused here, or lands after and clears the request.
func (i *Instance) SetAttention(reason string, now time.Time) (AttentionUndo, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if attentionStoppedLocked(i) {
		return AttentionUndo{}, ErrAttentionNotLive
	}
	previous := i.attention
	i.attention = &Attention{Reason: NormalizeAttentionReason(reason), SetAt: now.UTC()}
	// A status decision captured before the request must not mark it seen idle
	// or clear it; the poll re-observes on its next tick.
	i.stateEpoch++
	i.touchLocked()
	return AttentionUndo{previous: previous, epoch: i.stateEpoch}, nil
}

// ClearAttention removes the attention request. A non-zero setAt clears it
// only while it is still the request set at that time, so a client that saw an
// older request cannot erase a newer one. It returns the removed value and
// whether there was one to remove.
func (i *Instance) ClearAttention(setAt time.Time) (AttentionUndo, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	previous := i.attention
	if previous == nil || (!setAt.IsZero() && !previous.SetAt.Equal(setAt)) {
		return AttentionUndo{}, false
	}
	i.attention = nil
	i.touchLocked()
	return AttentionUndo{previous: previous, epoch: i.stateEpoch}, true
}

// RestoreAttention puts back the value that SetAttention or ClearAttention
// replaced, after the caller could not persist the change. When the state epoch
// moved since that change, a transition has already reconciled the current
// value, so the rollback leaves it as it is rather than replay a stale one.
func (i *Instance) RestoreAttention(u AttentionUndo) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.stateEpoch != u.epoch {
		return
	}
	i.attention = cloneAttention(u.previous)
	i.dropAttentionIfStoppedLocked()
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
		if !i.attention.SeenIdle {
			i.attention.SeenIdle = true
			i.attention.IdleSince = instanceNow().UTC()
		}
	case LiveRunning:
		if !i.attention.SeenIdle {
			return
		}
		since := i.attention.IdleSince
		if since.IsZero() || instanceNow().Sub(since) >= attentionAnswerIdle {
			i.attention = nil
			return
		}
		i.attention.SeenIdle = false
		i.attention.IdleSince = time.Time{}
	case LivenessUnset:
	default:
		i.attention = nil
	}
}

// dropAttentionIfStoppedLocked removes a request on a session that is not
// live. Paths that set liveness without noteStateChangeLocked call it, so no
// path reaches Lost, Dead, Archived or LimitReached with a request still set.
// Caller holds i.mu for writing, or owns i before it is shared.
func (i *Instance) dropAttentionIfStoppedLocked() {
	if attentionStoppedLocked(i) {
		i.attention = nil
	}
}

// attentionStoppedLocked reports whether no agent can answer a request: the
// session is not live, or a kill has committed. Caller holds i.mu.
func attentionStoppedLocked(i *Instance) bool {
	if i.userKilled {
		return true
	}
	switch i.liveness {
	case LiveArchived, LiveLost, LiveDead, LiveLimitReached:
		return true
	}
	return false
}
