package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// ----------------------------------------------------------------------------
// #7700 — every rail key the cursor row refuses owes the short status notice
// the TUI already uses for refusals (the #5373 `a`-on-a-resting-row
// convention): a key that silently does nothing is a missing error message,
// not a new feature. Each branch below returned a bare `m, nil` before the
// fix — the press arrived and left no evidence — so these tests fail on the
// pre-fix tree while asserting no overlay opened and nothing was dispatched.
//
// The two *PaintsOnScreen tests are different on purpose: they are the issue's
// own literal repros (w on the Agent tab row, R on a shell tab row) driven
// through the real Update key path and asserted ON THE RENDERED 80x24 frame —
// and they already pass on the pre-fix tree, because those two notices fired
// before this fix ever landed. They exist to pin the whole render end of the
// contract — handler → errBox → status bar → screen — so a notice can never
// again silently stop painting (the alternative the play-test could not rule
// out), and to prove the resolved path cannot be the silent one.
// ----------------------------------------------------------------------------

// TestRailVerbsEmptyRail_AnswerWithNotice: with no sessions at all, every
// row-scoped key still answers. The cursor rests on the Instances header, so
// each verb resolves no row — "No session under the cursor" — except the two
// pane-scoped keys, which name their own missing target.
func TestRailVerbsEmptyRail_AnswerWithNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.KeyMsg
		want string
	}{
		{"t new tab", runeKey('t'), "No session under the cursor"},
		{"w close tab", runeKey('w'), "No session under the cursor"},
		{"R rename tab", runeKey('R'), "No session under the cursor"},
		{"g jump prompt", runeKey('g'), "No session under the cursor"},
		{"< move tab left", runeKey('<'), "No session under the cursor"},
		{"> move tab right", runeKey('>'), "No session under the cursor"},
		{"1 tab jump", runeKey('1'), "No session under the cursor"},
		{"s open pane", runeKey('s'), "No session under the cursor"},
		{"o attach", runeKey('o'), "No session under the cursor"},
		{"c limit retry", runeKey('c'), "No session under the cursor"},
		{"F handoff", runeKey('F'), "No session under the cursor"},
		{"x hide pane", runeKey('x'), "x hides a focused pane"},
		{"ctrl+u scroll up", tea.KeyMsg{Type: tea.KeyCtrlU}, "nothing to scroll"},
		{"ctrl+d scroll down", tea.KeyMsg{Type: tea.KeyCtrlD}, "nothing to scroll"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.sidebar.SetSize(40, 24)

			h.Update(tc.msg)

			require.Equal(t, stateDefault, h.state, "the refusal must not open any overlay")
			require.Nil(t, h.confirmationOverlay)
			require.Contains(t, h.errBox.FullError(), tc.want,
				"the press must be answered by naming the refusal, not swallowed (#7700)")
		})
	}
}

// TestRailVerbsRestingRowWithoutBinding_AnswerWithNotice: the cursor sits on a
// resting row while the store's display binding is nil — the shape an
// archived-only rail produces, since the cursor-to-binding push skips
// archived rows (#1028). The roster verbs read the store binding and used to
// die on it silently; they must now name the row the cursor is actually on
// and its way back.
func TestRailVerbsRestingRowWithoutBinding_AnswerWithNotice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status session.Status
		msg    tea.KeyMsg
		want   string
	}{
		{"w on archived", session.Archived, runeKey('w'), `archived session "worker"`},
		{"R on archived", session.Archived, runeKey('R'), `archived session "worker"`},
		{"g on archived", session.Archived, runeKey('g'), `archived session "worker"`},
		{"< on archived", session.Archived, runeKey('<'), `archived session "worker"`},
		{"> on archived", session.Archived, runeKey('>'), `archived session "worker"`},
		{"1 on archived", session.Archived, runeKey('1'), `archived session "worker"`},
		{"s on archived", session.Archived, runeKey('s'), `archived session "worker"`},
		{"w on lost", session.Lost, runeKey('w'), `session "worker" while it is not running`},
		{"s on lost", session.Lost, runeKey('s'), `session "worker" while it is not running`},
		{"w on dead", session.Dead, runeKey('w'), `session "worker" while it is not running`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := archiveActionInstance(t, "worker", tc.status)
			h.store.AddInstance(inst)
			h.sidebar.SetSize(40, 24)
			// The cursor lands on the resting row but the store binding stays
			// nil — pushSelection deliberately never commits an archived row.
			h.sidebar.SetSelectedInstance(0)
			require.Same(t, inst, h.sidebar.GetSelectedInstance(),
				"precondition: the cursor rests on the resting row")
			require.Nil(t, h.store.GetSelectedInstance(),
				"precondition: the store binding is empty")

			h.Update(tc.msg)

			require.Equal(t, stateDefault, h.state, "the refusal must not open any overlay")
			require.Contains(t, h.errBox.FullError(), tc.want,
				"the refusal must name the resting row, not the missing binding (#7700)")
			require.Contains(t, h.errBox.FullError(), "restore it first",
				"the refusal must name the way back")
		})
	}
}

