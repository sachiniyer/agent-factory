package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ghostFor turns an archived record-free fixture into a ghost: the tracked
// instance is dropped and record materialization is forced to fail, so
// KillSession resolves the persisted row instead of a live Instance.
func ghostFor(t *testing.T, manager *Manager, repoID, title string) {
	t.Helper()
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, title))
	manager.mu.Unlock()
	previousRestore := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(data session.InstanceData) (*session.Instance, error) {
		if data.Title == title {
			return nil, errors.New("forced ghost: record cannot be materialized")
		}
		return previousRestore(data)
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = previousRestore })
	previousTmux := ghostKillTmuxByName
	ghostKillTmuxByName = func(string) (tmux.PaneState, bool, error) {
		return tmux.PaneStateKnown, false, nil
	}
	t.Cleanup(func() { ghostKillTmuxByName = previousTmux })
}

// archivedRecordFreeInstance builds the #3176 precondition: an archived session
// with no relocation record, because no restore was ever attempted against it.
// The failed-restore route (#3144) creates cleanup authorization on its way
// out; a direct kill starts with nothing.
func archivedRecordFreeInstance(
	t *testing.T, title string,
) (*Manager, string, string, *session.Instance, string) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, title)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.Nil(t, recordFor(t, repoID, title).Worktree.RelocationRecovery,
		"a direct kill must start from a record-free archive: no restore was attempted")
	return manager, repoID, repoPath, inst, archivedPath
}

// TestKillSession_DirectRepoGoneKillConsumesArchivedIdentity is the #3176
// current-master regression. Killing an archived session whose origin repo was
// deleted — with no restore attempted first — must establish an
// identity-qualified cleanup authorization before its tombstone and consume the
// exact archived directory through the claimed repo-gone transaction. On master
// the admission reads the absent relocation record as permission, ordinary git
// cleanup answers with failures against the missing origin, the row is deleted
// as settled, and the archived directory is orphaned with no handle left.
func TestKillSession_DirectRepoGoneKillConsumesArchivedIdentity(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-repo-gone-kill")
	require.NoError(t, os.RemoveAll(repoPath))

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-repo-gone-kill", RepoID: repoID})
	require.NoError(t, err,
		"a conclusively repo-gone direct kill must be admitted with fresh cleanup authorization")
	assert.True(t, inst.UserKilled(), "the admitted kill must reach its tombstone")
	assert.False(t, exists(archivedPath),
		"the kill must consume the exact authorized archived directory, not orphan it")
	assert.Nil(t, recordFor(t, repoID, "direct-repo-gone-kill"),
		"the settled kill must delete the session row")
}

// TestKillSession_DirectRepoGoneProbeUnknownRefusesUnchanged: an origin probe
// that cannot answer is permission in neither direction. The direct kill must
// be refused before its tombstone, with the archive, the session row, and the
// record-free relocation state all unchanged — never admitted on the strength
// of an absent record.
func TestKillSession_DirectRepoGoneProbeUnknownRefusesUnchanged(t *testing.T) {
	manager, repoID, _, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-probe-unknown")
	// The origin path still exists, so only Git can answer whether it is a
	// repository — and an unexecutable git is an unknown answer, not proof.
	t.Setenv("PATH", t.TempDir())

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-probe-unknown", RepoID: repoID})
	require.Error(t, err, "an unanswerable origin probe must refuse the direct kill")
	assert.ErrorContains(t, err, "cannot establish origin repo state")
	assert.False(t, inst.UserKilled(), "the refusal must land before the kill tombstone")
	assert.True(t, exists(archivedPath), "a refused kill must leave the archive intact")
	record := recordFor(t, repoID, "direct-probe-unknown")
	require.NotNil(t, record, "a refused kill must keep the session row")
	assert.Nil(t, record.Worktree.RelocationRecovery,
		"a refused kill must not change the record-free relocation state")
}

// TestKillSession_ArchivedRepoPresentKillStaysOrdinary guards the gate's
// non-trigger: with the origin repo present and answering, an archived kill
// takes the ordinary teardown exactly as before and leaves nothing behind.
func TestKillSession_ArchivedRepoPresentKillStaysOrdinary(t *testing.T) {
	manager, repoID, _, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-repo-present")

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-repo-present", RepoID: repoID})
	require.NoError(t, err)
	assert.False(t, exists(archivedPath),
		"an ordinary archived kill still removes the archived worktree")
	assert.Nil(t, recordFor(t, repoID, "direct-repo-present"),
		"the settled kill must delete the session row")
}

