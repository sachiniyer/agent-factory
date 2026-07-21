package app

import (
	"github.com/sachiniyer/agent-factory/terminal"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// scrollOwnerForKnownModes is the one authoritative terminal ownership decision
// used by nav preview and interactive mouse routing. Alternate-screen applications
// own their private history even when they do not track the mouse (Codex transcript);
// a primary-screen application that explicitly tracks mouse also owns wheel input.
func scrollOwnerForKnownModes(modes terminal.Modes) ui.ScrollOwner {
	if modes.AlternateScreen || modes.MouseTrackingEnabled() {
		return ui.ScrollOwnerChildApplication
	}
	return ui.ScrollOwnerHostHistory
}

func scrollOwnerForSnapshot(modes terminal.Modes, known bool) ui.ScrollOwner {
	if !known {
		return ui.ScrollOwnerNone
	}
	return scrollOwnerForKnownModes(modes)
}

// paneScrollOwnership is the input-time decision for one target. childMouse is
// derived from the same mode snapshot as owner so nav and interactive routing
// cannot disagree by consulting two independently updated flags.
type paneScrollOwnership struct {
	owner      ui.ScrollOwner
	childMouse bool
}

func (m *home) resolvePaneScrollOwnership(p *store.OpenPane, w *ui.TabbedWindow) paneScrollOwnership {
	if p == nil || w == nil {
		return paneScrollOwnership{}
	}
	if m.paneIsPreviewing(p) {
		// SetPreview starts at None; the target's detached PreviewSnapshot replaces
		// it when content and terminal modes land together. Never consult the
		// committed pane's live stream for a transient target.
		return paneScrollOwnership{owner: w.ScrollOwner()}
	}
	if lt := m.liveTerms[p.ID()]; lt != nil {
		modes, known := lt.TerminalModes()
		if !known {
			return paneScrollOwnership{owner: w.ObserveScrollOwnerUnknown()}
		}
		decision := paneScrollOwnership{
			owner:      scrollOwnerForKnownModes(modes),
			childMouse: modes.MouseTrackingEnabled(),
		}
		w.SetScrollOwner(decision.owner)
		return decision
	}
	// Capture-only tabs receive ownership from their latest PreviewSnapshot.
	// Unknown stays unknown rather than being inferred as host scrollback.
	return paneScrollOwnership{owner: w.ScrollOwner()}
}

// syncPaneScrollOwner resolves and installs the current owner for one pane.
// Applications can switch owners without changing process or pane identity, so
// callers use this at input time as well as on the preview tick.
func (m *home) syncPaneScrollOwner(p *store.OpenPane, w *ui.TabbedWindow) ui.ScrollOwner {
	return m.resolvePaneScrollOwnership(p, w).owner
}

func (m *home) syncPaneScrollOwners() {
	for _, p := range m.visiblePanes {
		if w := m.paneWindows[p.ID()]; w != nil {
			m.syncPaneScrollOwner(p, w)
		}
	}
	m.syncScrollHint()
}

// syncScrollHint keeps Ctrl-U/Ctrl-D honest. Child-owned or unknown previews
// cannot provide a non-mutating AF history view, so they do not advertise those
// keys. Mouse tracking is routed independently at input time.
func (m *home) syncScrollHint() {
	w, _ := m.focusedContentPane()
	m.menu.SetScrollAvailable(w != nil && w.ScrollOwner() == ui.ScrollOwnerHostHistory)
}
