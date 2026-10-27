package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2579 Part 2. CLAUDE.md: " · " joins fragments on one line; "—" sets off a
// clause. Four sites used the fragment separator for a clause boundary, and in
// both cases the correct form was visible in the SAME frame, which is what made
// the inconsistency read as a defect rather than a preference.

// The kill dialog joined two independent sentences with `·` while its very next
// paragraph set off the same kind of clause with `—`.
func TestKillConfirmSetsOffItsConsequenceClauseWithADash(t *testing.T) {
	lines := []string{
		unmergedSevereLine("dev/todo-core", 1),
		unmergedSevereLine("dev/todo-core", 3),
	}
	for _, line := range lines {
		assert.Contains(t, line, "— this cannot be undone",
			"a second sentence is a clause, not another fragment on the line")
		assert.NotContains(t, line, "· this cannot be undone")
	}
}

// The pane header flattened `instance · tab — cursor: instance · tab` into
// four indistinguishable fragments. That trailing clause is the ONLY signal
// that the workspace is not showing the row under the cursor, so nothing
// marking where the pane's identity ends actively costs comprehension. The
// preview arm set the same relationship off with parentheses — a third form —
// and scroll mode appended its own with `·`; all three now read alike.
func TestPaneHeaderSetsOffItsClausesWithADash(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Same(t, beta, h.store.GetSelectedInstance())

	view := h.View()
	assert.Contains(t, view, "beta · Agent · preview")
	assert.NotContains(t, view, "original alpha")
	assert.Equal(t, "alpha · Agent", h.paneWindows[paneA.ID()].PreviewOrigin())

	h.cancelPanePreview(false)
	view = h.View()
	assert.Contains(t, view, "alpha · Agent — cursor: beta · Agent",
		"the clause boundary must be a dash so the identity and the clause read apart")
	assert.NotContains(t, view, "· cursor: ",
		"the selection clause must not read as another identity fragment")
}

// TestPaneHeaderCursorClauseFollowsTheTreeCursor is the #5152 review contract:
// "cursor:" claims where the tree highlight rests, so the clause must come
// from the sidebar's cursor-derived selection — not the store's sticky display
// binding, which pushSelection deliberately leaves in place when the cursor
// parks on a section header. Naming the bound row while the cursor visibly
// rests on a header would be a lie about the cursor's position.
func TestPaneHeaderCursorClauseFollowsTheTreeCursor(t *testing.T) {
	h := paneTestHome(t)
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, h.store.GetInstanceByTitle("alpha"), paneA.Instance())

	// Cursor on beta's row while the pane stays bound to alpha: the clause
	// names the row under the cursor.
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Same(t, beta, h.sidebar.GetSelectedInstance())
	require.Equal(t, "beta · Agent", h.paneSelectionHint(paneA))

	// Cursor parked on the Sessions header: the display binding still marks
	// beta, but the cursor names no row, so the clause must drop rather than
	// assert a position the tree is not showing.
	h.sidebar.ClickHeaderKind(ui.SectionInstances)
	require.Nil(t, h.sidebar.GetSelectedInstance(),
		"precondition: the cursor rests on the Sessions header")
	require.Same(t, beta, h.store.GetSelectedInstance(),
		"precondition: the display binding is sticky across a header click")
	require.Empty(t, h.paneSelectionHint(paneA),
		"a header cursor has no instance to name — the clause must disappear")
	h.cancelPanePreview(false)
	view := h.View()
	assert.NotContains(t, view, "cursor:",
		"the pane title carries no cursor clause while the cursor is on a header")
	assert.Contains(t, view, "alpha · Agent")
}
