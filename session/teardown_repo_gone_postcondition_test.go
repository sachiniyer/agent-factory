package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// TestKillTeardown_SettledOrdinaryCleanupWithSurvivingArchiveReportsUnknown
// pins the #3278 postcondition. An archived kill's origin recheck is a
// point-in-time probe, and GitWorktree.Cleanup waits for hooks and reaps
// writers before its destructive git command — so an origin deleted inside
// that interval turns every git failure into an answered, settled error while
// the archived directory survives untouched. No probe placement can outrun
// that; what must hold instead is the postcondition: ordinary cleanup must not
// be reported settled while the archived directory still occupies its path,
// because the caller's next step deletes the row that is its only handle.
func TestKillTeardown_SettledOrdinaryCleanupWithSurvivingArchiveReportsUnknown(t *testing.T) {
	root := t.TempDir()
	repoPath := filepath.Join(root, "origin")
	require.NoError(t, exec.Command("git", "init", "-b", "main", repoPath).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "f.txt"), []byte("x"), 0o644))
	require.NoError(t, exec.Command("git", "-C", repoPath, "add", "f.txt").Run())
	require.NoError(t, exec.Command("git", "-C", repoPath,
		"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init").Run())
	archivedPath := filepath.Join(root, "archived-wt")
	require.NoError(t, exec.Command("git", "-C", repoPath,
		"worktree", "add", "-b", "af/wt", archivedPath).Run())

	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, archivedPath, "wt", "af/wt", "", false, true,
	)
	require.NoError(t, err)

	// The origin vanishes AFTER the pre-cleanup recheck answered present —
	// modeled by a recheck that returns nil — and before Cleanup's git
	// commands run.
	require.NoError(t, os.RemoveAll(repoPath))
	mode := teardownKill{recheckOrigin: func() error { return nil }}

	state, err := mode.handleWorktree(gw, "wt")
	require.Error(t, err,
		"a settled ordinary cleanup that left the archived directory in place must be reported unknown")
	assert.Equal(t, stateUnknown, state)
	assert.ErrorIs(t, err, ErrWorkspaceStateUnknown,
		"the caller keys record retention off the unknown-state classifier")
	assert.DirExists(t, archivedPath, "the archived directory must be left intact for the retry")
}
