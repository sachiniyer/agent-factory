package daemon

import (
	"os/exec"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2415: reserveCreate ran the archived-name-reuse rename BEFORE
// validateTitleAvailableLocked, so a create could be refused for a reason the
// rename has no effect on — an orphan tmux session, the reserved "root" name, a
// hook slug another project owns — after the archived session had already been
// renamed to "<title> (archived)", its worktree physically relocated, its manager
// key changed, and its durable record rewritten. There is no rollback on that
// path, so the user was left with a permanently renamed archived session for a
// create that never happened: precisely the state reserveCreate's own admission
// comment promises it never produces, and the same invariant #2127 protects from
// the branch side.

// assertArchivedUntouched pins the invariant on every surface that could carry a
// rename: the in-memory title, the manager key, the durable record (identity
// included), and the archive directory on disk. A fix that reverted only some of
// them would leave the session inconsistent rather than intact, so they are
// checked together.
func assertArchivedUntouched(t *testing.T, manager *Manager, archived *session.Instance, repoID, title, id string) {
	t.Helper()
	renamedTitle := title + " (archived)"

	assert.Equal(t, title, archived.Title, "the archived session must keep its name after a refusal")

	manager.mu.Lock()
	_, origKeyed := manager.instances[daemonInstanceKey(repoID, title)]
	_, renamedKeyed := manager.instances[daemonInstanceKey(repoID, renamedTitle)]
	manager.mu.Unlock()
	assert.True(t, origKeyed, "the archived row must stay keyed under its original name")
	assert.False(t, renamedKeyed, "no row may be keyed under the disambiguated name")

	rec := recordFor(t, repoID, title)
	require.NotNil(t, rec, "the archived record must survive the refusal under its original name")
	assert.Equal(t, id, rec.ID, "the archived session must be untouched, stable id and all")
	assert.Nil(t, recordFor(t, repoID, renamedTitle), "no renamed record may be persisted for a refused create")

	origDir, derr := archivedWorktreePath(repoID, title)
	require.NoError(t, derr)
	newDir, derr := archivedWorktreePath(repoID, renamedTitle)
	require.NoError(t, derr)
	assert.True(t, exists(origDir), "the archived worktree must stay at its original path")
	assert.False(t, exists(newDir), "no relocation may have happened")

	// And the archived session is still restorable — the refusal cost the user
	// nothing, which is the difference between this and the pre-fix behavior.
	_, _, rerr := manager.RestoreArchived(RestoreArchivedRequest{Title: title, RepoID: repoID})
	require.NoError(t, rerr, "the untouched archived session must still restore under its own name")
}

// TestReserveCreate_OrphanTmuxRefusesBeforeArchivedRename is the #2415 headline
// case, and the one the report names: a real orphan tmux session holding the name
// the create wants.
//
// Freeing the TITLE does not kill that pane. validateTitleAvailableLocked probes
// for it and refuses — but it ran after renameArchivedForReuseLocked had already
// moved the archived session out of the way, so the user paid for a create that
// could never have succeeded.
//
// Seeded with the branch FREED so #2127's guard cannot fire first: without that
// this test would pass against the unfixed code for the wrong reason, proving
// nothing about the check under test.
func TestReserveCreate_OrphanTmuxRefusesBeforeArchivedRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")

	// The orphan: a tmux session under exactly the name this create would claim,
	// owned by no af record. TestMain sandboxes the package onto a private tmux
	// server, so this cannot touch a real one.
	orphan := tmux.SanitizedNameForRepo("foo", repoPath)
	out, err := exec.Command("tmux", "new-session", "-d", "-s", orphan, "sh").CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+orphan).Run() })

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		release()
	}

	require.Error(t, err, "the create must be refused: an orphan tmux session already holds the name")
	assert.Contains(t, err.Error(), "conflicting tmux session",
		"the refusal must be the orphan-tmux one, not some other failure")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")
	assertArchivedUntouched(t, manager, archived, repoID, "foo", id)
}