// TestKillSession_ArchivedOriginVanishesAfterAdmissionRetainsRecord closes the
// admission-to-teardown interval (#3278 review): the gate's probe answered
// PRESENT, the tombstone committed, and the origin was then deleted before
// teardown. Ordinary cleanup must not settle against the missing origin and
// orphan the archive; the teardown-boundary recheck reports unknown, the
// tombstoned record is retained as the retry handle, and killing again
// authorizes identity-qualified cleanup and consumes the archive.
func TestKillSession_ArchivedOriginVanishesAfterAdmissionRetainsRecord(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-origin-vanishes")
	restoreSeam := session.SetBeforeKillTeardownOriginRecheckForTest(func() {
		require.NoError(t, os.RemoveAll(repoPath),
			"delete the origin between admission and the teardown boundary")
	})
	t.Cleanup(restoreSeam)

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-origin-vanishes", RepoID: repoID})
	require.Error(t, err,
		"an origin that vanishes after admission must not settle an ordinary cleanup")
	assert.ErrorContains(t, err, "kill the session again")
	assert.True(t, inst.UserKilled(), "the kill is committed; the retained record is its retry handle")
	assert.True(t, exists(archivedPath), "the archive must survive the refused ordinary cleanup")
	require.NotNil(t, recordFor(t, repoID, "direct-origin-vanishes"),
		"the tombstoned record must be retained for the retry")

	restoreSeam()
	_, err = manager.KillSession(KillSessionRequest{Title: "direct-origin-vanishes", RepoID: repoID})
	require.NoError(t, err,
		"killing again must authorize identity-qualified cleanup against the now-gone origin")
	assert.False(t, exists(archivedPath))
	assert.Nil(t, recordFor(t, repoID, "direct-origin-vanishes"))
}

// TestKillSession_DirectRepoGoneReplacedArchiveRefused: authorization derives
// from the current pathname occupant, so it must demand the archive's own
// creation-time evidence — the linked-worktree pointer — before authorizing
// deletion (#3278 review). An unrelated directory created at the archived path
// after the archive was moved away must be refused, not deleted.
func TestKillSession_DirectRepoGoneReplacedArchiveRefused(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-replaced-archive")
	require.NoError(t, os.RemoveAll(repoPath))
	require.NoError(t, os.RemoveAll(archivedPath))
	replacement := filepath.Join(archivedPath, "user-data")
	require.NoError(t, os.MkdirAll(replacement, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(replacement, "important.txt"),
		[]byte("not a worktree"), 0o644))

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-replaced-archive", RepoID: repoID})
	require.Error(t, err,
		"a pathname occupant without the archive's worktree pointer must not be authorized for deletion")
	assert.ErrorContains(t, err, "refusing to authorize repo-gone cleanup")
	assert.False(t, inst.UserKilled(), "the refusal must land before the kill tombstone")
	assert.True(t, exists(filepath.Join(replacement, "important.txt")),
		"the replacement directory must be left untouched")
}

// TestKillSession_ArchivedDirectoryAlreadyGoneStaysKillable: a user who
// manually deleted the archived directory must still be able to kill the stale
// row (#3278 review). There is nothing to authorize and nothing to orphan, so
// the gate steps aside and ordinary cleanup settles the absent path — with the
// origin present, and with the origin gone too.
func TestKillSession_ArchivedDirectoryAlreadyGoneStaysKillable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		removeRepo bool
	}{
		{name: "origin present", removeRepo: false},
		{name: "origin gone", removeRepo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title := "direct-archive-gone-" + map[bool]string{false: "present", true: "gone"}[tc.removeRepo]
			manager, repoID, repoPath, inst, archivedPath := archivedRecordFreeInstance(t, title)
			require.NoError(t, os.RemoveAll(archivedPath))
			if tc.removeRepo {
				require.NoError(t, os.RemoveAll(repoPath))
			}

			inst.SetBackend(&session.LocalBackend{})
			_, err := manager.KillSession(KillSessionRequest{Title: title, RepoID: repoID})
			require.NoError(t, err,
				"an absent archived directory has nothing to orphan; the stale row must stay killable")
			assert.Nil(t, recordFor(t, repoID, title), "the settled kill must delete the session row")
		})
	}
}

