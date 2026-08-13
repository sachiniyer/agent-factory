package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
