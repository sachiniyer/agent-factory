package app

import (
	"testing"

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
		unmergedSevereLine("dev/todo-core", 1, ""),
		unmergedSevereLine("dev/todo-core", 3, "main"),
	}
	for _, line := range lines {
		assert.Contains(t, line, "— this cannot be undone",
			"a second sentence is a clause, not another fragment on the line")
		assert.NotContains(t, line, "· this cannot be undone")
	}
}

// The pane header flattened `instance · tab — selected: instance · tab` into
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
	assert.Contains(t, view, "Preview beta · ◆ Agent — original alpha · ◆ Agent",
		"the preview's origin is a clause, not a parenthetical aside")

	h.cancelPanePreview(false)
	view = h.View()
	assert.Contains(t, view, "alpha · ◆ Agent — selected: beta · ◆ Agent",
		"the clause boundary must be a dash so the identity and the clause read apart")
	assert.NotContains(t, view, "· selected: ",
		"the selection clause must not read as another identity fragment")
}