// TestFinishUserKill_RepoGoneAfterTombstoneConverges: a tombstoned kill whose
// origin disappeared after commit must converge through the automatic retry
// (#3278 review). finishUserKill re-derives the missing cleanup authorization
// exactly like an explicit kill, so the poll's retry consumes the archive
// instead of repeating the teardown-boundary refusal forever.
func TestFinishUserKill_RepoGoneAfterTombstoneConverges(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-finish-kill")
	restoreSeam := session.SetBeforeKillTeardownOriginRecheckForTest(func() {
		require.NoError(t, os.RemoveAll(repoPath),
			"delete the origin between admission and the teardown boundary")
	})
	t.Cleanup(restoreSeam)

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-finish-kill", RepoID: repoID})
	require.Error(t, err, "the vanished origin must refuse the first teardown")
	require.True(t, inst.UserKilled(), "the kill must be committed for the retry loop to own")
	require.True(t, exists(archivedPath))
	restoreSeam()

	manager.finishUserKill(repoID, inst)
	assert.False(t, exists(archivedPath),
		"the automatic retry must authorize identity-qualified cleanup and consume the archive")
	assert.Nil(t, recordFor(t, repoID, "direct-finish-kill"),
		"the converged retry must delete the session row")
}

// TestKillSession_ArchivedGhostRepoGoneRefused: an archived row that cannot be
// materialized into an Instance has no runtime to build cleanup authorization
// with, so a record-free ghost with a gone origin must be refused before the
// tombstone (#3278 review) — ordinary ghost cleanup would settle its answered
// missing-origin failures and orphan the archive.
func TestKillSession_ArchivedGhostRepoGoneRefused(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-ghost-gone")
	require.NoError(t, os.RemoveAll(repoPath))
	ghostFor(t, manager, repoID, "direct-ghost-gone")

	_, err := manager.KillSession(KillSessionRequest{Title: "direct-ghost-gone", RepoID: repoID})
	require.Error(t, err, "an archived record-free ghost with a gone origin must be refused")
	assert.ErrorContains(t, err, "identity-qualified cleanup cannot be authorized")
	assert.False(t, inst.UserKilled(), "the refusal must land before the kill tombstone")
	assert.True(t, exists(archivedPath), "the refused ghost kill must leave the archive intact")
	require.NotNil(t, recordFor(t, repoID, "direct-ghost-gone"),
		"the refused ghost kill must keep the row as the archive's handle")
}

// TestKillSession_ArchivedGhostDirectoryAlreadyGoneStaysKillable: with both
// the archived directory and the origin gone, there is nothing to orphan and
// the ghost guard must step aside (#3278 review) — ordinary ghost cleanup
// settles the absent path and the stale row is cleared instead of becoming
// permanently undeletable.
func TestKillSession_ArchivedGhostDirectoryAlreadyGoneStaysKillable(t *testing.T) {
	manager, repoID, repoPath, _, archivedPath :=
		archivedRecordFreeInstance(t, "direct-ghost-archive-gone")
	require.NoError(t, os.RemoveAll(repoPath))
	require.NoError(t, os.RemoveAll(archivedPath))
	ghostFor(t, manager, repoID, "direct-ghost-archive-gone")

	_, err := manager.KillSession(KillSessionRequest{Title: "direct-ghost-archive-gone", RepoID: repoID})
	require.NoError(t, err,
		"an absent archived directory has nothing to orphan; the stale ghost row must stay killable")
	assert.Nil(t, recordFor(t, repoID, "direct-ghost-archive-gone"),
		"the settled ghost kill must delete the session row")
}

