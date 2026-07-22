package daemon

import (
	"fmt"
	"os/exec"
	"testing"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReserveCreate_HeldArchivedBranchRefusesBeforeRename is the #2127
// regression lock.
//
// The rot: `af sessions create --name foo` over an archived "foo" renamed the
// archived session to "foo (archived)" to free the TITLE, then failed anyway at
// `git worktree add` — because archiving relocates a worktree rather than
// removing it (#2013), so the archived session still had <prefix>foo checked
// out, and the new session derives exactly that branch. The user was left with
// a create that failed AND an archived session renamed out of the way for it:
// the state reserveCreate's own admission comment promises never to produce ("a
// refusal never leaves an archived session renamed for a create that then did
// not happen").
//
// This seeds the shape archiving actually produces — branch still held — and
// pins the honest failure: refuse up front, rename nothing, and say what is
// blocking and how to clear it.
func TestReserveCreate_HeldArchivedBranchRefusesBeforeRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")
	branch := manager.branchForTitle("foo")

	// The precondition the old fixture never established: git positively reports
	// the archived worktree as holding the branch this create would derive.
	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	require.Contains(t, held, branch,
		"the archived worktree must still hold the derived branch; without that this test proves nothing")

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		release()
	}

	require.Error(t, err, "the create must be refused: freeing the name would not free the branch it needs")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")

	// Actionable: name the blocking branch, where it is held, and a way out.
	msg := err.Error()
	assert.Contains(t, msg, branch, "the error must name the branch that blocks the create")
	assert.Contains(t, msg, "af sessions kill", "the error must offer a way to release the branch")
	assert.Contains(t, msg, "different name", "the error must offer the non-destructive alternative")

	// The invariant, asserted on every surface that could carry the rename:
	// in-memory title, manager key, on-disk record, and the archive directory.
	assert.Equal(t, "foo", archived.Title, "the archived session must keep its name after a refusal")
	manager.mu.Lock()
	_, origKeyed := manager.instances[daemonInstanceKey(repoID, "foo")]
	_, renamedKeyed := manager.instances[daemonInstanceKey(repoID, "foo (archived)")]
	manager.mu.Unlock()
	assert.True(t, origKeyed, "the archived row must stay keyed under its original name")
	assert.False(t, renamedKeyed, "no row may be keyed under the disambiguated name")

	rec := recordFor(t, repoID, "foo")
	require.NotNil(t, rec, "the archived record must survive the refusal under its original name")
	assert.Equal(t, id, rec.ID, "the archived session must be untouched, stable id and all")
	assert.Nil(t, recordFor(t, repoID, "foo (archived)"), "no renamed record may be persisted for a refused create")

	origDir, derr := archivedWorktreePath(repoID, "foo")
	require.NoError(t, derr)
	newDir, derr := archivedWorktreePath(repoID, "foo (archived)")
	require.NoError(t, derr)
	assert.True(t, exists(origDir), "the archived worktree must stay at its original path")
	assert.False(t, exists(newDir), "no relocation may have happened")

	// And the archived session is still restorable — the refusal cost the user
	// nothing, which is the difference between this and the pre-fix behavior.
	_, _, rerr := manager.RestoreArchived(RestoreArchivedRequest{Title: "foo", RepoID: repoID})
	require.NoError(t, rerr, "the untouched archived session must still restore under its own name")
}

// TestReserveCreate_HeldBranchWithoutArchivedCollisionIsUnguarded keeps the
// guard as narrow as the invariant it protects. A held branch with NO archived
// session to rename triggers no rename, so there is nothing to leave orphaned —
// and refusing there would turn the guard into a general branch-availability
// gate over every explicit title, refusing creates this issue is not about.
// Such a create proceeds and fails at `git worktree add` exactly as before.
func TestReserveCreate_HeldBranchWithoutArchivedCollisionIsUnguarded(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)

	// A worktree holding <prefix>orphan with no session record pointing at it —
	// #2091's field shape, and no archived row named "orphan" anywhere.
	holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle("orphan"), "orphan")

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "orphan", Program: "claude"})
	require.NoError(t, err, "with no archived collision there is no rename to protect; the guard must stay out of the way")
	defer release()

	assert.Equal(t, "orphan", title)
	assert.Nil(t, renamed, "nothing was renamed")
}