// TestOpenPaneArchivedBinding_AnswersWithNotice: `s` resolves the store's
// sticky display binding, not the cursor — an archived binding used to open a
// pane that pruneDeadPanes reaped in the same update, leaving a press with no
// answer. The refusal now names the row's state and its way back.
func TestOpenPaneArchivedBinding_AnswersWithNotice(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Archived)
	selectInstance(h, inst)
	h.sidebar.SetSize(40, 24)

	h.Update(runeKey('s'))

	require.Equal(t, stateDefault, h.state, "the refusal must not open any overlay")
	require.Empty(t, h.store.OpenPanes(), "no pane may be left half-open on an archived binding")
	require.Contains(t, h.errBox.FullError(), `cannot open a pane on archived session "worker"`,
		"the dead key must name the row's state (#7700)")
	require.Contains(t, h.errBox.FullError(), "restore it first")
}

// TestRailVerbsInFlightRow_AnswerWithNotice: a row fenced by an in-flight op —
// the state the snapshot reconcile can adopt between the painted frame and
// the press — used to swallow the tab, pane, attach, and handoff verbs with a
// bare `m, nil`. Each must now name the op fencing it, the same answer the
// lifecycle verbs already gave for the same state (#7551 convention).
func TestRailVerbsInFlightRow_AnswerWithNotice(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   session.InFlightOp
		msg  tea.KeyMsg
		want string
	}{
		{"t on creating", session.OpCreating, runeKey('t'), "still being created"},
		{"s on creating", session.OpCreating, runeKey('s'), "still being created"},
		{"o on creating", session.OpCreating, runeKey('o'), "still being created"},
		{"enter on creating", session.OpCreating, tea.KeyMsg{Type: tea.KeyEnter}, "still being created"},
		{"F on creating", session.OpCreating, runeKey('F'), "still being created"},
		{"t on killing", session.OpKilling, runeKey('t'), "being deleted"},
		{"s on killing", session.OpKilling, runeKey('s'), "being deleted"},
		{"t on restoring", session.OpRestoring, runeKey('t'), "being restored"},
		{"s on restoring", session.OpRestoring, runeKey('s'), "being restored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := archiveActionInstance(t, "worker", session.Ready)
			inst.SetInFlightOpForTest(tc.op)
			selectInstance(h, inst)
			h.sidebar.SetSize(40, 24)

			h.Update(tc.msg)

			require.Equal(t, stateDefault, h.state, "a fenced row must not open any overlay")
			require.Nil(t, h.confirmationOverlay)
			require.Contains(t, h.errBox.FullError(), tc.want,
				"the verb must be answered by naming the op fencing it (#7700)")
		})
	}
}

// TestTabJumpOutOfRange_AnswersWithNotice: a number key past the roster's end
// was "a no-op (#930 PR 4)" — indistinguishable from a dead key. With tree
// focus the jump names the session's actual roster; with a pane focused the
// same miss names the pane's session.
func TestTabJumpOutOfRange_AnswersWithNotice(t *testing.T) {
	t.Run("tree focus", func(t *testing.T) {
		h := newTestHome(t)
		inst := archiveActionInstance(t, "worker", session.Ready)
		selectInstance(h, inst)
		h.sidebar.SetSize(40, 24)

		h.Update(runeKey('5'))

		require.Contains(t, h.errBox.FullError(), `session "worker" has no tab 5`,
			"an out-of-range jump must say what the session actually has (#7700)")
	})

	t.Run("pane focus", func(t *testing.T) {
		h := newTestHome(t)
		inst := archiveActionInstance(t, "worker", session.Ready)
		selectInstance(h, inst)
		resizeHome(h, 80, 24)
		p := openTestPane(t, h, inst, 0)
		require.NotNil(t, h.focusedOpenPane(),
			"precondition: the pane owns the focus ring")

		h.Update(runeKey('5'))

		require.Contains(t, h.errBox.FullError(), `session "worker" has no tab 5`,
			"a pane-focused out-of-range jump must name the pane's session (#7700)")
		require.Equal(t, p, h.focusedOpenPane(),
			"the refused jump must not retarget the pane")
	})
}