// TestKillSession_ArchivedGhostOriginVanishesAfterAdmissionRetained: the ghost
// guard's admission probe is point-in-time, so an origin deleted between
// admission and the worktree-cleanup boundary must be caught by the boundary
// re-probe (#3278 review): the teardown reports unknown, the tombstoned row is
// retained, and the follow-up kill is refused by the guard with the archive
// intact — never settled over answered missing-origin failures.
func TestKillSession_ArchivedGhostOriginVanishesAfterAdmissionRetained(t *testing.T) {
	manager, repoID, repoPath, _, archivedPath :=
		archivedRecordFreeInstance(t, "direct-ghost-vanishes")
	ghostFor(t, manager, repoID, "direct-ghost-vanishes")
	previousCleanup := ghostCleanupWorktree
	ghostCleanupWorktree = func(
		data *session.InstanceData, title string, checkpoint func(*session.InstanceData) error,
	) (sessiongit.CleanupState, error, <-chan error) {
		if title == "direct-ghost-vanishes" {
			require.NoError(t, os.RemoveAll(repoPath),
				"delete the origin between ghost admission and the cleanup boundary")
		}
		return previousCleanup(data, title, checkpoint)
	}
	t.Cleanup(func() { ghostCleanupWorktree = previousCleanup })

	_, err := manager.KillSession(KillSessionRequest{Title: "direct-ghost-vanishes", RepoID: repoID})
	require.Error(t, err,
		"an origin that vanishes after ghost admission must not settle an ordinary cleanup")
	assert.ErrorContains(t, err, "cleanup boundary")
	assert.True(t, exists(archivedPath), "the archive must survive the refused ghost cleanup")
	require.NotNil(t, recordFor(t, repoID, "direct-ghost-vanishes"),
		"the tombstoned ghost row must be retained")

	_, err = manager.KillSession(KillSessionRequest{Title: "direct-ghost-vanishes", RepoID: repoID})
	require.Error(t, err, "the follow-up kill must be refused by the ghost admission guard")
	assert.ErrorContains(t, err, "identity-qualified cleanup cannot be authorized")
	assert.True(t, exists(archivedPath), "the archive must remain intact after the refused retry")
}

// TestKillSession_ArchivedGhostGenericStallRepoGoneRefused: an
// identity-unknown cleanup_stalled record is process-epoch state that restart
// normalization deliberately drops, leaving the worktree effectively
// record-free — so an archived ghost carrying one must get the record-free
// guards (#3278 review). Testing the raw persisted pointer instead skipped
// them all, and an origin-gone kill settled ordinary git failures and orphaned
// the archive while deleting the row.
func TestKillSession_ArchivedGhostGenericStallRepoGoneRefused(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-ghost-generic-stall")
	data := inst.ToInstanceData()
	data.Worktree.RelocationRecovery = &session.GitWorktreeRelocationRecoveryData{
		State: sessiongit.RelocationRecoveryCleanupStalled,
	}
	require.NoError(t, persistInstanceData(repoID, data))
	require.NoError(t, os.RemoveAll(repoPath))
	ghostFor(t, manager, repoID, "direct-ghost-generic-stall")

	_, err := manager.KillSession(KillSessionRequest{Title: "direct-ghost-generic-stall", RepoID: repoID})
	require.Error(t, err,
		"a generic-stall archived ghost with a gone origin must be refused, not settled over answered failures")
	assert.ErrorContains(t, err, "identity-qualified cleanup cannot be authorized")
	assert.True(t, exists(archivedPath), "the refused kill must leave the archive intact")
	require.NotNil(t, recordFor(t, repoID, "direct-ghost-generic-stall"),
		"the refused kill must keep the row as the archive's handle")
}

// TestKillSession_DirectRepoGoneForeignWorktreeRefused: a still-live linked
// worktree of another repository parked at the archived path must never be
// deleted on the strength of this session's record (#3278 review): its gitdir
// pointer resolves to metadata that still exists, which the gone origin's
// archive cannot have.
func TestKillSession_DirectRepoGoneForeignWorktreeRefused(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-foreign-worktree")
	require.NoError(t, os.RemoveAll(repoPath))

	// Build a second, still-live repository with a linked worktree and park
	// that worktree at the archived path.
	foreignRepo := filepath.Join(t.TempDir(), "foreign-repo")
	require.NoError(t, exec.Command("git", "init", "-b", "main", foreignRepo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(foreignRepo, "keep.txt"), []byte("x"), 0o644))
	require.NoError(t, exec.Command("git", "-C", foreignRepo, "add", "keep.txt").Run())
	require.NoError(t, exec.Command("git", "-C", foreignRepo,
		"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init").Run())
	foreignWorktree := filepath.Join(t.TempDir(), "foreign-wt")
	require.NoError(t, exec.Command("git", "-C", foreignRepo,
		"worktree", "add", "-b", "wt", foreignWorktree).Run())
	require.NoError(t, os.RemoveAll(archivedPath))
	require.NoError(t, exec.Command("git", "-C", foreignRepo,
		"worktree", "move", foreignWorktree, archivedPath).Run())

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-foreign-worktree", RepoID: repoID})
	require.Error(t, err,
		"a live foreign worktree at the archived path must not be authorized for deletion")
	assert.ErrorContains(t, err, "belongs to a live repository")
	assert.False(t, inst.UserKilled(), "the refusal must land before the kill tombstone")
	assert.True(t, exists(filepath.Join(archivedPath, "keep.txt")),
		"the foreign worktree must be left untouched")
}

