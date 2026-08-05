package ui

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
)

func TestEmptyWorkspacePreservesFrameAtTinySize(t *testing.T) {
	lay := layout.Grid{}.Solve(40, 10)
	out := EmptyWorkspace(lay.Workspace)

	requireExactRect(t, out, lay.Workspace, "empty workspace")
	lines := strings.Split(stripANSI(out), "\n")
	assert.True(t, strings.HasSuffix(lines[0], "╮"), "top right corner must stay visible")
	assert.True(t, strings.HasSuffix(lines[len(lines)-1], "╯"), "bottom right corner must stay visible")
}

func TestFirstRunWorkspacePreservesFrameAtTinySize(t *testing.T) {
	lay := layout.Grid{}.Solve(40, 10)
	out := FirstRunWorkspace(lay.Workspace)

	requireExactRect(t, out, lay.Workspace, "first-run workspace")
	lines := strings.Split(stripANSI(out), "\n")
	assert.True(t, strings.HasSuffix(lines[0], "╮"), "top right corner must stay visible")
	assert.True(t, strings.HasSuffix(lines[len(lines)-1], "╯"), "bottom right corner must stay visible")
}

// TestNoActiveProjectWorkspaceDoesNotAdvertiseCreate is #2830. Launched outside
// a repo (registry mode, #2477) af has no active project, focus lands on the
// Projects section, and that section is a captive list that consumes `n` by
// design (#1620). Even reached from the tree, `n` refuses: a session needs a
// project to live in (#2764/#2815). So the zero-session copy advertising `n`
// was a promise no focused region could keep — the user pressed it and nothing
// happened at all.
func TestNoActiveProjectWorkspaceDoesNotAdvertiseCreate(t *testing.T) {
	lay := layout.Grid{}.Solve(80, 24)
	out := stripANSI(NoActiveProjectWorkspace(lay.Workspace, "press ctrl+p to pick one"))

	assert.Contains(t, out, "No project selected — press ctrl+p to pick one.",
		"the empty state must name the one key that moves the user forward")
	assert.NotContains(t, out, "press n",
		"advertising create in a mode where no focused region can run it is the bug")
	assert.NotContains(t, out, "No sessions yet",
		"the blocker is the missing project, not the missing sessions")
}

// With no key bound to switch_project the line still has to say something
// actionable rather than degrade to a bare statement of fact.
func TestNoActiveProjectWorkspaceWithoutAKeyStillPointsSomewhere(t *testing.T) {
	lay := layout.Grid{}.Solve(80, 24)
	out := stripANSI(NoActiveProjectWorkspace(lay.Workspace, "pick one in the Projects section"))

	assert.Contains(t, out, "No project selected — pick one in the Projects section.")
	assert.NotContains(t, out, "press n")
}

// The ordinary zero-session state is untouched: inside a repo `n` works, and
// that is still the right thing to advertise.
func TestFirstRunWorkspaceStillAdvertisesCreate(t *testing.T) {
	lay := layout.Grid{}.Solve(80, 24)
	out := stripANSI(FirstRunWorkspace(lay.Workspace))

	assert.Contains(t, out, "No sessions yet — press n to create one.")
}

func TestNoActiveProjectWorkspacePreservesFrameAtTinySize(t *testing.T) {
	lay := layout.Grid{}.Solve(40, 10)
	out := NoActiveProjectWorkspace(lay.Workspace, "press ctrl+p to pick one")

	requireExactRect(t, out, lay.Workspace, "no-active-project workspace")
	lines := strings.Split(stripANSI(out), "\n")
	assert.True(t, strings.HasSuffix(lines[0], "╮"), "top right corner must stay visible")
	assert.True(t, strings.HasSuffix(lines[len(lines)-1], "╯"), "bottom right corner must stay visible")
}
