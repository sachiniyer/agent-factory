package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/git"
)

// A live row flagged worktree-missing reaches kill teardown with nothing
// fencing its path: no record, or an identity-unknown stall discharged on
// proven absence. A directory appearing there before the deletion boundary is
// unverified, so the boundary re-proves absence and kill refuses an occupant
// rather than RemoveAll it; a still-absent path settles git's stale
// registration (#5102).
func TestKillTeardown_MissingLiveRowRechecksItsPathAtTheDeletionBoundary(t *testing.T) {
	setup := func(t *testing.T) (*Instance, *git.GitWorktree, string, string) {
		t.Helper()
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := initTempGitRepo(t)
		runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
		wtPath := filepath.Join(filepath.Dir(repoRoot), "missing-live")
		runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/missing-live", wtPath)
		require.NoError(t, os.RemoveAll(wtPath), "deleted outside af")
		inst, err := NewInstance(InstanceOptions{Title: "missing-live", Path: repoRoot, Program: "claude"})
		require.NoError(t, err)
		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "missing-live", "af/missing-live", "", false, true)
		require.NoError(t, err)
		inst.gitWorktree = gw
		require.NotEqual(t, LiveArchived, inst.liveness, "premise: a live row")
		inst.SetWorktreeMissing(WorktreeMissingDeletedReason(wtPath))
		return inst, gw, repoRoot, wtPath
	}
	registered := func(t *testing.T, repoRoot, path string) bool {
		t.Helper()
		out, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
		require.NoError(t, err)
		resolved, _ := filepath.EvalSymlinks(filepath.Dir(path))
		return strings.Contains(string(out), "worktree "+filepath.Join(resolved, filepath.Base(path))+"\n") ||
			strings.Contains(string(out), "worktree "+path+"\n")
	}

	for _, occupant := range []string{"directory", "symlink"} {
		t.Run("a "+occupant+" appearing after admission refuses and survives", func(t *testing.T) {
			inst, gw, repoRoot, wtPath := setup(t)
			mode, _, err := inst.prepareKillTeardown(false)
			require.NoError(t, err)
			require.NotNil(t, mode.recheckMissing, "a missing live row must carry the boundary recheck")

			target := t.TempDir()
			marker := filepath.Join(target, "users-work.txt")
			require.NoError(t, os.WriteFile(marker, []byte("not af's"), 0o644))
			restore := SetBeforeKillTeardownOriginRecheckForTest(func() {
				if occupant == "symlink" {
					require.NoError(t, os.Symlink(target, wtPath))
					return
				}
				require.NoError(t, os.Rename(target, wtPath))
				marker = filepath.Join(wtPath, "users-work.txt")
			})
			defer restore()

			state, err := mode.handleWorktree(gw, "missing-live")
			require.Error(t, err)
			assert.Equal(t, stateUnknown, state, "the tombstone must stay as the retry handle")
			assert.ErrorIs(t, err, ErrWorkspaceStateUnknown)
			assert.Contains(t, err.Error(), wtPath)
			data, readErr := os.ReadFile(marker)
			require.NoError(t, readErr, "kill must not delete an unverified occupant")
			assert.Equal(t, "not af's", string(data))
			assert.True(t, registered(t, repoRoot, wtPath), "nothing ran past the refusal")
		})
	}

	t.Run("a still-absent path settles the stale registration", func(t *testing.T) {
		inst, gw, repoRoot, wtPath := setup(t)
		require.True(t, registered(t, repoRoot, wtPath), "premise: git still lists the deleted worktree")
		mode, _, err := inst.prepareKillTeardown(false)
		require.NoError(t, err)
		require.NotNil(t, mode.recheckMissing)

		state, err := mode.handleWorktree(gw, "missing-live")
		require.NoError(t, err)
		assert.Equal(t, stateKnown, state)
		assert.False(t, registered(t, repoRoot, wtPath), "git's stale registration is reaped")
		assert.NoDirExists(t, wtPath)
	})
}