// TestKillSession_ArchivedSiblingWorktreeAtPathRefused: `git worktree remove
// -f` trusts registration alone, so a different worktree of the same
// still-present origin parked at the archived path would be accepted and
// deleted, dirty changes included (#3278 review). The registered-only cleanup
// must prove the registration carries THIS session's branch before the
// destructive remove, and refuse otherwise with the record retained.
func TestKillSession_ArchivedSiblingWorktreeAtPathRefused(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-sibling-parked")

	// Move the real archive aside, drop its stale registration, and park a
	// sibling worktree (different branch, dirty) at the archived path.
	require.NoError(t, os.RemoveAll(archivedPath))
	require.NoError(t, exec.Command("git", "-C", repoPath, "worktree", "prune").Run())
	siblingPath := filepath.Join(t.TempDir(), "sibling-wt")
	require.NoError(t, exec.Command("git", "-C", repoPath,
		"worktree", "add", "-b", "sibling-branch", siblingPath).Run())
	require.NoError(t, os.WriteFile(filepath.Join(siblingPath, "dirty-sibling.txt"),
		[]byte("uncommitted sibling work"), 0o644))
	require.NoError(t, exec.Command("git", "-C", repoPath,
		"worktree", "move", siblingPath, archivedPath).Run())

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-sibling-parked", RepoID: repoID})
	require.Error(t, err,
		"a sibling worktree registered at the archived path must not be deleted on this session's kill")
	assert.ErrorContains(t, err, "not provably this session's worktree")
	assert.True(t, exists(filepath.Join(archivedPath, "dirty-sibling.txt")),
		"the sibling's dirty work must be left untouched")
	require.NotNil(t, recordFor(t, repoID, "direct-sibling-parked"),
		"the refused kill must retain the tombstoned record")
}

// TestKillSession_ArchivedStaleRegistrationReplacementRefused: a worktree
// registration is repo-side metadata that keeps reporting the recorded path
// and branch after the worktree was moved aside — git merely marks the entry
// prunable — so the listing is stale evidence about the occupant (#3278
// review). A plain directory at the archived path must be refused before
// anything destructive touches it, on the occupant's own missing linkage.
func TestKillSession_ArchivedStaleRegistrationReplacementRefused(t *testing.T) {
	manager, repoID, _, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-stale-registration")

	// Move the archive aside WITHOUT telling git (registration stays), and
	// put an unrelated directory at the registered path.
	require.NoError(t, os.Rename(archivedPath, archivedPath+"-moved-aside"))
	replacement := filepath.Join(archivedPath, "unrelated")
	require.NoError(t, os.MkdirAll(replacement, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(replacement, "data.txt"),
		[]byte("not the archive"), 0o644))

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-stale-registration", RepoID: repoID})
	require.Error(t, err,
		"a stale registration must not vouch for an occupant that lacks the worktree linkage")
	assert.ErrorContains(t, err, "does not carry this worktree's linkage")
	assert.True(t, exists(filepath.Join(replacement, "data.txt")),
		"the replacement must be left untouched")
	require.NotNil(t, recordFor(t, repoID, "direct-stale-registration"),
		"the refused kill must retain the record")
}

