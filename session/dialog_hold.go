package session

import (
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// DialogHold says the agent's trust gate has held a launch dialog it will not
// answer, on a pane that has not changed for the gate's settle time (#7635).
// The gate still holds and sends no key; this only makes the hold visible.
//
// It rides on Observation, and on the wire from a sandbox's agent server. The
// field is additive: a peer that predates it sends none, and nothing is raised.
// It carries no text. What af shows is built here from Kind and the two
// integers, so nothing a peer sends is shown as af's own words.
type DialogHold struct {
	// Kind is the hold's kind, from tmux's closed set.
	Kind tmux.DialogHoldKind `json:"kind"`
	// Width and Height are the pane's size; zero when unknown.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Since is when the pane last changed. A zero Since is no hold.
	Since time.Time `json:"since"`
	// Key names the held pane: the same pane showing the same bytes keeps its
	// key, across polls and across a daemon restart. The daemon flags a
	// session once per key.
	Key string `json:"key"`
}

// Subject is what af is waiting on, with the pane size when known: "a
// folder-trust dialog it can't read (pane 60×12)".
func (h DialogHold) Subject() string { return tmux.DialogHoldSubject(h.Kind, h.Width, h.Height) }

// Reason is the hold as one line for a live session's attention signal.
func (h DialogHold) Reason() string { return tmux.DialogHoldReason(h.Kind, h.Width, h.Height) }

// dialogHoldKeyLen bounds a key af will keep. The local recorder's is 32 hex
// characters; the bounds leave room for a peer that hashes differently.
const (
	minDialogHoldKeyLen = 16
	maxDialogHoldKeyLen = 64
)

// NormalizedDialogHold is the one boundary a hold crosses before the daemon
// acts on it, whether it came from this process or off the wire. It returns
// nil for what is not a hold: a nil one, a zero Since, or a key that is not
// lowercase hex of a sane length. A kind outside the closed set becomes
// DialogHoldOther, and an implausible size is dropped.
func NormalizedDialogHold(h *DialogHold) *DialogHold {
	if h == nil || h.Since.IsZero() || !validDialogHoldKey(h.Key) {
		return nil
	}
	n := DialogHold{Kind: tmux.KnownDialogHoldKind(h.Kind), Since: h.Since, Key: h.Key}
	// Both dimensions or neither: DialogHoldSubject prints them only together.
	if h.Width >= 1 && h.Height >= 1 && h.Width <= 9999 && h.Height <= 9999 {
		n.Width, n.Height = h.Width, h.Height
	}
	return &n
}

func validDialogHoldKey(key string) bool {
	if len(key) < minDialogHoldKeyLen || len(key) > maxDialogHoldKeyLen {
		return false
	}
	for _, c := range []byte(key) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// dialogHoldBackend is implemented by a backend whose trust gate runs in this
// process and can report a settled hold.
type dialogHoldBackend interface {
	DialogHold(i *Instance) *DialogHold
}

// DialogHold reports the settled hold of the local backend's trust gate, or nil.
func (b *LocalBackend) DialogHold(i *Instance) *DialogHold {
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil {
		return nil
	}
	notice, ok := ts.DialogHoldNotice()
	if !ok {
		return nil
	}
	return &DialogHold{Kind: notice.Kind, Width: notice.Width, Height: notice.Height, Since: notice.Since, Key: notice.Key}
}

// DialogHold returns the settled hold of this session's own trust gate, or
// nil: no hold, or a backend whose gate runs elsewhere.
func (i *Instance) DialogHold() *DialogHold {
	if b, ok := i.currentBackend().(dialogHoldBackend); ok {
		return b.DialogHold(i)
	}
	return nil
}

// DialogHoldFlagged returns the key of the held launch-dialog pane this
// session was last flagged for, or "". The key is persisted with the row, so
// that across a daemon restart the same held pane is neither flagged twice nor
// flagged again after the user dismissed its notice; it is cleared when the
// hold resolves. It also returns the SetAt of the
// attention request the daemon raised for a held dialog if that request is
// still on the row. A zero time means no such request: none was raised, or it
// was dismissed, answered, or replaced by someone else's.
func (i *Instance) DialogHoldFlagged() (key string, ownedSetAt time.Time) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.attention != nil && i.attention.Origin == AttentionOriginDialogHold {
		ownedSetAt = i.attention.SetAt
	}
	return i.dialogHoldFlagged, ownedSetAt
}

// SetDialogHoldFlagged records the held pane this session is flagged for, and
// returns the previous key for a caller whose persist fails. "" forgets it.
func (i *Instance) SetDialogHoldFlagged(key string) (previous string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	previous = i.dialogHoldFlagged
	if previous != key {
		i.dialogHoldFlagged = key
		i.touchLocked()
	}
	return previous
}
