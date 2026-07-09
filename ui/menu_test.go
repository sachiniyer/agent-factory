package ui

import (
	"strings"
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
	i.SetStatusForTest(session.Ready)
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

func TestMenuArchiveRestoreActionByRowState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     session.Status
		wantAction bool
	}{
		{name: "running archives", status: session.Running, wantAction: true},
		{name: "ready archives", status: session.Ready, wantAction: true},
		{name: "lost restores", status: session.Lost, wantAction: true},
		{name: "dead restores", status: session.Dead, wantAction: true},
		{name: "archived restores", status: session.Archived, wantAction: true},
		{name: "creating omits archive restore", status: session.Loading, wantAction: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &session.Instance{}
			inst.SetStatusForTest(tc.status)
			m := NewMenu()
			m.SetInstance(inst)

			if got := menuHasOption(m, keys.KeyArchive); got != tc.wantAction {
				t.Fatalf("KeyArchive present = %v, want %v", got, tc.wantAction)
			}
		})
	}
}

func menuHasOption(m *Menu, want keys.KeyName) bool {
	for _, k := range m.options {
		if k == want {
			return true
		}
	}
	return false
}

func TestMenuNewInstanceShowsSubmitProgramAndCancel(t *testing.T) {
	m := NewMenu()
	m.SetState(StateNewInstance)
	m.SetSize(80, 1)

	out := m.String()
	for _, want := range []string{"enter submit name", "tab change program", "esc cancel"} {
		if !strings.Contains(out, want) {
			t.Fatalf("new-instance footer missing %q:\n%s", want, out)
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

func TestMenuNormalWidthSurfacesTabAndPaneManagement(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())
	m.SetSize(100, 1)

	out := m.String()
	for _, want := range []string{
		"D kill",
		"t new tab",
		"w close tab",
		"1-9 jump",
		"s open pane",
		"tab focus",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("normal-width footer missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "S split pane") {
		t.Fatalf("normal-width footer must not advertise split without a preview:\n%s", out)
	}

	m.SetSplitPaneAvailable(true)
	if out = m.String(); !strings.Contains(out, "S split pane") {
		t.Fatalf("normal-width footer missing split hint while preview is active:\n%s", out)
	}
}

func TestMenuSelftestWidthKeepsSelectedRowAndTabPaneHints(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())
	m.SetSize(100, 1)

	out := m.String()
	for _, want := range []string{
		"n new",
		"D kill",
		"t new tab",
		"w close tab",
		"1-9 jump",
		"s open pane",
		"tab focus",
		"? help",
		"q quit",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("selftest-width footer missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "↵ interact") {
		t.Fatalf("selftest-width footer should drop interact before tree-focus/selected-row/tab/pane hints:\n%s", out)
	}
}

// TestMenuPaneHintsFollowFocus pins the #1088/#1419 hint model: tree focus
// advertises the open-pane verb with the instance actions; split-pane is
// advertised only while a preview exists to commit; a focused
// workspace pane gets its own option set — attach/scroll on its binding,
// pane switching, open-pane, hide-pane.
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
	if !has(keys.KeyOpenPane) || has(keys.KeySplitPane) || has(keys.KeyHidePane) {
		t.Errorf("tree focus must advertise open pane, not split/hide without a preview; options=%v", m.options)
	}
	if !has(keys.KeyNewTab) || !has(keys.KeyKill) {
		t.Errorf("tree focus keeps the instance actions; options=%v", m.options)
	}
	m.SetSplitPaneAvailable(true)
	if !has(keys.KeySplitPane) {
		t.Errorf("tree focus must advertise split pane while a preview can be committed; options=%v", m.options)
	}
	m.SetSplitPaneAvailable(false)

	m.SetFocusRegion(layout.PaneRegion(7))
	if !has(keys.KeyOpenPane) || has(keys.KeySplitPane) || !has(keys.KeyHidePane) ||
		!has(keys.KeyPanePrev) || !has(keys.KeyPaneNext) || !has(keys.KeyEnter) {
		t.Errorf("a focused pane must advertise pane focus/open/hide + attach, not split without a preview; options=%v", m.options)
	}
	if has(keys.KeyNewTab) {
		t.Errorf("pane focus swaps to the pane option set; options=%v", m.options)
	}
	m.SetSplitPaneAvailable(true)
	if !has(keys.KeySplitPane) {
		t.Errorf("a focused pane must advertise split pane while a preview can be committed; options=%v", m.options)
	}
}

func TestMenuNarrowPaneFocusDoesNotShowHideWithoutOpen(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())
	m.SetFocusRegion(layout.PaneRegion(7))
	m.SetSize(80, 1)

	out := m.String()
	if strings.Contains(out, "hide pane") && !strings.Contains(out, "open pane") {
		t.Fatalf("narrow pane footer must not show hide without the recovery/open action:\n%s", out)
	}
}
