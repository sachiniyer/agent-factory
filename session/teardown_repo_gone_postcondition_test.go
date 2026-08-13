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

// TestKillTeardown_UnprovenArchiveAbsenceReportsUnknown: only ENOENT proves the
// archive gone (#3278 review). A stat that FAILS — modeled with an unreadable
// parent — is not evidence of absence, so the settled cleanup must still be
// reported unknown instead of letting the row delete over a directory that may
// exist.
func TestKillTeardown_UnprovenArchiveAbsenceReportsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based stat failures cannot be modeled as root")
	}
	root := t.TempDir()
	repoPath := filepath.Join(root, "origin")
	require.NoError(t, exec.Command("git", "init", "-b", "main", repoPath).Run())
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "f.txt"), []byte("x"), 0o644))
	require.NoError(t, exec.Command("git", "-C", repoPath, "add", "f.txt").Run())
	require.NoError(t, exec.Command("git", "-C", repoPath,
		"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init").Run())
	shield := filepath.Join(root, "shield")
	require.NoError(t, os.MkdirAll(shield, 0o755))
	archivedPath := filepath.Join(shield, "archived-wt")
	require.NoError(t, exec.Command("git", "-C", repoPath,
		"worktree", "add", "-b", "af/wt", archivedPath).Run())

	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, archivedPath, "wt", "af/wt", "", false, true,
	)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(repoPath))
	// Make the postcondition's stat FAIL rather than answer: the archive's
	// parent becomes unsearchable after cleanup has already run against the
	// missing origin.
	mode := teardownKill{recheckOrigin: func() error { return nil }}
	require.NoError(t, os.Chmod(shield, 0o000))
	t.Cleanup(func() { _ = os.Chmod(shield, 0o755) })

	state, err := mode.handleWorktree(gw, "wt")
	require.Error(t, err, "an unprovable archive absence must be reported unknown")
	assert.Equal(t, stateUnknown, state)
	assert.ErrorIs(t, err, ErrWorkspaceStateUnknown)
}

// TestArchivedCleanupSettled pins the settlement decision (#3278 review): only
// a conclusive ENOENT proves the archive dealt with; an occupied path or a
// stat that failed rather than answered refuses the settlement, and the caller
// retains the row.
func TestArchivedCleanupSettled(t *testing.T) {
	root := t.TempDir()
	occupied := filepath.Join(root, "archive")
	require.NoError(t, os.Mkdir(occupied, 0o755))

	assert.Error(t, ArchivedCleanupSettled(occupied),
		"an occupied path must refuse the settlement")

	require.NoError(t, os.RemoveAll(occupied))
	assert.NoError(t, ArchivedCleanupSettled(occupied),
		"a conclusively absent path proves the archive dealt with")

	if os.Geteuid() != 0 {
		shield := filepath.Join(root, "shield")
		require.NoError(t, os.MkdirAll(filepath.Join(shield, "inner"), 0o755))
		require.NoError(t, os.Chmod(shield, 0o000))
		t.Cleanup(func() { _ = os.Chmod(shield, 0o755) })
		assert.Error(t, ArchivedCleanupSettled(filepath.Join(shield, "inner")),
			"a stat that failed rather than answered must refuse the settlement")
	}
}