// TestKillSession_ArchivedGhostGenericStallBoundaryRefusalDoesNotLatch: when a
// normalized generic-stall ghost is refused at the cleanup boundary, the raw
// persisted cleanup_stalled pointer must not be reread as evidence of a
// running descriptor worker (#3278 review) — that installed the
// restart-required process latch over a worker that never ran, blocking every
// explicit retry. The retry must instead reach the admission guard's ordinary
// refusal.
func TestKillSession_ArchivedGhostGenericStallBoundaryRefusalDoesNotLatch(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-ghost-stall-boundary")
	data := inst.ToInstanceData()
	data.Worktree.RelocationRecovery = &session.GitWorktreeRelocationRecoveryData{
		State: sessiongit.RelocationRecoveryCleanupStalled,
	}
	require.NoError(t, persistInstanceData(repoID, data))
	ghostFor(t, manager, repoID, "direct-ghost-stall-boundary")
	previousCleanup := ghostCleanupWorktree
	ghostCleanupWorktree = func(
		d *session.InstanceData, title string, checkpoint func(*session.InstanceData) error,
	) (sessiongit.CleanupState, error, <-chan error) {
		if title == "direct-ghost-stall-boundary" {
			require.NoError(t, os.RemoveAll(repoPath),
				"delete the origin between ghost admission and the cleanup boundary")
		}
		return previousCleanup(d, title, checkpoint)
	}
	t.Cleanup(func() { ghostCleanupWorktree = previousCleanup })

	_, err := manager.KillSession(KillSessionRequest{Title: "direct-ghost-stall-boundary", RepoID: repoID})
	require.Error(t, err, "the vanished origin must refuse the boundary")
	assert.True(t, exists(archivedPath))

	_, err = manager.KillSession(KillSessionRequest{Title: "direct-ghost-stall-boundary", RepoID: repoID})
	require.Error(t, err, "the retry must be refused by the admission guard, not a stale process latch")
	assert.NotContains(t, err.Error(), "restart the daemon",
		"no descriptor worker ran, so no restart-required latch may be installed")
	assert.ErrorContains(t, err, "identity-qualified cleanup cannot be authorized")
	assert.True(t, exists(archivedPath), "the archive must remain intact")
}

// TestKillSession_ArchivedForeignRepoWorktreeAtPathRefused: a genuine linked
// worktree of a DIFFERENT live repository parked at the archived path
// satisfies the generic pointer shape while the recorded origin's stale
// registration still reports the expected path and branch (#3278 review). The
// occupant check must bind the pointer to THIS origin's metadata and refuse
// before anything destructive touches the foreign checkout's processes.
func TestKillSession_ArchivedForeignRepoWorktreeAtPathRefused(t *testing.T) {
	manager, repoID, _, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-foreign-live-worktree")

	// Move the archive aside WITHOUT telling git (stale registration stays),
	// then park a real worktree of a different live repository at the path.
	require.NoError(t, os.Rename(archivedPath, archivedPath+"-moved-aside"))
	foreignRepo := filepath.Join(t.TempDir(), "foreign-live-repo")
	require.NoError(t, exec.Command("git", "init", "-b", "main", foreignRepo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(foreignRepo, "keep.txt"), []byte("x"), 0o644))
	require.NoError(t, exec.Command("git", "-C", foreignRepo, "add", "keep.txt").Run())
	require.NoError(t, exec.Command("git", "-C", foreignRepo,
		"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init").Run())
	foreignWorktree := filepath.Join(t.TempDir(), "foreign-live-wt")
	require.NoError(t, exec.Command("git", "-C", foreignRepo,
		"worktree", "add", "-b", "wt", foreignWorktree).Run())
	require.NoError(t, exec.Command("git", "-C", foreignRepo,
		"worktree", "move", foreignWorktree, archivedPath).Run())

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-foreign-live-worktree", RepoID: repoID})
	require.Error(t, err,
		"a foreign repository's live worktree at the archived path must be refused")
	assert.ErrorContains(t, err, "belongs to a different repository")
	assert.True(t, exists(filepath.Join(archivedPath, "keep.txt")),
		"the foreign worktree must be left untouched")
	require.NotNil(t, recordFor(t, repoID, "direct-foreign-live-worktree"),
		"the refused kill must retain the record")
}

// TestKillSession_ArchivedSeparateGitDirKillStaysOrdinary: a `git init
// --separate-git-dir` origin redirects `<repo>/.git` to its real metadata
// directory, and the occupant binding must compare against that resolved root
// (#3278 review) — comparing against the redirect file's path refused every
// ordinary kill of such an archived session, retaining it forever.
func TestKillSession_ArchivedSeparateGitDirKillStaysOrdinary(t *testing.T) {
	manager, repoID, _ := newStatusTestManager(t)
	root := t.TempDir()
	sepRepo := filepath.Join(root, "sep-repo")
	sepMeta := filepath.Join(root, "sep-meta")
	require.NoError(t, exec.Command("git", "init", "-b", "main",
		"--separate-git-dir", sepMeta, sepRepo).Run())
	require.NoError(t, os.WriteFile(filepath.Join(sepRepo, "f.txt"), []byte("x"), 0o644))
	require.NoError(t, exec.Command("git", "-C", sepRepo, "add", "f.txt").Run())
	require.NoError(t, exec.Command("git", "-C", sepRepo,
		"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init").Run())

	inst, _ := registerArchivable(t, manager, repoID, sepRepo, "direct-separate-gitdir")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{
		Title: "direct-separate-gitdir", RepoID: repoID,
	})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()

	inst.SetBackend(&session.LocalBackend{})
	_, err = manager.KillSession(KillSessionRequest{Title: "direct-separate-gitdir", RepoID: repoID})
	require.NoError(t, err,
		"an ordinary archived kill must work for a separate-git-dir origin")
	assert.False(t, exists(archivedPath), "the archived worktree must be removed")
	assert.Nil(t, recordFor(t, repoID, "direct-separate-gitdir"),
		"the settled kill must delete the session row")
}

