package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// pressNav drives handleDefaultKeyPress with a mapped nav key, the way
// handleKeyPress dispatches it in stateDefault.
func pressNav(t *testing.T, h *home, key string) {
	t.Helper()
	name, ok := keys.GlobalKeyStringsMap[key]
	require.True(t, ok, "key %q must be mapped", key)
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}, name)
}

// addTreeInstance adds an instance carrying a real agent + shell tab pair
// (the shape of a started instance after `t`) to the home's projection, so
// tree walks and tab jumps have two real slots to land on (#1100: fresh
// instances hold only the agent tab and no slot is padded).
func addTreeInstance(t *testing.T, h *home, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: title, Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.AddTabForTest("agent", session.TabKindAgent)
	inst.AddTabForTest("shell", session.TabKindShell)
	h.store.AddInstance(inst)
	return inst
}

// TestTreeNav_JKWalksTabChildren pins the #1024 PR 3 / #1515 nav model at the
// app layer: j/k walk tab-to-tab, crossing instance boundaries directly from
// the last tab of one instance to the first tab of the next. Each tab row
// drives the store's active tab (the content pane binding), and h folds back
// to the parent title row.
func TestTreeNav_JKWalksTabChildren(t *testing.T) {
	h := newTestHome(t)
	a := addTreeInstance(t, h, "alpha")
	b := addTreeInstance(t, h, "beta")

	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	require.Same(t, a, h.sidebar.GetSelectedInstance())

	// j → alpha's agent tab row; j → terminal tab row. The active tab follows.
	pressNav(t, h, "j")
	sel := h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 0, h.store.ActiveTab())
	assert.Equal(t, 0, h.store.ActiveTab(), "the Agent slot is active")

	pressNav(t, h, "j")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.Equal(t, 1, h.store.ActiveTab(),
		"Enter on this row would attach the terminal tab — the tab dimension routes attach")

	// j past the last child lands on beta's first tab; alpha folds
	// (collapse-by-default), and the cursor never stops on beta's title row.
	pressNav(t, h, "j")
	require.Same(t, b, h.sidebar.GetSelectedInstance())
	assert.Same(t, b, h.store.GetSelectedInstance(), "tree selection retargets the store selection")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 0, sel.TabIndex)
	assert.Equal(t, 0, h.store.ActiveTab())

	// k back up lands on alpha's last tab, not alpha's title row.
	pressNav(t, h, "k")
	require.Same(t, a, h.sidebar.GetSelectedInstance())
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex)

	// h on a tab row folds to the parent instance row.
	require.True(t, h.sidebar.GetSelection().IsTab)
	pressNav(t, h, "h")
	sel = h.sidebar.GetSelection()
	assert.False(t, sel.IsTab, "h folds the subtree and lands on the instance row")
	assert.Same(t, a, h.sidebar.GetSelectedInstance())

	// l re-expands.
	pressNav(t, h, "l")
	pressNav(t, h, "j")
	assert.True(t, h.sidebar.GetSelection().IsTab)
}

func TestTreeNav_DownAutoOpensArchivedAtLiveBoundary(t *testing.T) {
	h := newTestHome(t)
	live := startedLocalInstance(t, "live-one")
	archived := archiveActionInstance(t, "put-away", session.Ready)
	archived.SetArchived()

	h.store.AddInstance(live)
	h.store.AddInstance(archived)
	resizeHome(h, 120, 40)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	require.Same(t, live, h.sidebar.GetSelectedInstance())
	require.NotContains(t, h.sidebar.View(), "put-away", "Archived starts collapsed")

	pressNav(t, h, "j") // live Agent tab
	pressNav(t, h, "j") // live Terminal tab
	require.True(t, h.sidebar.GetSelection().IsTab)

	pressNav(t, h, "j")
	sel := h.sidebar.GetSelection()
	require.Equal(t, ui.SectionArchived, sel.Kind,
		"Down after the last live tab must auto-open Archived and select the first archived row")
	require.False(t, sel.IsHeader)
	require.Same(t, archived, h.sidebar.GetSelectedInstance())
	assert.Contains(t, h.sidebar.View(), "put-away", "auto-opened Archived must render archived rows")

	pressNav(t, h, "k")
	sel = h.sidebar.GetSelection()
	require.Equal(t, ui.SectionInstances, sel.Kind)
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Same(t, live, h.sidebar.GetSelectedInstance())
}