// A live row the missing flag never reached is exposed the same way (#5102).
// An identity-unknown stall keeps the presence probe answering unknown, so the
// flag is never set; kill admission then discharges the stall on proven
// absence, and with nothing left fencing the path ordinary Cleanup's
// unregistered RemoveAll would delete whatever appeared there — during pane
// teardown, or already before teardown entry. A row whose worktree was deleted
// outside af before the poll flagged it is the same exposure without the stall.
// Each must carry the deletion-boundary recheck and refuse the occupant.
func TestKillTeardown_UnflaggedLiveRowProvenAbsentRechecksItsPath(t *testing.T) {
	setup := func(t *testing.T, stalled bool) (*Instance, *git.GitWorktree, string, string) {
		t.Helper()
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := initTempGitRepo(t)
		runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
		wtPath := filepath.Join(filepath.Dir(repoRoot), "stalled-live")
		runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/stalled-live", wtPath)
		require.NoError(t, os.RemoveAll(wtPath), "deleted outside af")
		inst, err := NewInstance(InstanceOptions{Title: "stalled-live", Path: repoRoot, Program: "claude"})
		require.NoError(t, err)
		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "stalled-live", "af/stalled-live", "", false, true)
		require.NoError(t, err)
		if stalled {
			require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{State: git.RelocationRecoveryStalled}))
			require.True(t, gw.HasUnresolvedRelocation(), "premise: an identity-unknown stall")
		}
		inst.gitWorktree = gw
		require.NotEqual(t, LiveArchived, inst.liveness, "premise: a live row")
		missing, _ := inst.WorktreeMissing()
		require.False(t, missing, "premise: the missing flag never reached this row")
		return inst, gw, repoRoot, wtPath
	}
	occupy := func(t *testing.T, wtPath string) string {
		t.Helper()
		target := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(target, "users-work.txt"), []byte("not af's"), 0o644))
		require.NoError(t, os.Rename(target, wtPath))
		return filepath.Join(wtPath, "users-work.txt")
	}
	refused := func(t *testing.T, mode teardownKill, gw *git.GitWorktree, wtPath, marker string) {
		t.Helper()
		state, err := mode.handleWorktree(gw, "stalled-live")
		require.Error(t, err, "kill must refuse an unverified occupant")
		assert.Equal(t, stateUnknown, state, "the tombstone must stay as the retry handle")
		assert.ErrorIs(t, err, ErrWorkspaceStateUnknown)
		assert.Contains(t, err.Error(), wtPath)
		data, readErr := os.ReadFile(marker)
		require.NoError(t, readErr, "kill must not delete an unverified occupant")
		assert.Equal(t, "not af's", string(data))
	}

	t.Run("discharged at admission, occupant appears during pane teardown", func(t *testing.T) {
		inst, gw, _, wtPath := setup(t, true)
		require.NoError(t, inst.ValidateWorktreeDestructionAdmission())
		require.False(t, gw.HasUnresolvedRelocation(), "premise: admission discharged the stall")
		mode, _, err := inst.prepareKillTeardown(false)
		require.NoError(t, err)
		marker := occupy(t, wtPath)
		refused(t, mode, gw, wtPath, marker)
	})

	t.Run("discharged at admission, occupant already present at teardown entry", func(t *testing.T) {
		inst, gw, _, wtPath := setup(t, true)
		require.NoError(t, inst.ValidateWorktreeDestructionAdmission())
		marker := occupy(t, wtPath)
		mode, _, err := inst.prepareKillTeardown(false)
		require.NoError(t, err)
		refused(t, mode, gw, wtPath, marker)
	})

	t.Run("record-free row absent at teardown entry, occupant appears during pane teardown", func(t *testing.T) {
		inst, gw, _, wtPath := setup(t, false)
		mode, _, err := inst.prepareKillTeardown(false)
		require.NoError(t, err)
		marker := occupy(t, wtPath)
		refused(t, mode, gw, wtPath, marker)
	})

	t.Run("a still-absent path settles the stale registration", func(t *testing.T) {
		inst, gw, repoRoot, wtPath := setup(t, true)
		require.NoError(t, inst.ValidateWorktreeDestructionAdmission())
		mode, _, err := inst.prepareKillTeardown(false)
		require.NoError(t, err)
		state, err := mode.handleWorktree(gw, "stalled-live")
		require.NoError(t, err)
		assert.Equal(t, stateKnown, state)
		out, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
		require.NoError(t, err)
		assert.NotContains(t, string(out), filepath.Base(wtPath), "git's stale registration is reaped")
	})

	t.Run("control: a present worktree keeps ordinary cleanup", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := initTempGitRepo(t)
		runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
		wtPath := filepath.Join(filepath.Dir(repoRoot), "present-live")
		runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/present-live", wtPath)
		inst, err := NewInstance(InstanceOptions{Title: "present-live", Path: repoRoot, Program: "claude"})
		require.NoError(t, err)
		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "present-live", "af/present-live", "", false, true)
		require.NoError(t, err)
		inst.gitWorktree = gw
		mode, _, err := inst.prepareKillTeardown(false)
		require.NoError(t, err)
		assert.Nil(t, mode.recheckMissing, "a present, never-discharged worktree is not armed")
		state, err := mode.handleWorktree(gw, "present-live")
		require.NoError(t, err)
		assert.Equal(t, stateKnown, state)
		assert.NoDirExists(t, wtPath)
	})
}
