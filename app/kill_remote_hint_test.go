package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// #5966: the severe kill dialog's recovery hint must describe what an OFF-BOX
// archive does. The local hint ("preserves the worktree and its refs") is false
// there: a docker or hook archive pushes only the current branch, so a side branch
// or stash the dialog just detected would be lost by following it.
func TestKillArchiveHintIsBackendCorrect(t *testing.T) {
	local := killArchiveHint(false, "a")
	assert.Contains(t, local, "preserves the worktree and its refs")

	remote := killArchiveHint(true, "a")
	assert.NotContains(t, remote, "preserves the worktree and its refs", "the local guarantee is false off-box")
	assert.Contains(t, remote, "not a full backup")
	assert.Contains(t, remote, "other branches and stashes are lost")
	assert.Contains(t, remote, "copy those off the host first")
}