// TestKillSession_ArchivedLegacyNoBranchStaysKillable: old archives persisted
// no branch, and constructing an impossible "refs/heads/" expectation made
// every origin-present kill of such a row tombstone and retain forever (#3278
// review). Ownership for legacy rows rests on the exact occupant/backpointer
// binding, which needs no branch.
func TestKillSession_ArchivedLegacyNoBranchStaysKillable(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "direct-legacy-branchless")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{
		Title: "direct-legacy-branchless", RepoID: repoID,
	})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()

	// Model the legacy persisted shape: no branch on record.
	legacyGw, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, archivedPath, "direct-legacy-branchless", "", "", false, false,
	)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(legacyGw)

	inst.SetBackend(&session.LocalBackend{})
	_, err = manager.KillSession(KillSessionRequest{Title: "direct-legacy-branchless", RepoID: repoID})
	require.NoError(t, err, "a legacy archive with no persisted branch must stay killable")
	assert.False(t, exists(archivedPath), "the archived worktree must be removed")
	assert.Nil(t, recordFor(t, repoID, "direct-legacy-branchless"),
		"the settled kill must delete the session row")
}

// TestKillSession_DirectRepoGoneStalledIdentityPersistsAndReclaims: a bounded
// identity probe that times out during the direct-kill claim must leave a
// durable stalled fence — not an in-memory-only record a crash forgets — and a
// later kill must reclaim that fence and complete the repo-gone cleanup instead
// of leaving the session unkillable (#3278 review).
func TestKillSession_DirectRepoGoneStalledIdentityPersistsAndReclaims(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-stalled-identity")
	require.NoError(t, os.RemoveAll(repoPath))
	restoreIdentity := sessiongit.SetRelocationIdentityErrorForTest(
		archivedPath, context.DeadlineExceeded)
	t.Cleanup(restoreIdentity)

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-stalled-identity", RepoID: repoID})
	require.Error(t, err, "an unresolvable archived identity must refuse the kill")
	assert.ErrorContains(t, err, "could not be resolved for cleanup authorization")
	assert.False(t, inst.UserKilled(), "the refusal must land before the kill tombstone")
	assert.True(t, exists(archivedPath))
	record := recordFor(t, repoID, "direct-stalled-identity")
	require.NotNil(t, record, "the refused kill must keep the session row")
	recovery := record.Worktree.RelocationRecovery
	require.NotNil(t, recovery, "the failed bounded identity probe must leave a durable fence")
	assert.Equal(t, sessiongit.RelocationRecoveryStalled, recovery.State)

	restoreIdentity()
	_, err = manager.KillSession(KillSessionRequest{Title: "direct-stalled-identity", RepoID: repoID})
	require.NoError(t, err,
		"a later kill must reclaim the stalled fence and authorize repo-gone cleanup")
	assert.False(t, exists(archivedPath))
	assert.Nil(t, recordFor(t, repoID, "direct-stalled-identity"))
}

