package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W8.4 (LG-11): an archived sandbox row has no daemon-side checkout, so freeing
// its title is a record rename. It used to fail with "no worktree to relocate".
func TestRenameArchived_SandboxRowRenamesWithoutAWorktree(t *testing.T) {
	i := &Instance{
		Title:    "s",
		Path:     t.TempDir(),
		Branch:   "root/s",
		backend:  newInertSandboxBackend("docker"),
		liveness: LiveArchived,
	}
	require.Nil(t, i.gitWorktree, "fixture drifted: a sandbox row carries no local worktree")

	require.NoError(t, i.RenameArchived("s (archived)", "/unused/dest", ""))
	assert.Equal(t, "s (archived)", i.Title)
	assert.Equal(t, "root/s", i.Branch, "the recorded branch is untouched")
	assert.Equal(t, LiveArchived, i.GetLiveness())

	// The rollback direction (persist failure) uses the same call.
	require.NoError(t, i.RenameArchived("s", "/unused/dest", ""))
	assert.Equal(t, "s", i.Title)
}

func TestRenameArchived_LocalRowWithoutWorktreeStillRefuses(t *testing.T) {
	i := &Instance{Title: "l", liveness: LiveArchived, backend: &LocalBackend{}}
	err := i.RenameArchived("l (archived)", "/unused/dest", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no worktree to relocate")
}
