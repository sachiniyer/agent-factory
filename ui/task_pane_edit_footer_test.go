package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/task"
)

// TestTaskPaneEditActionFooterKeepsEscLabelWhereItFits is #3630's second half.
// The action row used to jump from its full 51-cell form straight to a 39-cell
// one, and of everything it dropped it dropped esc's LABEL — the one key here
// whose destination ("back to the list", not "out of the manager") is not
// guessable from the glyph. At 80x24 the row has ~50 cells, so it was paying
// ~11 spare columns for that.
func TestTaskPaneEditActionFooterKeepsEscLabelWhereItFits(t *testing.T) {
	require.NoError(t, keys.ApplyOverrides(nil))
	t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })

	repo := newGitRepo(t)
	footerAt := func(width int) string {
		tp := NewTaskPane()
		tp.SetSize(width, 12)
		tp.SetTasks([]task.Task{{
			ID: "abc", Name: "nightly", Prompt: "do it", CronExpr: "0 0 * * *",
			ProjectPath: repo, Program: "claude", Enabled: true,
		}})
		tp.SetFocus(true)
		tp.EnterEditSelected()
		lines := strings.Split(tp.String(), "\n")
		return lines[len(lines)-1]
	}

	// The full form is 51 cells; the middle tier is 44. Every width between them
	// used to render the 39-cell form and lose esc's label.
	for _, w := range []int{44, 46, 48, 50} {
		row := footerAt(w)
		assert.Containsf(t, row, "esc list",
			"width %d: esc's destination must be named wherever it fits: %q", w, row)
		assert.Containsf(t, row, "q quit", "width %d: and the quit key stays: %q", w, row)
		assert.NotContainsf(t, row, "…", "width %d: the row must fit, not clip: %q", w, row)
		assert.NotContainsf(t, row, "run now",
			"width %d: the verbs still abbreviate — only the destinations are protected: %q", w, row)
	}

	// Narrower than the middle tier: esc's label is the thing that goes, because
	// the only alternative is dropping the quit key, which is pinned elsewhere.
	narrow := footerAt(40)
	assert.Contains(t, narrow, "q quit", "the quit key outranks esc's label: %q", narrow)
	assert.Contains(t, narrow, "esc", "esc itself is never dropped: %q", narrow)
	assert.NotContains(t, narrow, "…", "the row must fit, not clip: %q", narrow)
}