// TestAttachExpandedArchivedHeader_AnswersWithRowFence: `o` and `F` on the
// expanded Archived header resolve no cursor row, but the ▾-marked resting
// row underneath still owns its fences — the press must hear that row's own
// answer (the same interactiveGuard it gives on the row itself), not silence.
func TestAttachExpandedArchivedHeader_AnswersWithRowFence(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"o attach", runeKey('o')},
		{"F handoff", runeKey('F')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := archiveActionInstance(t, "worker", session.Archived)
			h.store.AddInstance(inst)
			h.sidebar.SetSize(40, 24)
			archivedHeaderState(t, h, inst)

			h.Update(tc.msg)

			require.Equal(t, stateDefault, h.state, "the refusal must not open any overlay")
			require.Contains(t, h.errBox.FullError(), "is archived",
				"the press must hear the marked row's own fence (#7700)")
			require.Contains(t, h.errBox.FullError(), "restore",
				"the fence must name the way back")
		})
	}
}

// TestRowVerbsCollapsedHeader_AnswerNoSession: a collapsed folder hides its
// bound row from the verb-resolution rules — the ▾ marker only exists on
// visible rows — so `o`, `c`, and `F` answer the bare absence, matching the
// "No archived session under the cursor" `r` already gives there.
func TestRowVerbsCollapsedHeader_AnswerNoSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"o attach", runeKey('o')},
		{"c limit retry", runeKey('c')},
		{"F handoff", runeKey('F')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			inst := archiveActionInstance(t, "worker", session.Archived)
			h.store.AddInstance(inst)
			h.sidebar.SetSize(40, 24)
			h.store.SelectInstance(inst)
			h.sidebar.SetSelectedInstance(0)
			h.sidebar.CollapseSection()

			sel := h.sidebar.GetSelection()
			require.True(t, sel.IsHeader, "precondition: the cursor rests on the collapsed Archived header")
			require.Equal(t, ui.SectionArchived, sel.Kind)

			h.Update(tc.msg)

			require.Equal(t, stateDefault, h.state, "the refusal must not open any overlay")
			require.Contains(t, h.errBox.FullError(), "No session under the cursor",
				"a hidden binding resolves nothing, so the press names the absence (#7700)")
		})
	}
}

// issue7700LiveTabRowHome builds the issue's exact repro scene: a live
// session with an Agent tab at slot 0 and a shell tab at slot 1, the store
// bound to it (what makes its tab rows render — instanceExpanded's SameRow
// gate means a live tab row implies a bound store, so inst==nil cannot
// mislabel this row), and the rail cursor parked on the requested tab row —
// the shape af_select leaves behind.
func issue7700LiveTabRowHome(t *testing.T, tabIdx int) (*home, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "worker")
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddTabForTest("shell", session.TabKindShell)
	h.store.AddInstance(inst)
	h.store.SetSelectedInstance(inst)
	h.sidebar.SelectTabRow("worker", tabIdx)

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab, "precondition: the rail cursor rests on a tab row")
	require.Equal(t, tabIdx, sel.TabIndex)
	require.Same(t, inst, h.store.GetSelectedInstance(),
		"precondition: a live tab row implies the store's bound selection")

	resizeHome(h, 80, 24)
	return h, inst
}

// TestAgentTabCloseRefusalPaintsOnScreen is repro 1 of #7700 end to end: `w`
// with the rail cursor on the Agent tab row answers on the rendered frame,
// not just inside the errBox. This passes on the pre-fix tree — the notice
// fired before this fix — so it pins the render end of the contract rather
// than covering new code: a refusal that stops reaching the screen now fails
// this test no matter what the handler still sets.
func TestAgentTabCloseRefusalPaintsOnScreen(t *testing.T) {
	h, _ := issue7700LiveTabRowHome(t, 0)

	h.Update(runeKey('w'))

	require.Contains(t, xansi.Strip(h.View()),
		"the agent tab can't be closed; use D to kill the session",
		"the issue's own repro must show the refusal on the 80x24 frame")
}

// TestShellTabRenameRefusalPaintsOnScreen is repro 2 of #7700 end to end: `R`
// with the rail cursor on a shell tab row answers on the rendered frame.
// Same standing as the `w` case: passes on the pre-fix tree, here to keep the
// handler → status-bar → screen chain provably live.
func TestShellTabRenameRefusalPaintsOnScreen(t *testing.T) {
	h, _ := issue7700LiveTabRowHome(t, 1)

	h.Update(runeKey('R'))

	require.Contains(t, xansi.Strip(h.View()),
		"Agent and Terminal tabs can't be renamed",
		"the issue's own repro must show the refusal on the 80x24 frame")
}
