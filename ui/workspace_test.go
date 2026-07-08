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
