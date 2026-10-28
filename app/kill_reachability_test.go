package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A squash preserves the diff but not the original commit identity. The
// confirmation describes reachability even after the change lands remotely.
func TestHandleKillSquashMergeWarnsAboutReachability(t *testing.T) {
	repo, base := initBaseRepo(t)
	wt := addWorktree(t, repo, base, "dev/squashed")
	commitInWorktree(t, wt)
	killGit(t, repo, "merge", "--squash", "dev/squashed")
	killGit(t, repo, "commit", "-m", "squashed change")
	killGit(t, repo, "update-ref", "refs/remotes/origin/master", "HEAD")
	inst := startedWorktreeInstance(t, "squashed", repo, wt, "dev/squashed", base)
	_, h := armKill(t, inst)
	rendered := flatten(h.confirmationOverlay.Render())
	assert.Contains(t, rendered, "not reachable from any remote-tracking ref")
	assert.NotContains(t, rendered, "unmerged PR")
	assert.Equal(t, "k", h.confirmationOverlay.ConfirmKey)
}