// TestReserveCreate_ReservedHookNameRefusesBeforeArchivedRename covers a second,
// entirely different reason the same way — which is the point. #2415 is not one
// missing case but an ordering defect: EVERY check living after the rename
// inherited it, which is why the fix is a split rather than a moved line.
//
// Here the blocker is a concurrent create holding the global hook slug. Hook names
// are shared across projects (the scripts receive them verbatim as --name), and
// findArchivedOnlyCollisionLocked — which decides whether to rename — consults
// reservedTitles and reservedTmuxNames but never reservedRemoteNames. So the
// rename went ahead and validateTitleAvailableLocked then refused, on a hold that
// was already in place before the rename touched anything.
//
// Hermetic: no tmux involved, so it pins the ordering rather than the mechanics of
// any one check.
func TestReserveCreate_ReservedHookNameRefusesBeforeArchivedRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")

	// A concurrent hook create in flight under the slug this create would claim.
	slug := session.Slugify("foo")
	manager.mu.Lock()
	manager.reservedRemoteNames[slug] = struct{}{}
	manager.mu.Unlock()

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, Title: "foo", Program: "claude", ForceRemote: true,
	})
	if release != nil {
		release()
	}

	require.Error(t, err, "the create must be refused: the hook name is reserved by a concurrent create")
	assert.Contains(t, err.Error(), "is already reserved",
		"the refusal must be the reserved-hook-name one, unchanged in wording by moving it earlier")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")
	assertArchivedUntouched(t, manager, archived, repoID, "foo", id)
}

// TestReserveCreate_UnclaimableGuardStaysOutOfTheWay keeps the guard as narrow as
// the invariant it protects, mirroring #2127's equivalent. With no archived
// collision there is no rename to protect, so the refusal must land exactly where
// it always did — same error, from validateTitleAvailableLocked — rather than
// moving earlier and changing behavior for creates this issue is not about.
func TestReserveCreate_UnclaimableGuardStaysOutOfTheWay(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)

	orphan := tmux.SanitizedNameForRepo("solo", repoPath)
	out, err := exec.Command("tmux", "new-session", "-d", "-s", orphan, "sh").CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+orphan).Run() })

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "solo", Program: "claude"})
	if release != nil {
		release()
	}
	require.Error(t, err, "an orphan tmux session still blocks a create with no archived collision")
	assert.Contains(t, err.Error(), "conflicting tmux session")
	assert.Nil(t, renamed)
}

// TestValidateTitleClaimable_IgnoresTheRowBeingRenamed is the other direction of
// the fix, and the regression it could most easily have introduced. The archived
// row is the claim the rename is about to release, so counting it against the new
// session would refuse a reuse that would have succeeded — turning a data-integrity
// fix into a feature regression.
//
// Exercised on the hook-slug scans, which are the only ones in the claimable half
// that read af's own records at all.
func TestValidateTitleClaimable_IgnoresTheRowBeingRenamed(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	archived, err := session.NewInstance(session.InstanceOptions{Title: "MyApp", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	hookRow := []session.InstanceData{{Title: "MyApp", Path: repoPath, BackendType: "remote"}}
	require.True(t, hookRow[0].IsRemoteHook(), "the fixture must actually be a hook row or this asserts nothing")

	err = manager.validateTitleClaimableLocked(repoID, repoPath, "MyApp", "claude", runtimeNamespaceRemoteHook, false, hookRow, archived)
	require.NoError(t, err,
		"the archived row being renamed must not be counted as the collision that blocks its own reuse")

	// Without the exclusion the very same row refuses — proof the scan really does
	// see it, so the NoError above is the exclusion working rather than a scan that
	// never fired.
	err = manager.validateTitleClaimableLocked(repoID, repoPath, "MyApp", "claude", runtimeNamespaceRemoteHook, false, hookRow, nil)
	require.Error(t, err, "an unignored hook row with the same slug must still refuse")
	assert.Contains(t, err.Error(), "already maps to hook name")
}
