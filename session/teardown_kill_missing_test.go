package session

import (
	"fmt"
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

// A missing-row kill that refuses an unverified occupant must read as a
// refusal — the path af will not delete and the remedy — not as the deadline
// cut ErrWorkspaceStateUnknown's own text describes: no deadline fired and af
// removed nothing (#5102). Classification is unchanged: the error still is
// ErrWorkspaceStateUnknown, so the record is kept and the kill retried.
func TestKillTeardown_MissingRowRefusalReadsAsARefusal(t *testing.T) {
	setup := func(t *testing.T) (*Instance, string, string) {
		t.Helper()
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := initTempGitRepo(t)
		runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
		parent := filepath.Join(t.TempDir(), "parent")
		require.NoError(t, os.Mkdir(parent, 0o755))
		wtPath := filepath.Join(parent, "refusal-live")
		runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/refusal-live", wtPath)
		require.NoError(t, os.RemoveAll(wtPath), "deleted outside af")
		inst, err := NewInstance(InstanceOptions{Title: "refusal-live", Path: repoRoot, Program: "claude"})
		require.NoError(t, err)
		gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "refusal-live", "af/refusal-live", "", false, true)
		require.NoError(t, err)
		inst.gitWorktree = gw
		inst.SetWorktreeMissing(WorktreeMissingDeletedReason(wtPath))
		return inst, parent, wtPath
	}
	kill := func(t *testing.T, inst *Instance) error {
		t.Helper()
		mode, _, err := inst.prepareKillTeardown(false)
		require.NoError(t, err)
		require.NotNil(t, mode.recheckMissing, "premise: the missing row is armed")
		err = inst.teardownTabs(mode)
		require.Error(t, err)
		t.Logf("kill teardown error: %q", err.Error())
		assert.True(t, TeardownStateUnknown(err), "the record must still be kept and the kill retried")
		assert.ErrorIs(t, err, ErrWorkspaceStateUnknown)
		assert.NotContains(t, err.Error(), "deadline", "no deadline fired")
		assert.NotContains(t, err.Error(), "partially removed", "af removed nothing")
		assert.Contains(t, err.Error(), `leaving "refusal-live"'s session state intact so the cleanup can be retried`)
		return err
	}

	t.Run("an occupant", func(t *testing.T) {
		inst, _, wtPath := setup(t)
		require.NoError(t, os.Mkdir(wtPath, 0o755))
		err := kill(t, inst)
		assert.Contains(t, err.Error(), "a directory has since appeared at "+wtPath)
		assert.Contains(t, err.Error(), "af has not verified it and will not delete it")
		assert.Contains(t, err.Error(), "inspect it, remove it or move it aside, then kill the session again")
		assert.DirExists(t, wtPath)
	})

	t.Run("a path that cannot be checked", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores the permission denial this case needs")
		}
		inst, parent, wtPath := setup(t)
		require.NoError(t, os.Chmod(parent, 0))
		t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
		err := kill(t, inst)
		assert.Contains(t, err.Error(), "the state of its path "+wtPath+" could not be established")
		assert.Contains(t, err.Error(), "kill the session again once the path answers")
	})

	t.Run("control: a genuine deadline cut keeps the sentinel's text", func(t *testing.T) {
		cut := fmt.Errorf("%w: git worktree remove timed out", ErrWorkspaceStateUnknown)
		retained := worktreeRetentionError(cut, "refusal-live")
		assert.ErrorIs(t, retained, ErrWorkspaceStateUnknown)
		assert.Contains(t, retained.Error(), "cut off by its deadline")
		assert.False(t, workspaceStateRefusal(cut))
	})
}
