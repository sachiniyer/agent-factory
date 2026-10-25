package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// A probe that saw ENOENT while af's own move had the path empty must not be
// applied once that move has been rolled back to the same path. The undo of a
// committed archive is exactly that ABA: BeginRestore moves the worktree back to
// the path the probe asked about and AbortRestoreToLost clears the op, so the
// gw pointer, the path and OpNone all match again. Only the lifecycle epoch
// records that an operation began and finished while the probe was in flight
// (#5102).
func TestRefreshWorktreeMissing_ProbeSpanningARolledBackOperationIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	inst := worktreeMissingInstance(t, path)
	inst.SetStartedForTest(true)
	inst.liveness = LiveArchived // a committed archive, about to be undone

	previous := afterWorktreeMissingProbe
	t.Cleanup(func() { afterWorktreeMissingProbe = previous })
	afterWorktreeMissingProbe = func() {
		afterWorktreeMissingProbe = func() {}
		// The probe's ENOENT is in hand. The operation that made the path empty
		// now runs to its rollback: the worktree lands back at the same path.
		require.NoError(t, inst.Transition(BeginRestore()))
		require.NoError(t, os.MkdirAll(path, 0o755))
		require.NoError(t, inst.Transition(AbortRestoreToLost()))
	}

	missing, changed := inst.RefreshWorktreeMissing()
	assert.False(t, missing, "a probe that spans a completed rollback must not flag a present worktree")
	assert.False(t, changed)
	gotMissing, reason := inst.WorktreeMissing()
	assert.False(t, gotMissing)
	assert.Empty(t, reason)
	assert.DirExists(t, path, "premise: the worktree is present again")

	// The next poll re-probes from scratch and answers truthfully.
	missing, _ = inst.RefreshWorktreeMissing()
	assert.False(t, missing, "a present path never sets the flag")
}

// A probe whose lstat lands while an archived-title reuse has the worktree moved
// aside reads ENOENT for af's own move. When that reuse cannot persist, the
// daemon renames the archive back to the very path the probe asked about, so
// the gw pointer, the recorded path and OpNone all match again over a directory
// that is present — the same ABA as a rolled-back restore, reached through
// RenameArchived, which moves neither lifecycle axis. Only the epoch bump the
// rename makes records that it ran while the probe was out (#5102).
func TestRefreshWorktreeMissing_ProbeSpanningARenameRoundTripIsDropped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
	wtPath := filepath.Join(filepath.Dir(repoRoot), "reused-title")
	runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/reused-title", wtPath)
	newDest := filepath.Join(filepath.Dir(repoRoot), "reused-title-2")

	inst, err := NewInstance(InstanceOptions{Title: "reused-title", Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "reused-title", "af/reused-title", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	inst.liveness = LiveArchived

	// The probe's lstat lands mid-move: the reuse's rename has carried the bytes
	// out of the recorded path, and the record still names it.
	inFlight := filepath.Join(t.TempDir(), "in-flight")
	require.NoError(t, os.Rename(wtPath, inFlight))

	previous := afterWorktreeMissingProbe
	t.Cleanup(func() { afterWorktreeMissingProbe = previous })
	afterWorktreeMissingProbe = func() {
		afterWorktreeMissingProbe = func() {}
		// The probe's ENOENT is in hand. The reuse finishes its move under the
		// new title, its persist fails, and the rollback moves it home.
		require.NoError(t, os.Rename(inFlight, wtPath))
		require.NoError(t, inst.RenameArchived("reused-title-2", newDest, ""))
		require.NoError(t, inst.RenameArchived("reused-title", wtPath, ""))
	}

	missing, changed := inst.RefreshWorktreeMissing()
	assert.False(t, missing, "a probe that spans a rename round trip must not flag a present worktree")
	assert.False(t, changed)
	gotMissing, reason := inst.WorktreeMissing()
	assert.False(t, gotMissing)
	assert.Empty(t, reason)
	assert.False(t, inst.ToInstanceData().Worktree.Missing, "the persisted row must not carry the flag")
	assert.Equal(t, wtPath, gw.GetWorktreePath(), "premise: the record names the probed path again")
	assert.DirExists(t, wtPath, "premise: the worktree is present again")

	// The next poll re-probes from scratch and answers truthfully.
	missing, _ = inst.RefreshWorktreeMissing()
	assert.False(t, missing, "a present path never sets the flag")
}
