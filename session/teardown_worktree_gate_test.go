package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// An EXTERNAL worktree is the user's own checkout and Cleanup removes nothing
// for it. Handing its path to the occupancy check would gate a kill on the very
// directory the user runs `af` from — their shell sits there — and retain the
// tombstoned record indefinitely over a process that was never in danger.
//
// There is no destructive action to protect, so there is nothing to gate.
func TestWorktreePathOf_ExternalWorktreeIsNotGated(t *testing.T) {
	external, err := git.NewGitWorktreeFromStorage(
		"/repo", "/repo", "session", "branch", "deadbeef", true, false,
	)
	require.NoError(t, err)
	require.True(t, external.IsExternalWorktree(), "precondition: the fixture must be external")

	require.Empty(t, worktreePathOf(external),
		"an external worktree is never removed, so gating on it can only refuse kills that were always safe")
}

// An af-created worktree IS removed, so its path is supplied and the occupancy
// evidence applies.
func TestWorktreePathOf_ManagedWorktreeIsGated(t *testing.T) {
	managed, err := git.NewGitWorktreeFromStorage(
		"/repo", "/wt", "session", "branch", "deadbeef", false, true,
	)
	require.NoError(t, err)
	require.False(t, managed.IsExternalWorktree(), "precondition: the fixture must be managed")

	require.NotEmpty(t, worktreePathOf(managed),
		"an af-created worktree is about to be deleted, which is exactly what the occupancy check protects")
}

// gw is documented nil-able on this path.
func TestWorktreePathOf_NilWorktreeIsNotGated(t *testing.T) {
	require.Empty(t, worktreePathOf(nil))
}
