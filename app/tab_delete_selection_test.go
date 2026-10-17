package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// ----------------------------------------------------------------------------
// #5298 — deleting a tab must never move the selection to another session.
// The post-delete landing spot is the nearest remaining sibling: the tab that
// took the deleted tab's index, else the previous one. These tests drive the
// real Update path (w opens the consent dialog, y accepts it) because the bug
// lived in the handler's own ordering — a pane reconcile's relayout re-read the
// sidebar and committed the drifted flat index BEFORE the tree re-pin ran — so
// no direct call to a helper can stand in for it.
// ----------------------------------------------------------------------------

// twoSessionTabHome builds the reported repro's shape: a multi-tab alpha whose
// subtree is followed by beta's instance row — the row a deleted last tab's
// stale flat index lands on exactly.
func twoSessionTabHome(t *testing.T) (*home, *session.Instance, *session.Instance) {
	t.Helper()
	h, alpha := multiTabHome(t)
	beta := instanceWithFakeBackend(t, "beta")
	beta.AddTabForTest("agent", session.TabKindAgent)
	h.store.AddInstance(beta)
	return h, alpha, beta
}

// pressWThenY drives the real Update path for the tab-delete gesture: `w` opens
// the confirmation overlay and `y` accepts it, running the whole delete +
// reconcile + re-pin through the same dispatch the user's keys take.
func pressWThenY(t *testing.T, h *home) {
	t.Helper()
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	require.Equal(t, stateConfirm, h.state, "w must open the delete confirmation")
	_, _ = h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.Equal(t, stateDefault, h.state, "y must run the confirmed delete and close the dialog")
	require.Nil(t, h.confirmationOverlay)
}

// assertSelectionOnSiblingTab is the #5298 property: after a delete the cursor
// rests on a TAB row of the SAME session — never the next session's row — at
// the expected sibling slot, and the store's display selection agrees.
func assertSelectionOnSiblingTab(t *testing.T, h *home, inst *session.Instance, wantTab int, wantName string) {
	t.Helper()
	assert.Same(t, inst, h.store.GetSelectedInstance(),
		"the display selection must stay on %q — a tab delete must not jump sessions", inst.Title)
	sel := h.sidebar.GetSelection()
	require.Equal(t, ui.SectionInstances, sel.Kind)
	require.True(t, sel.IsTab,
		"the cursor must land on a sibling tab row, not a session/instance row")
	require.Less(t, sel.ItemIndex, len(h.store.GetInstances()))
	assert.Same(t, inst, h.store.GetInstances()[sel.ItemIndex],
		"the cursor row belongs to %q, not its neighbor", inst.Title)
	assert.Equal(t, wantTab, sel.TabIndex, "cursor lands on the expected sibling slot")
	assert.Equal(t, wantName, inst.GetTabs()[sel.TabIndex].Name)
	assert.Equal(t, wantTab, h.store.ActiveTab(), "the active tab agrees with the cursor row")
}

// TestCloseTab_LastTabDeleteKeepsSelectionInSession is the issue's repro: alpha's
// LAST tab is selected and open as a pane (the state `t` leaves behind), and
// deleting it frees the flat index that beta's instance row then occupies. The
// cursor must land on the previous sibling INSIDE alpha — on master it drifted
// onto beta and pushSelection committed beta as the selection.
func TestCloseTab_LastTabDeleteKeepsSelectionInSession(t *testing.T) {
	h, alpha, _ := twoSessionTabHome(t)

	// Cursor on alpha's last tab row (shell-3, slot 3) with its pane open and
	// focused — exactly what `t → Terminal` leaves.
	h.sidebar.SelectTabRow(alpha.Title, 3)
	pane := openTestPane(t, h, alpha, 3)
	require.Same(t, pane, h.focusedOpenPane())
	require.Same(t, alpha, h.store.GetSelectedInstance())
	require.Equal(t, 3, h.store.ActiveTab())

	recordCloseTab(t, h)
	pressWThenY(t, h)

	require.Equal(t, 3, alpha.TabCount(), "the last tab was deleted")
	assertSelectionOnSiblingTab(t, h, alpha, 2, "shell-2")
}

// TestCloseTab_MiddleTabDeleteKeepsSelectionInSession: a middle tab's slot is
// filled by the NEXT sibling sliding down, so the cursor lands on the tab now
// at the deleted index — shell-3, not the previous sibling and never another
// session.
func TestCloseTab_MiddleTabDeleteKeepsSelectionInSession(t *testing.T) {
	h, alpha, _ := twoSessionTabHome(t)

	h.sidebar.SelectTabRow(alpha.Title, 2) // shell-2; shell-3 still follows it
	openTestPane(t, h, alpha, 2)

	recordCloseTab(t, h)
	pressWThenY(t, h)

	require.Equal(t, 3, alpha.TabCount(), "the middle tab was deleted")
	assertSelectionOnSiblingTab(t, h, alpha, 2, "shell-3")
}

// TestSnapshotTabDeleteKeepsSelectionInSession covers the sibling path that
// shares the code: a tab deleted out-of-band — `af sessions tab-delete`,
// another client, a daemon action — arrives through the snapshot reconcile.
// The selection lands on the same nearest sibling an in-TUI delete would pick.
// On master the same mid-reconcile drift applies: the pane close's relayout
// commits beta (or, after the reconcile's tail re-pin, drops the cursor onto
// alpha's instance row rather than a tab row) — never the sibling tab.
func TestSnapshotTabDeleteKeepsSelectionInSession(t *testing.T) {
	h := newTestHome(t)
	alpha := freshLocalInstance(t, "oobalpha") // real local backend: ReconcileTabsFromData engages
	alpha.SetStatusForTest(session.Running)
	_, err := alpha.AddWebTab("http://localhost:3000", "a")
	require.NoError(t, err)
	_, err = alpha.AddWebTab("http://localhost:3001", "b")
	require.NoError(t, err)
	selectInstance(h, alpha)
	beta := instanceWithFakeBackend(t, "beta")
	beta.AddTabForTest("agent", session.TabKindAgent)
	h.store.AddInstance(beta)
	resizeHome(h, 200, 40)

	// Cursor on alpha's LAST tab row (b, slot 2) with its pane open — the same
	// state the in-TUI repro ends in.
	h.sidebar.SelectTabRow(alpha.Title, 2)
	openTestPane(t, h, alpha, 2)
	require.Same(t, alpha, h.store.GetSelectedInstance())
	require.Equal(t, 2, h.store.ActiveTab())

	// The daemon's next roster no longer lists tab b.
	data := alpha.ToInstanceData()
	require.Equal(t, "b", data.Tabs[len(data.Tabs)-1].Name)
	data.Tabs = data.Tabs[:len(data.Tabs)-1]

	require.True(t, h.reconcileSnapshot([]session.InstanceData{data, beta.ToInstanceData()}),
		"an out-of-band tab drop is a visible change")
	require.Equal(t, 2, alpha.TabCount(), "the reconcile dropped the deleted tab locally")

	assertSelectionOnSiblingTab(t, h, alpha, 1, "a")
}
