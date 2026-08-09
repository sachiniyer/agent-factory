package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// A ghost's tmux enumeration must include PENDING TAB CLEANUP handles, or kill
// deletes the worktree around a session it never attempted to stop (#3137).
//
// PendingTabCleanup exists precisely because a tab's removal from Tabs is
// already durable while its teardown is not confirmed (#2669) — so these are the
// names most likely to still have a live process, and they were the only ones
// ghostTmuxNames did not enumerate.
//
// The ghostBlind guard does not cover the gap: CheckWorktreeOccupants runs only
// when a tmux kill was blind, so if every ENUMERATED session dies cleanly the
// guard never runs, and the un-enumerated pending handle keeps running while git
// deletes the directory underneath it.
func TestGhostTmuxNames_IncludesPendingTabCleanupHandles(t *testing.T) {
	data := &session.InstanceData{
		TmuxName: "af_agent",
		Tabs: []session.TabData{
			{TmuxName: "af_agent"},
			{TmuxName: "af_shell"},
		},
		PendingTabCleanup: []session.TabCleanupData{
			{TmuxName: "af_pending"},
		},
	}

	names := ghostTmuxNames(data)

	require.Contains(t, names, "af_pending",
		"a pending cleanup handle is a tmux session with a live process and no other record of it; "+
			"omitting it means kill deletes the worktree while that process is still writing into it")
	require.Contains(t, names, "af_agent")
	require.Contains(t, names, "af_shell")

	// Dedupe must still hold across the three sources.
	seen := map[string]int{}
	for _, name := range names {
		seen[name]++
	}
	for name, count := range seen {
		require.Equal(t, 1, count, "name %q enumerated %d times", name, count)
	}
}

// A pending handle that duplicates a live tab must not be enumerated twice, and
// an empty handle must be ignored — the same contract the other two sources have.
func TestGhostTmuxNames_PendingHandlesDedupeAndSkipEmpty(t *testing.T) {
	data := &session.InstanceData{
		TmuxName: "af_agent",
		Tabs:     []session.TabData{{TmuxName: "af_agent"}},
		PendingTabCleanup: []session.TabCleanupData{
			{TmuxName: "af_agent"},
			{TmuxName: ""},
			{TmuxName: "af_ghost"},
		},
	}

	names := ghostTmuxNames(data)
	require.Equal(t, []string{"af_agent", "af_ghost"}, names)
}
