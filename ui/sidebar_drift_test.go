package ui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// #7551 — every row verb resolves its target through syncFromStore, which
// lazily applies any projection mutation that landed since the last painted
// frame. rebuildVisibleItems keeps the cursor's flat index, so a mutation
// that folds or removes rows ABOVE the cursor's own row — the reconcile's
// toRemove path, a kill completing and dropping its row (handleInstanceKilled),
// an archive re-homing a row into the folder (handleInstanceArchived) —
// silently re-points the index at an earlier row: a neighbor session, a
// section header, or the root separator. The next sidebar read then names a
// row the user never selected ("cursor ended on beta"), and a verb dispatched
// against it lands on the wrong row — or on nothing, when the index lands on
// a header — exactly the play-test's silent key. followCursorRow must re-pin
// the cursor to its own row whenever that row still exists, whichever
// direction the rebuild moved it.

// TestSidebar_CursorKeepsItsRowWhenRowsVanishAbove: the row above the cursor's
// session is removed; the cursor must stay on its own tab row instead of
// drifting onto the next session.
func TestSidebar_CursorKeepsItsRowWhenRowsVanishAbove(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	alpha := archTestInstance(t, "alpha", session.Ready)
	beta := archTestInstance(t, "beta", session.Ready)
	gamma := archTestInstance(t, "gamma", session.Ready)
	for _, inst := range []*session.Instance{alpha, beta, gamma} {
		addAgentShellTabs(inst)
		addTestInstance(s, inst)
	}
	s.SetSize(40, 40)

	s.SelectInstance(beta)
	s.SelectTabRow("beta", 1)
	require.Same(t, beta, s.GetSelectedInstance(), "precondition: cursor on beta's second tab row")

	// One row vanishes above the cursor — the kept flat index then names
	// gamma's row where beta's second tab row sat.
	require.True(t, s.proj.RemoveInstance(alpha))
	require.Same(t, beta, s.GetSelectedInstance(),
		"the cursor must follow its own row, not drift onto gamma (#7551)")
	sel := s.GetSelection()
	require.True(t, sel.IsTab && sel.TabIndex == 1,
		"the cursor must still rest on beta's second tab row, not merely somewhere inside beta")
}

// TestSidebar_CursorKeepsItsRowOffTheArchivedHeader: the same removal, but the
// drifted index lands exactly on the expanded Archived folder's header — the
// shape that resolves no instance at all, so a row verb dies silently.
func TestSidebar_CursorKeepsItsRowOffTheArchivedHeader(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	alpha := archTestInstance(t, "alpha", session.Ready)
	beta := archTestInstance(t, "beta", session.Ready)
	old := archTestInstance(t, "old", session.Archived)
	for _, inst := range []*session.Instance{alpha, beta, old} {
		addAgentShellTabs(inst)
		addTestInstance(s, inst)
	}
	s.SetSize(40, 40)
	s.expandSectionKind(SectionArchived)

	s.SelectInstance(beta)
	s.SelectTabRow("beta", 1)
	require.Same(t, beta, s.GetSelectedInstance(), "precondition: cursor on beta's second tab row")

	// With the Archived folder open, dropping alpha puts the Archived header
	// exactly where the cursor's flat index points — GetSelectedInstance
	// would resolve nil and every row verb would no-op.
	require.True(t, s.proj.RemoveInstance(alpha))
	require.Same(t, beta, s.GetSelectedInstance(),
		"the cursor must follow beta's tab row, not drift onto the Archived header (#7551)")
}

// TestSidebar_CursorKeepsItsInstanceRowWhenRowsVanishAbove: the same removal
// with the cursor on the session's own INSTANCE row — the drift there slid the
// cursor down onto its first tab row (still the same session, but a different
// row than the one the user left it on).
func TestSidebar_CursorKeepsItsInstanceRowWhenRowsVanishAbove(t *testing.T) {
	s := NewSidebar(store.NewProjection())
	alpha := archTestInstance(t, "alpha", session.Ready)
	beta := archTestInstance(t, "beta", session.Ready)
	gamma := archTestInstance(t, "gamma", session.Ready)
	for _, inst := range []*session.Instance{alpha, beta, gamma} {
		addAgentShellTabs(inst)
		addTestInstance(s, inst)
	}
	s.SetSize(40, 40)

	s.SelectInstance(beta)
	require.Same(t, beta, s.GetSelectedInstance(), "precondition: cursor on beta")
	require.False(t, s.GetSelection().IsTab, "precondition: cursor on beta's instance row")

	require.True(t, s.proj.RemoveInstance(alpha))
	sel := s.GetSelection()
	require.False(t, sel.IsHeader, "the cursor must not end up on a header")
	require.False(t, sel.IsTab,
		"the cursor must stay on beta's instance row, not slide onto a tab row (#7551)")
	require.Same(t, beta, s.GetSelectedInstance())
}