func TestTreeNav_TabStopAcrossInstancePreservesParentForActions(t *testing.T) {
	h := newTestHome(t)
	alpha := startedLocalInstance(t, "alpha")
	beta := startedLocalInstance(t, "beta")
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)
	h.sidebar.SetSelectedInstance(0)
	h.store.SetSelectedInstance(alpha)
	_ = h.selectionChanged()

	pressNav(t, h, "j") // alpha tab 0
	pressNav(t, h, "j") // alpha tab 1
	pressNav(t, h, "j") // beta tab 0, with no beta title stop

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.ItemIndex, "tab row keeps the parent instance index")
	require.Same(t, beta, h.sidebar.GetSelectedInstance(),
		"instance actions resolve the selected tab's parent instance")

	var createdFor string
	t.Cleanup(SetTabCreatorForTest(func(title, repoID string) (string, string, error) {
		createdFor = title
		return spawnDaemonTab(beta)
	}))

	_, _ = h.handleNewTab()
	assert.Equal(t, beta.Title, createdFor,
		"new-tab action must target the parent session of the selected tab")
}

// TestTreeNav_NumberJumpMovesCursorOnTabRows pins the 1-9 muscle-memory rule:
// with the cursor on the instance row a number jump changes only the active
// tab (pre-tree behavior); with the cursor inside the tab subtree the cursor
// follows the jump so the tree and the tab bar agree.
func TestTreeNav_NumberJumpMovesCursorOnTabRows(t *testing.T) {
	h := newTestHome(t)
	addTreeInstance(t, h, "alpha")
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()

	// Cursor on the instance row: jump to tab 2 — cursor stays put.
	_, _ = h.handleTabJump(2)
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.False(t, h.sidebar.GetSelection().IsTab, "cursor stays on the instance row")

	// Cursor on a tab row: jump moves the cursor with the active tab.
	pressNav(t, h, "j") // tab row 0 (re-selects tab 0)
	require.True(t, h.sidebar.GetSelection().IsTab)
	require.Equal(t, 0, h.store.ActiveTab())
	_, _ = h.handleTabJump(2)
	sel := h.sidebar.GetSelection()
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex, "cursor followed the number jump")

	// Out-of-range jump stays a no-op.
	_, _ = h.handleTabJump(9)
	assert.Equal(t, 1, h.store.ActiveTab())
	assert.Equal(t, 1, h.sidebar.GetSelection().TabIndex)
}

// TestTreeNav_TabCreateCloseFromTabRow is the regression test for the PR
// #1081 play-test bug: with the cursor parked ON A TAB ROW, `t` must create
// AND select the new tab, and a following `w` must close exactly the cursor's
// tab — not a stale clamped index (the silent wrong-tab close) — and land on
// the left neighbor. The clobber came from SyncCursorToActiveTab reading
// ActiveTab() only after syncFromStore: the tab-slot change trips the
// structure rebuild, whose pushSelection re-asserts the cursor row's tab index
// over the one the handler just set.
func TestTreeNav_TabCreateCloseFromTabRow(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "tw-tab-row")
	selectInstance(h, inst)

	var closedNames []string
	t.Cleanup(SetTabCreatorForTest(func(title, repoID string) (string, string, error) {
		return spawnDaemonTab(inst)
	}))
	t.Cleanup(SetTabCloserForTest(func(title, repoID, tabName string) error {
		closedNames = append(closedNames, tabName)
		return nil
	}))

	// Park the cursor on tab row 1 (the shell tab).
	pressNav(t, h, "j")
	pressNav(t, h, "j")
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Equal(t, 1, h.store.ActiveTab())

	// t: the new tab (index 2) must be created AND selected, cursor following.
	_, _ = h.handleNewTab()
	require.Equal(t, 3, inst.TabCount())
	assert.Equal(t, 2, h.store.ActiveTab(), "t from a tab row must select the new tab")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 2, sel.TabIndex, "cursor must follow onto the new tab's row")

	// w: must close exactly the cursor's tab — the fresh one — and land left.
	newTabName := inst.GetTabs()[2].Name
	_, _ = h.handleCloseTab()
	require.Equal(t, []string{newTabName}, closedNames,
		"w from a tab row must close the cursor's tab, never a stale index")
	require.Equal(t, 2, inst.TabCount())
	assert.Equal(t, 1, h.store.ActiveTab(), "w must land on the left neighbor")
	sel = h.sidebar.GetSelection()
	assert.True(t, sel.IsTab)
	assert.Equal(t, 1, sel.TabIndex, "cursor must land on the left neighbor's row")
}