// TestKillSession_StalledAlternateReclaimVerifiesSelectedPath: a stalled
// record can carry an identity-qualified alternate from an interrupted
// relocation. When the reclaim selects that alternate — the primary is gone —
// the pointer verification and the authorization must follow the path the
// claim actually chose, not the pre-claim snapshot (#3278 review), or a valid
// archive at the alternate is refused over the obsolete primary's absence.
func TestKillSession_StalledAlternateReclaimVerifiesSelectedPath(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "direct-stalled-alternate")
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	gw, err := inst.GetGitWorktree()
	require.NoError(t, err)
	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{
		Title: "direct-stalled-alternate", RepoID: repoID,
	})
	require.NoError(t, err)
	archivedPath := inst.GetWorktreePath()
	require.NoError(t, os.RemoveAll(repoPath))

	// Model an interrupted move: the archive now lives at the alternate and
	// the recorded primary is gone; the stalled record carries the alternate's
	// identity.
	movedPath := archivedPath + "-moved"
	require.NoError(t, os.Rename(archivedPath, movedPath))
	info, err := os.Stat(movedPath)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.NoError(t, gw.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryStalled,
		AlternatePath: movedPath,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}))
	require.NoError(t, persistInstanceData(repoID, inst.ToInstanceData()))

	inst.SetBackend(&session.LocalBackend{})
	_, err = manager.KillSession(KillSessionRequest{Title: "direct-stalled-alternate", RepoID: repoID})
	require.NoError(t, err,
		"the reclaim selects the alternate, and authorization must follow that path")
	assert.False(t, exists(movedPath),
		"the kill must consume the archive at the path the claim selected")
	assert.Nil(t, recordFor(t, repoID, "direct-stalled-alternate"),
		"the settled kill must delete the session row")
}

// TestKillSession_DirectRepoGoneStagingPersistFailureStaysRetryable: when the
// direct-kill producer's FIRST durable write (the claim_stale staging fence)
// fails, nothing durable exists — so the in-memory fence must be un-staged, or
// the same-process retry is stranded: the gate skips its producer over the
// leftover claim_stale, admission refuses, and no tombstone means no finisher
// (#3278 review). The retry must re-derive authorization from scratch.
func TestKillSession_DirectRepoGoneStagingPersistFailureStaysRetryable(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-staging-persist")
	require.NoError(t, os.RemoveAll(repoPath))

	diskFull := errors.New("forced staging persistence failure")
	previousPersist := testHookPersistInstanceData
	failed := false
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "direct-staging-persist" && data.Worktree.RelocationRecovery != nil && !failed {
			failed = true
			return diskFull
		}
		return nil
	}
	t.Cleanup(func() { testHookPersistInstanceData = previousPersist })

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-staging-persist", RepoID: repoID})
	require.Error(t, err, "the failed staging write must refuse the kill")
	assert.ErrorContains(t, err, diskFull.Error())
	assert.False(t, inst.UserKilled(), "the refusal must land before the kill tombstone")
	assert.True(t, exists(archivedPath))

	_, err = manager.KillSession(KillSessionRequest{Title: "direct-staging-persist", RepoID: repoID})
	require.NoError(t, err,
		"the same-process retry must re-derive authorization after the staging write failure")
	assert.False(t, exists(archivedPath))
	assert.Nil(t, recordFor(t, repoID, "direct-staging-persist"))
}

// TestKillSession_StalledFenceOverAbsentArchiveClearsAndSettles: an
// identity-unknown stalled fence whose archived directory was then manually
// removed guards nothing, and both kill's and restore's claims wrap the same
// ENOENT — so without clearing it the row is permanently undeletable (#3278
// review). The kill must clear the fence durably and settle the absent path.
func TestKillSession_StalledFenceOverAbsentArchiveClearsAndSettles(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath :=
		archivedRecordFreeInstance(t, "direct-stalled-absent")
	require.NoError(t, os.RemoveAll(repoPath))
	restoreIdentity := sessiongit.SetRelocationIdentityErrorForTest(
		archivedPath, context.DeadlineExceeded)
	t.Cleanup(restoreIdentity)

	inst.SetBackend(&session.LocalBackend{})
	_, err := manager.KillSession(KillSessionRequest{Title: "direct-stalled-absent", RepoID: repoID})
	require.Error(t, err, "the stalled fence must first be established")
	record := recordFor(t, repoID, "direct-stalled-absent")
	require.NotNil(t, record)
	require.NotNil(t, record.Worktree.RelocationRecovery)
	require.Equal(t, sessiongit.RelocationRecoveryStalled, record.Worktree.RelocationRecovery.State)

	restoreIdentity()
	require.NoError(t, os.RemoveAll(archivedPath),
		"remove the archived directory the stalled fence was guarding")

	_, err = manager.KillSession(KillSessionRequest{Title: "direct-stalled-absent", RepoID: repoID})
	require.NoError(t, err,
		"a stalled fence over a conclusively absent archive must clear and let the kill settle")
	assert.Nil(t, recordFor(t, repoID, "direct-stalled-absent"),
		"the settled kill must delete the permanently-stuck row")
}
