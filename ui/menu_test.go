package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// readyUIInstance builds an actionable Ready-status instance for ui-package
// render tests. Real daemon-published sessions carry stable IDs; tests that need
// the legacy inert shape construct an id-less Instance explicitly (#2234).
// Liveness is unexported, so this goes through the SetStatus shim rather than a
// struct literal (#1195 Phase 1b).
func readyUIInstance() *session.Instance {
	i := &session.Instance{ID: "ui-test-session"}
	i.SetStatusForTest(session.Ready)
	return i
}

// TestMenuShowsBothScrollKeysWhenControllerProvidesHistory verifies that the
// controller's explicit history capability surfaces both scroll shortcuts.
// Menu never owned a tab binding; its old activeTab mirror was unread dead state
// that made the former agent/terminal variants of this test identical (#1991).
func TestMenuShowsBothScrollKeysWhenControllerProvidesHistory(t *testing.T) {
	m := NewMenu()
	m.SetScrollAvailable(true)
	// Use a non-loading instance so addInstanceOptions renders the full menu.
	m.SetInstance(readyUIInstance())

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

func TestMenuHidesScrollKeysWhenControllerCannotProvideHistory(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())
	m.SetScrollAvailable(false)

	assertAbsent := func() {
		t.Helper()
		for _, k := range m.options {
			if k == keys.KeyShiftUp || k == keys.KeyShiftDown {
				t.Fatalf("unavailable scroll key leaked into options: %v", m.options)
			}
		}
	}
	m.SetFocusRegion(layout.RegionTree)
	assertAbsent()
	m.SetFocusRegion(layout.PaneRegion(7))
	assertAbsent()

	m.SetScrollAvailable(true)
	var found int
	for _, k := range m.options {
		if k == keys.KeyShiftUp || k == keys.KeyShiftDown {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("restoring host ownership yielded %d scroll keys, want 2: %v", found, m.options)
	}
}

// TestMenuArchiveRestoreActionByRowState pins WHICH mgmt verb the footer offers
// per row state (#1605): a live row offers KeyArchive, a resting row offers
// KeyRestore, and a creating row offers neither. The two are mutually exclusive —
// exactly one is ever present.
func TestMenuArchiveRestoreActionByRowState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          string
		status      session.Status
		wantKill    bool
		wantArchive bool
		wantRestore bool
	}{
		{name: "running archives", id: "running-id", status: session.Running, wantKill: true, wantArchive: true},
		{name: "ready archives", id: "ready-id", status: session.Ready, wantKill: true, wantArchive: true},
		{name: "lost restores", id: "lost-id", status: session.Lost, wantKill: true, wantRestore: true},
		{name: "dead restores", id: "dead-id", status: session.Dead, wantKill: true, wantRestore: true},
		{name: "archived restores", id: "archived-id", status: session.Archived, wantKill: true, wantRestore: true},
		{name: "creating omits lifecycle actions", id: "pending-id", status: session.Loading},
		{name: "id-less omits lifecycle actions", status: session.Ready},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &session.Instance{ID: tc.id}
			inst.SetStatusForTest(tc.status)
			m := NewMenu()
			m.SetInstance(inst)

			if got := menuHasOption(m, keys.KeyKill); got != tc.wantKill {
				t.Fatalf("KeyKill present = %v, want %v", got, tc.wantKill)
			}
			if got := menuHasOption(m, keys.KeyArchive); got != tc.wantArchive {
				t.Fatalf("KeyArchive present = %v, want %v", got, tc.wantArchive)
			}
			if got := menuHasOption(m, keys.KeyRestore); got != tc.wantRestore {
				t.Fatalf("KeyRestore present = %v, want %v", got, tc.wantRestore)
			}
		})
	}
}

// A create whose launch crossed the uncertainty boundary is deliberately inert:
// af cannot safely attach, archive, or restore it because the runtime identity was
// never confirmed. Its stable record is still the only cleanup handle the user
// has, though, so the footer must keep Kill available as a separate capability.
func TestMenuStartupUnknownOffersKillWithoutRuntimeActions(t *testing.T) {
	inst := readyUIInstance()
	inst.MarkStartupStateUnknown()
	m := NewMenu()
	m.SetInstance(inst)

	if !menuHasOption(m, keys.KeyKill) {
		t.Fatal("startup-unknown row has no Kill action")
	}
	for _, forbidden := range []keys.KeyName{
		keys.KeyArchive, keys.KeyRestore, keys.KeyEnter, keys.KeyAttach,
		keys.KeyNewTab, keys.KeyCloseTab, keys.KeyOpenPane,
	} {
		if menuHasOption(m, forbidden) {
			t.Fatalf("startup-unknown row exposes unsafe action %q", forbidden)
		}
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
			inst := &session.Instance{ID: "ui-test-session"}
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

// #2399: the hint row joins fragments on one line, so it takes the repo-wide
// " · " separator (CLAUDE.md copy conventions) rather than the " • " it used to
// render. Asserted on the real rendered row, not on the package var, so a future
// renderer that stops using `separator` cannot quietly reintroduce a bullet.
//
// The bullet check is scoped to what the menu itself emits: a hint's own DESC
// could legitimately contain one, and this row is the only thing under test.
func TestMenuJoinsHintsWithMiddleDot(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())
	m.SetSize(100, 1)

	out := m.String()
	if !strings.Contains(out, " · ") {
		t.Fatalf("hint row must join fragments with the middle dot separator:\n%s", out)
	}
	if strings.Contains(out, "•") {
		t.Fatalf("hint row still renders a bullet separator (#2399):\n%s", out)
	}
}

// The hint row is shed by WIDTH (hintDropOrder), so the separator's cell cost is
// load-bearing: one cell wider and some terminal size silently loses a hint that
// used to fit. U+00B7 and U+2022 are both East Asian Ambiguous and measure one
// cell, so the swap is free — but "measures the same" is exactly the kind of
// assumption that is true until a width table changes, so it is pinned here
// rather than reasoned about in a comment alone.
func TestMenuSeparatorIsWidthNeutral(t *testing.T) {
	if got := lipgloss.Width(separator); got != 3 {
		t.Fatalf("separator measures %d cells, want 3 — a wider separator sheds hints "+
			"at widths that used to fit (#2399/#1083)", got)
	}
	if got, want := lipgloss.Width(separator), lipgloss.Width(" • "); got != want {
		t.Fatalf("separator is %d cells but the bullet it replaced was %d; the drop order "+
			"was tuned against the old width", got, want)
	}
}

// The narrowest supported terminal is where a width regression would show first,
// so the row is checked there too: it must still fit its bar, and must not have
// been pushed onto the bullet-free path by simply dropping every hint.
func TestMenuMiddleDotFitsAtEightyColumns(t *testing.T) {
	m := NewMenu()
	m.SetInstance(readyUIInstance())
	m.SetSize(80, 1)

	out := m.String()
	if !strings.Contains(out, " · ") {
		t.Fatalf("80-column hint row must still join fragments with the middle dot:\n%s", out)
	}
	if strings.Contains(out, "•") {
		t.Fatalf("80-column hint row still renders a bullet separator (#2399):\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("hint row overflows an 80-column bar at %d cells:\n%s", w, out)
		}
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
