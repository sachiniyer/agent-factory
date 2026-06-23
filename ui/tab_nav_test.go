package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// instanceWithTabs builds a bare (unstarted) instance carrying n tabs so the tab
// bar / navigation math can be exercised without a live tmux session. Tab[0] is
// the agent tab; the rest are shell tabs.
func instanceWithTabs(n int) *session.Instance {
	tabs := make([]*session.Tab, 0, n)
	tabs = append(tabs, &session.Tab{Name: "agent", Kind: session.TabKindAgent})
	for i := 1; i < n; i++ {
		tabs = append(tabs, &session.Tab{Name: "shell", Kind: session.TabKindShell})
	}
	return &session.Instance{Tabs: tabs}
}

// TestTabbedWindowJumpToTab covers the number-key jump: in-range indices select
// the tab, out-of-range indices are a no-op (#930 PR 4).
func TestTabbedWindowJumpToTab(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	tw.SetInstance(instanceWithTabs(3))

	require.True(t, tw.JumpToTab(2))
	require.Equal(t, 2, tw.GetActiveTab())

	require.False(t, tw.JumpToTab(3), "jumping past the last tab is a no-op")
	require.Equal(t, 2, tw.GetActiveTab(), "a no-op jump must not move the active tab")

	require.False(t, tw.JumpToTab(-1), "a negative index is a no-op")
	require.Equal(t, 2, tw.GetActiveTab())

	require.True(t, tw.JumpToTab(0), "the agent tab is always jumpable")
	require.Equal(t, 0, tw.GetActiveTab())
}

// TestTabbedWindowSelectLastAndNeighbor covers SelectLastTab (used after a new
// tab is appended) and SelectTab (used to land on a neighbor after a close).
func TestTabbedWindowSelectLastAndNeighbor(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	tw.SetInstance(instanceWithTabs(4))

	tw.SelectLastTab()
	require.Equal(t, 3, tw.GetActiveTab(), "SelectLastTab selects the final tab")

	tw.SelectTab(2)
	require.Equal(t, 2, tw.GetActiveTab())

	// Out-of-range selections clamp into range rather than dangling.
	tw.SelectTab(99)
	require.Equal(t, 3, tw.GetActiveTab())
	tw.SelectTab(-5)
	require.Equal(t, 0, tw.GetActiveTab())
}

// TestTabbedWindowClampsActiveTabOnInstanceSwitch is the audit guard: switching
// to an instance with fewer tabs must not leave activeTab pointing past the end
// (which would make isAgentSlot() lie and capture a phantom slot).
func TestTabbedWindowClampsActiveTabOnInstanceSwitch(t *testing.T) {
	tw := NewTabbedWindow(NewTabPane())
	tw.SetInstance(instanceWithTabs(4))
	require.True(t, tw.JumpToTab(3))
	require.Equal(t, 3, tw.GetActiveTab())

	tw.SetInstance(instanceWithTabs(2))
	require.LessOrEqual(t, tw.GetActiveTab(), 1,
		"activeTab must be clamped when switching to an instance with fewer tabs")
	require.GreaterOrEqual(t, tw.GetActiveTab(), 0)
}
