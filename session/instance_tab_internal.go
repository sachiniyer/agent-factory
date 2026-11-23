package session

import "github.com/sachiniyer/agent-factory/session/tmux"

// replaceTabFieldLocked swaps the tab at idx for a COPY carrying the mutation f
// applies, instead of writing the field in place. Callers must hold i.mu for
// writing.
//
// GetTabs copies only the SLICE and hands out the same *Tab pointers, which
// callers (tree.TabLabels on the render path, the pane refresh) then read
// without holding i.mu — so assigning to a live tab's Name/ID races those
// readers. Copy-on-write keeps the readers race-free: a reader that already
// holds the old pointer keeps reading a consistent old value, and the next
// GetTabs hands out the new one. The tmux pointer rides along on the copy, so
// the tab's live session is preserved across the swap (no PTY blip).
func (i *Instance) replaceTabFieldLocked(idx int, f func(*Tab)) {
	cp := *i.Tabs[idx]
	f(&cp)
	if cp.ID != i.Tabs[idx].ID || cp.Name != i.Tabs[idx].Name || cp.tmux != i.Tabs[idx].tmux {
		i.touchLocked()
	}
	i.Tabs[idx] = &cp
}

// setTmuxLocked stores ts as the agent tab's tmux session, materializing the
// single Agent tab on first assignment so the agent session is always Tabs[0].
// Passing nil clears the session but leaves the tab in place (and is a no-op
// before the agent tab exists). Callers must hold i.mu for writing.
func (i *Instance) setTmuxLocked(ts *tmux.TmuxSession) {
	if len(i.Tabs) == 0 {
		if ts == nil {
			return
		}
		i.Tabs = []*Tab{newAgentTab(ts)}
		i.touchLocked()
		return
	}
	if i.Tabs[0].tmux != ts {
		i.Tabs[0].tmux = ts
		i.touchLocked()
	}
}

// tmuxLocked returns the agent tab's tmux session, or nil when the instance has
// no agent tab yet (not started) or is remote. Callers must hold i.mu (read or
// write).
func (i *Instance) tmuxLocked() *tmux.TmuxSession {
	if len(i.Tabs) == 0 {
		return nil
	}
	return i.Tabs[0].tmux
}

// GetTabs returns a snapshot of the instance's tab list under the instance
// mutex. The returned slice is a copy, so callers (the UI tab bar) can iterate
// it without racing concurrent tab mutation.
//
// The *Tab elements are the LIVE pointers, not copies, and callers read their
// Name/Kind/ID off-lock (tree.TabLabels on the render path, the TUI's
// tabNameAt/tabIndexByName). That is safe because those fields are never
// assigned in place once a tab is in i.Tabs: a tab's name can change, but the
// writers that change it — RenameTab and ReconcileTabsFromData — swap in a
// COPY carrying the new value (replaceTabFieldLocked) instead of writing the
// object a reader is already holding. So a snapshot keeps reading the values it
// was taken with, and the next GetTabs observes the new ones. Anything that
// wants to change one of those fields must go through replaceTabFieldLocked;
// assigning to a handed-out tab's Name is a data race, not a stale read (#1930).
//
// This does NOT extend to the whole struct: tmux and Conversation are still
// assigned IN PLACE under i.mu (setTmuxLocked, setupTabs' dead-shell
// replacement, teardown's ref clearing). The package does read those off a
// snapshot in places — setupTabs and teardownTabs both capture tabs under the
// lock and work outside it — and that is safe only because the daemon's
// per-instance op-lock serializes start/teardown against every other mutation.
// That discipline lives in the daemon, not in this type: prefer the locking
// accessors (ToInstanceData), and if you read tmux/Conversation off
// a snapshot, know that is what you are leaning on.
func (i *Instance) GetTabs() []*Tab {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]*Tab, len(i.Tabs))
	copy(out, i.Tabs)
	return out
}

// tabTmuxAtLocked returns the tmux session of the tab at idx, or nil when idx
// is out of range or the tab has no session. Callers must hold i.mu.
func (i *Instance) tabTmuxAtLocked(idx int) *tmux.TmuxSession {
	if idx < 0 || idx >= len(i.Tabs) {
		return nil
	}
	return i.Tabs[idx].tmux
}
