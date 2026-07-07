package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// readyUIInstance builds a Ready-status instance for ui-package render tests.
// liveness is unexported, so it goes through the SetStatus shim rather than a
// struct literal (#1195 Phase 1b).
func readyUIInstance() *session.Instance {
	i := &session.Instance{}
	i.SetStatus(session.Ready)
	return i
}

// TestMenuTerminalTabShowsBothScrollKeys verifies that when the terminal tab
// is active, the instance menu surfaces both scroll shortcuts. Regression test
// for issue #270.
func TestMenuTerminalTabShowsBothScrollKeys(t *testing.T) {
	m := NewMenu()
	// Use a non-loading instance so addInstanceOptions renders the full menu.
	m.SetInstance(readyUIInstance())
	m.SetActiveTab(1)

	var gotShiftUp, gotShiftDown int
	for _, k := range m.options {
		switch k {
		case keys.KeyShiftUp:
			gotShiftUp++
		case keys.KeyShiftDown:
			gotShiftDown++
		}
	}

	if gotShiftUp != 1 {
		t.Errorf("expected exactly 1 KeyShiftUp in terminal tab menu, got %d", gotShiftUp)
	}
	if gotShiftDown != 1 {
		t.Errorf("expected exactly 1 KeyShiftDown in terminal tab menu, got %d", gotShiftDown)
	}
}

// TestMenuAgentTabShowsBothScrollKeys verifies that the Agent tab also
// surfaces both scroll shortcuts — the agent output supports scrolling identically to
// terminal, and the help screen documents the shortcuts for both. Regression
// test for issue #467.
func TestMenuAgentTabShowsBothScrollKeys(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())
	m.SetActiveTab(0)

	var gotShiftUp, gotShiftDown int
	for _, k := range m.options {
		switch k {
		case keys.KeyShiftUp:
			gotShiftUp++
		case keys.KeyShiftDown:
			gotShiftDown++
		}
	}

	if gotShiftUp != 1 {
		t.Errorf("expected exactly 1 KeyShiftUp in agent tab menu, got %d", gotShiftUp)
	}
	if gotShiftDown != 1 {
		t.Errorf("expected exactly 1 KeyShiftDown in agent tab menu, got %d", gotShiftDown)
	}
}

func TestMenuRestorableRowsShowArchiveRestoreAction(t *testing.T) {
	for _, status := range []session.Status{session.Lost, session.Dead, session.Archived} {
		inst := &session.Instance{}
		inst.SetStatus(status)
		m := NewMenu()
		m.SetInstance(inst)

		var found bool
		for _, k := range m.options {
			if k == keys.KeyArchive {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("status %v: restore action not shown in menu options", status)
		}
	}
}

// TestMenuRemoteInstanceOmitsUnsupportedTabKeys guards against regressing #988:
// remote instances block `t` (new tab) and `w` (close tab) — those handlers
// reject IsRemote() with an error — so the footer menu must only surface the
// tab keys that actually work (cycle / 1-9 jump), while local instances keep
// the full set.
func TestMenuRemoteInstanceOmitsUnsupportedTabKeys(t *testing.T) {
	remote := readyUIInstance()
	remote.SetBackend(&session.HookBackend{})
	if !remote.IsRemote() {
		t.Fatal("sanity: instance should report as remote")
	}

	m := NewMenu()
	m.SetInstance(remote)

	var gotNewTab, gotCloseTab, gotTab, gotJump int
	for _, k := range m.options {
		switch k {
		case keys.KeyNewTab:
			gotNewTab++
		case keys.KeyCloseTab:
			gotCloseTab++
		case keys.KeyTab:
			gotTab++
		case keys.KeyJumpTab:
			gotJump++
		}
	}
	if gotNewTab != 0 || gotCloseTab != 0 {
		t.Errorf("remote menu must not surface t/w tab keys; got newTab=%d closeTab=%d", gotNewTab, gotCloseTab)
	}
	if gotTab != 1 || gotJump != 1 {
		t.Errorf("remote menu should still surface tab cycle and 1-9 jump; got tab=%d jump=%d", gotTab, gotJump)
	}

	local := readyUIInstance()
	if local.IsRemote() {
		t.Fatal("sanity: instance should report as local")
	}
	m.SetInstance(local)
	var localNewTab, localCloseTab int
	for _, k := range m.options {
		switch k {
		case keys.KeyNewTab:
			localNewTab++
		case keys.KeyCloseTab:
			localCloseTab++
		}
	}
	if localNewTab != 1 || localCloseTab != 1 {
		t.Errorf("local menu should surface t/w tab keys; got newTab=%d closeTab=%d", localNewTab, localCloseTab)
	}
}

// TestMenuPaneHintsFollowFocus pins the #1088 hint model: tree focus
// advertises the open-pane verb with the instance actions; a focused
// workspace pane gets its own option set — attach/scroll on its binding,
// open-pane, hide-pane.
func TestMenuPaneHintsFollowFocus(t *testing.T) {
	local := readyUIInstance()
	m := NewMenu()
	m.SetInstance(local)

	has := func(want keys.KeyName) bool {
		for _, k := range m.options {
			if k == want {
				return true
			}
		}
		return false
	}

	m.SetFocusRegion(layout.RegionTree)
	if !has(keys.KeyOpenPane) || !has(keys.KeySplitPane) || has(keys.KeyHidePane) {
		t.Errorf("tree focus must advertise open/split pane, not hide; options=%v", m.options)
	}
	if !has(keys.KeyNewTab) || !has(keys.KeyKill) {
		t.Errorf("tree focus keeps the instance actions; options=%v", m.options)
	}

	m.SetFocusRegion(layout.PaneRegion(7))
	if !has(keys.KeyOpenPane) || !has(keys.KeySplitPane) || !has(keys.KeyHidePane) || !has(keys.KeyEnter) {
		t.Errorf("a focused pane must advertise open/split/hide pane + attach; options=%v", m.options)
	}
	if has(keys.KeyNewTab) {
		t.Errorf("pane focus swaps to the pane option set; options=%v", m.options)
	}
}
