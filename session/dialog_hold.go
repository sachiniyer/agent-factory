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
type DialogHold struct {
	// Subject is what af is waiting on, with the pane size when known: "a
	// folder-trust dialog it can't read (pane 60×12)".
	Subject string `json:"subject"`
	// Since is when the pane last changed. One held, unchanged pane keeps one
	// Since, which is how the daemon raises its signal once per hold.
	Since time.Time `json:"since"`
}

// Reason is the hold as one line for a live session's attention signal.
func (h DialogHold) Reason() string {
	return tmux.DialogHoldNotice{Subject: h.Subject, Since: h.Since}.Reason()
}

func cloneDialogHold(h *DialogHold) *DialogHold {
	if h == nil {
		return nil
	}
	c := *h
	return &c
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
	return &DialogHold{Subject: notice.Subject, Since: notice.Since}
}

// DialogHold returns the settled hold of this session's own trust gate, or
// nil: no hold, or a backend whose gate runs elsewhere.
func (i *Instance) DialogHold() *DialogHold {
	if b, ok := i.currentBackend().(dialogHoldBackend); ok {
		return b.DialogHold(i)
	}
	return nil
}
