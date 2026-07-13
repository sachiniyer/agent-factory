package ui

import (
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// previewFromInstance is the test-only PreviewSource that captures straight from
// the instance — exactly what the daemon Preview RPC does server-side — so the
// TabPane state-machine tests exercise the same content path they did before the
// daemon became the sole capturer (#1592 Phase 2 PR6). Production injects the
// daemon-backed source instead.
func previewFromInstance(instance *session.Instance, tab int, full bool) (string, error) {
	if tab == 0 {
		return instance.AgentServer().Preview(tab, full)
	}
	if full {
		return instance.PreviewTabFullHistory(tab)
	}
	return instance.PreviewTab(tab)
}

// newTestTabbedWindow builds an unbound TabbedWindow; tests bind an instance
// via setWindowInstance.
func newTestTabbedWindow() *TabbedWindow {
	return NewTabbedWindow(NewTabPane(previewFromInstance), nil)
}

// setWindowInstance is the test wiring for binding a window: give it an open
// pane bound to inst (preserving any current tab index) and re-clamp, exactly
// what opening a pane does in production. A nil inst unbinds the window.
func setWindowInstance(tw *TabbedWindow, inst *session.Instance) {
	if inst == nil {
		tw.pane = nil
		return
	}
	tab := 0
	if tw.pane != nil {
		tab = tw.pane.Tab()
	}
	proj := store.NewProjection()
	proj.AddInstance(inst)
	tw.pane = proj.AddOpenPane(inst, tab)
	tw.ClampActiveTab()
}

// addTestInstance adds inst to the sidebar's projection and syncs the row
// list eagerly, mirroring the pre-store Sidebar.AddInstance which rebuilt the
// visible items at mutation time (production syncs lazily on the next read).
func addTestInstance(s *Sidebar, inst *session.Instance) func() {
	finalize := s.proj.AddInstance(inst)
	s.syncFromStore()
	return finalize
}

// addAgentShellTabs stamps a tmux-less agent + shell tab pair on inst — the
// shape of a started instance after `t`. Since #1100 fresh instances carry
// only the agent tab and TabLabels mirrors the real tab list, so fixtures
// that exercise two tab slots must hold two real tabs.
func addAgentShellTabs(inst *session.Instance) {
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddTabForTest("shell", session.TabKindShell)
}
