package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

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

// TestMenuArchiveRestoreActionByRowState pins WHICH mgmt verb the footer offers
// per row state (#1605): a live row offers KeyArchive, a resting row offers
// KeyRestore, and a creating row offers neither. The two are mutually exclusive —
// exactly one is ever present.
func TestMenuArchiveRestoreActionByRowState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      session.Status
		wantArchive bool
		wantRestore bool
	}{
		{name: "running archives", status: session.Running, wantArchive: true},
		{name: "ready archives", status: session.Ready, wantArchive: true},
		{name: "lost restores", status: session.Lost, wantRestore: true},
		{name: "dead restores", status: session.Dead, wantRestore: true},
		{name: "archived restores", status: session.Archived, wantRestore: true},
		{name: "creating omits both", status: session.Loading, wantArchive: false, wantRestore: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &session.Instance{}
			inst.SetStatusForTest(tc.status)
			m := NewMenu()
			m.SetInstance(inst)

			if got := menuHasOption(m, keys.KeyArchive); got != tc.wantArchive {
				t.Fatalf("KeyArchive present = %v, want %v", got, tc.wantArchive)
			}
			if got := menuHasOption(m, keys.KeyRestore); got != tc.wantRestore {
				t.Fatalf("KeyRestore present = %v, want %v", got, tc.wantRestore)
			}
		})
	}
}

