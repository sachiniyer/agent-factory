package ui

import (
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// newTestTabbedWindow builds a TabbedWindow over a fresh projection. The
// projection is reachable via tw.proj for tests that need to seed data.
func newTestTabbedWindow() *TabbedWindow {
	return NewTabbedWindow(NewTabPane(), store.NewProjection())
}

// setWindowInstance is the test wiring for the pre-store TabbedWindow.SetInstance:
// bind the projection's display selection to inst and re-clamp the active tab,
// exactly what selectionChanged does in production.
func setWindowInstance(tw *TabbedWindow, inst *session.Instance) {
	tw.proj.SetSelectedInstance(inst)
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