// TestReserveCreate_UnprobableBranchHoldsStillReuse is the three-valued case.
//
// git.BranchesHeldByWorktrees returns nil holds when it could not ask (its
// documented contract), and "I could not ask" is not "the branch is held". A
// guard that conflated them would refuse a legitimate reuse on the strength of
// an answer git never gave — the fabricated-negative failure this repo keeps
// paying for. On an unanswerable probe the create proceeds exactly as it did
// pre-guard: if the branch really is held, `git worktree add` refuses it loudly
// and destroys nothing.
//
// Forced through the branchesHeldByWorktrees seam rather than by breaking the
// repo, which reserveCreate needs readable to reach this code at all.
func TestReserveCreate_UnprobableBranchHoldsStillReuse(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Branch genuinely HELD: the guard would refuse this if it could probe, so a
	// completed reuse here is proof the failed probe alone let it through.
	seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")
	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	require.Contains(t, held, manager.branchForTitle("foo"))

	probed := false
	prev := branchesHeldByWorktrees
	branchesHeldByWorktrees = func(string) (map[string]string, error) {
		probed = true
		return nil, fmt.Errorf("forced branch-hold probe failure (#2127)")
	}
	t.Cleanup(func() { branchesHeldByWorktrees = prev })

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		defer release()
	}

	require.True(t, probed, "the seam never fired; this test did not exercise the failed-probe path")
	require.NoError(t, err, "an unanswerable probe must not refuse the create — nil holds is not 'held'")
	assert.Equal(t, "foo", title)
	require.NotNil(t, renamed, "the reuse proceeds as it did pre-guard")
	assert.Equal(t, "foo (archived)", renamed.Title)
}

// TestReserveCreate_InPlaceCreateIgnoresBranchHold: `--here` attaches to the
// repo's own working tree at ITS current branch (NewGitWorktreeInPlace) and
// never derives <prefix><title>, so a hold on that branch cannot block it. The
// guard must not refuse a create over a branch it will not use.
func TestReserveCreate_InPlaceCreateIgnoresBranchHold(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, Title: "foo", Program: "claude", InPlace: true,
	})
	require.NoError(t, err, "an --here create derives no branch from the title, so a hold on that branch is not its problem")
	defer release()

	assert.Equal(t, "foo", title)
	require.NotNil(t, renamed, "the archived name is still freed for an --here create")
	assert.Equal(t, "foo (archived)", renamed.Title)
	assert.NotNil(t, recordFor(t, repoID, "foo (archived)"))
}

// TestReserveCreate_ArchivedBranchHoldSurvivesTheRename is the fact the guard
// exists because of, pinned directly rather than inferred: renaming an archived
// session does NOT release its branch. If this ever stops being true — option 1
// or 2 on #2127 — the guard becomes dead weight and this test says so by
// failing, rather than the guard quietly refusing creates that would now work.
func TestReserveCreate_ArchivedBranchHoldSurvivesTheRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	branch := manager.branchForTitle("foo")

	// Re-attach the branch so the rename runs with it held, the way archiving
	// leaves it. (Seeded freed first only so the guard lets the rename happen —
	// this test is about what the rename does to the hold, not about the guard.)
	archivedPath, err := archivedWorktreePath(repoID, "foo")
	require.NoError(t, err)
	out, err := exec.Command("git", "-C", archivedPath, "checkout", branch).CombinedOutput()
	require.NoError(t, err, string(out))

	manager.mu.Lock()
	diskData, lerr := loadRepoInstanceData(repoID)
	require.NoError(t, lerr)
	renamed, err := manager.renameArchivedForReuseLocked(repoID, repoPath, "foo", "claude", runtimeNamespaceLocalTmux, &diskData)
	manager.mu.Unlock()
	require.NoError(t, err)
	require.NotNil(t, renamed, "the rename must have run for this test to say anything")

	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	holder, stillHeld := held[branch]
	assert.True(t, stillHeld,
		"renaming an archived session frees its TITLE but not its BRANCH — the premise of #2127's guard")
	assert.Contains(t, holder, "(archived)",
		"the branch follows the relocated archived worktree to its new path")
}