// TestMenuArchiveRestoreHintByRowState verifies the mgmt-group footer hint
// (#1605): a live row advertises `a archive`; a resting (archived/lost/dead) row
// advertises the dedicated `r restore` key instead. The two verbs live on
// separate keys — the footer shows exactly the one the selected row supports.
func TestMenuArchiveRestoreHintByRowState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status session.Status
		want   string
		reject string
	}{
		{name: "running shows archive", status: session.Running, want: "a archive", reject: "r restore"},
		{name: "ready shows archive", status: session.Ready, want: "a archive", reject: "r restore"},
		{name: "lost shows restore", status: session.Lost, want: "r restore", reject: "a archive"},
		{name: "dead shows restore", status: session.Dead, want: "r restore", reject: "a archive"},
		{name: "archived shows restore", status: session.Archived, want: "r restore", reject: "a archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &session.Instance{}
			inst.SetStatusForTest(tc.status)
			m := NewMenu()
			m.SetInstance(inst)
			m.SetSize(120, 1)

			out := m.String()
			if !strings.Contains(out, tc.want) {
				t.Fatalf("footer missing %q for %s row:\n%s", tc.want, tc.name, out)
			}
			if strings.Contains(out, tc.reject) {
				t.Fatalf("footer unexpectedly shows %q for %s row:\n%s", tc.reject, tc.name, out)
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

// TestMenuNewInstanceDropsPromptHintBeforeTheWayOut is the narrow-terminal
// guard for #1936. Adding a fourth hint took the naming row from ~62 to ~78
// cells, so it now overflows bars it used to fit — and the clamp cuts the RIGHT
// edge, i.e. `esc cancel`, the only advertised way out of the form (the #1083
// class). The prompt hint must be shed first instead: it advertises an optional
// field, while submit/change-program/cancel are the form's load-bearing verbs.
func TestMenuNewInstanceDropsPromptHintBeforeTheWayOut(t *testing.T) {
	// 52..78 is the band this change created: wide enough for the three
	// load-bearing verbs (51 cells) but not for the fourth hint. Below 52 even
	// the shed row overflows and the status bar's clamp takes over — that is
	// pre-existing behavior the drop list has never been able to fix.
	for _, width := range []int{78, 70, 60, 55} {
		m := NewMenu()
		m.SetState(StateNewInstance)
		m.SetNamingHasPrompt(true)
		m.SetSize(width, 1)
		out := m.String()

		for _, want := range []string{"enter submit name", "tab change program", "esc cancel"} {
			if !strings.Contains(out, want) {
				t.Fatalf("width %d dropped the load-bearing hint %q:\n%s", width, want, out)
			}
		}
		if strings.Contains(out, "initial prompt") {
			t.Fatalf("width %d kept the optional prompt hint in an overflowing row:\n%s", width, out)
		}
		for i, line := range strings.Split(out, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d: hint row line %d is %d cells wide:\n%s", width, i, got, out)
			}
		}
	}

	// At the standard 80-column terminal everything still fits — the drop is a
	// degradation, not the default.
	m := NewMenu()
	m.SetState(StateNewInstance)
	m.SetSize(80, 1)
	if out := m.String(); !strings.Contains(out, "initial prompt") {
		t.Fatalf("an 80-column bar must still advertise the prompt field:\n%s", out)
	}
}

// TestMenuNewInstancePromptHintSwapsAndDoesNotAlias covers the #1936 hint: the
// naming footer advertises the initial-prompt field, and marks it once the
// field holds text. The aliasing half is the part worth pinning — the swap
// builds a copy, because writing the "✓" variant into the shared
// newInstanceMenuOptions slice would leak it into every later naming form,
// including ones with no prompt at all.
func TestMenuNewInstancePromptHintSwapsAndDoesNotAlias(t *testing.T) {
	m := NewMenu()
	m.SetState(StateNewInstance)
	m.SetSize(120, 1)

	if out := m.String(); !strings.Contains(out, "initial prompt") {
		t.Fatalf("new-instance footer missing the initial-prompt hint:\n%s", out)
	}

	m.SetNamingHasPrompt(true)
	if out := m.String(); !strings.Contains(out, "initial prompt ✓") {
		t.Fatalf("footer must mark an attached prompt:\n%s", out)
	}

	// A fresh menu must be back to the unmarked hint: if the swap had mutated
	// the package-level slice in place, this would still read "✓".
	fresh := NewMenu()
	fresh.SetState(StateNewInstance)
	fresh.SetSize(120, 1)
	if out := fresh.String(); strings.Contains(out, "initial prompt ✓") {
		t.Fatalf("the prompt-hint swap leaked into the shared options slice:\n%s", out)
	}
}

// (removed) TestMenuRemoteInstanceOmitsUnsupportedTabKeys — obsolete after
// #1592 Phase 4 PR7: the remote HookBackend now reports full parity
// (TabManagement=true), so a remote instance surfaces the t/w tab keys exactly
// like a local one. The "remote omits unsupported tab keys" behavior it guarded
// no longer exists.

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

// TestMenuProjectsFocusFooterIsHonest pins the #1620 fix: with the bottom
// Projects section focused, the footer advertises exactly the section's live
// affordances — Enter switch and `/` search — plus the cross-region
// focus/help/quit. It must NOT fall through to the instance verbs (n new,
// D kill, s open pane), which are all no-ops there now, so the footer never
// advertises a key that does nothing.
func TestMenuProjectsFocusFooterIsHonest(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())
	m.SetFocusRegion(layout.RegionProjects)

	has := func(want keys.KeyName) bool {
		for _, k := range m.options {
			if k == want {
				return true
			}
		}
		return false
	}

	if !has(keys.KeySwitchProjectRow) || !has(keys.KeySearch) {
		t.Errorf("Projects focus must advertise switch + search; options=%v", m.options)
	}
	if !has(keys.KeyTab) || !has(keys.KeyHelp) || !has(keys.KeyQuit) {
		t.Errorf("Projects focus keeps the focus/help/quit chrome; options=%v", m.options)
	}
	for _, leak := range []keys.KeyName{keys.KeyNew, keys.KeyKill, keys.KeyOpenPane, keys.KeyNewTab} {
		if has(leak) {
			t.Errorf("Projects focus must not advertise instance verb %v (it is a no-op there); options=%v", leak, m.options)
		}
	}

	m.SetSize(120, 1)
	out := m.String()
	if !strings.Contains(out, "search") || !strings.Contains(out, "switch") {
		t.Fatalf("Projects footer must render switch + search:\n%s", out)
	}
	if strings.Contains(out, "new") {
		t.Fatalf("Projects footer must not render the instance 'new' verb:\n%s", out)
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
