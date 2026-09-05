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
