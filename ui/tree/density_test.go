package tree

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

func TestCollapsedSessionUsesOneRowAndRetainsSelectedDetail(t *testing.T) {
	inst, err := session.FromInstanceData(session.InstanceData{ID: "dense", Title: "Review change", Branch: "review-branch", Program: "test", BackendType: "remote", Status: session.Ready, Liveness: session.LiveReady})
	require.NoError(t, err)
	inst.SetStatusForTest(session.Ready)
	renderer := NewInstanceRenderer()
	renderer.SetWidth(80)
	compact := renderer.Render(inst, 1, false, false, false)
	require.Equal(t, 1, lipgloss.Height(compact))
	require.Contains(t, compact, "Review change")
	require.Contains(t, compact, readyIcon)
	require.NotContains(t, compact, "review-branch")
	require.Contains(t, renderer.Render(inst, 1, true, false, false), "review-branch")
	inst.Branch = ""
	require.False(t, strings.Contains(renderer.Render(inst, 1, true, false, false), branchIcon), "missing branches have no placeholder")
	inst.ReconcileArchiveWarning("Archive incomplete: complete original tree retained at /retained/source")
	require.Contains(t, renderer.Render(inst, 1, false, false, false), "/retained/source", "density never hides recovery locations")
}
