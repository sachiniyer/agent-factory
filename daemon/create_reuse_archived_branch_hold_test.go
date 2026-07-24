package daemon

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReserveCreate_HeldArchivedBranchIsReclaimed is the #2127 durable fix — the
// FULL-RECLAIM path.
//
// The rot: `af sessions create --name foo` over an archived "foo" renamed the
// archived session to "foo (archived)" to free the TITLE, then failed anyway at
// `git worktree add` — because archiving relocates a worktree rather than
// removing it (#2013), so the archived session still had <prefix>foo checked
// out, and the new session derives exactly that branch.
//
// #2129 shipped the interim guard: refuse up front so the create at least stops
// leaving an archived session renamed for a create that never happened. That made
// the failure honest, but reuse-archived-name still never WORKED for a local
// session — it refused every time, because per #2013 the branch is always held.
//
// The durable fix moves the BRANCH aside with the title, so the reclaim is
// complete: the create succeeds, and the archived session keeps its work under a
// name that matches its new title.
func TestReserveCreate_HeldArchivedBranchIsReclaimed(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")
	branch := manager.branchForTitle("foo")

	// The precondition: git positively reports the archived worktree as holding
	// the branch this create would derive. Without it this test proves nothing —
	// it would be exercising the freed-branch path, which already worked.
	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	require.Contains(t, held, branch,
		"the archived worktree must still hold the derived branch; without that this test proves nothing")

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		defer release()
	}

	require.NoError(t, err, "the create must now SUCCEED: the archived branch is moved aside with the title")
	assert.Equal(t, "foo", title, "the new session takes the reclaimed title verbatim")
	require.NotNil(t, renamed, "the archived session must have been renamed to free the name")
	assert.Equal(t, "foo (archived)", renamed.Title)

	// The reclaim, and the whole point: the branch the new session derives is FREE.
	// This is the assertion that fails against the interim guard and against the
	// original bug alike — the first because no create happens at all, the second
	// because the branch stays held.
	held, herr = sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	assert.NotContains(t, held, branch,
		"the derived branch must be released, or the create this title was granted for still cannot build its worktree")

	// And it is free in the way that matters: the real `git worktree add` the
	// create runs now succeeds on it. Only running the add can tell a released
	// branch from one that merely looks released.
	dest := filepath.Join(t.TempDir(), "new-session")
	out, addErr := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, dest).CombinedOutput()
	require.NoError(t, addErr, "the granted title %q must be usable by the create it was granted for: %s", title, string(out))
	out, rmErr := exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", dest).CombinedOutput()
	require.NoError(t, rmErr, string(out))

	// The archived session did not lose anything: its work moved to a branch that
	// matches its new title, its record followed, and it still restores.
	archivedBranch := manager.branchForTitle("foo (archived)")
	assert.Equal(t, archivedBranch, archived.GetBranch(),
		"the archived session's recorded branch must move with it, or its record and git disagree")
	held, herr = sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	holder, stillHeld := held[archivedBranch]
	assert.True(t, stillHeld, "the archived worktree must still be on its (renamed) branch")
	assert.Contains(t, holder, "(archived)", "and that worktree is the relocated archive")

	rec := recordFor(t, repoID, "foo (archived)")
	require.NotNil(t, rec, "the renamed archived record must be persisted under its new title")
	assert.Equal(t, id, rec.ID, "the archived session keeps its stable id across the reclaim")
	assert.Equal(t, archivedBranch, rec.Branch, "the persisted branch must match the renamed one")

	_, _, rerr := manager.RestoreArchived(RestoreArchivedRequest{Title: "foo (archived)", RepoID: repoID})
	require.NoError(t, rerr, "the archived session must still restore after its branch moved")
}

// The reclaim is not unconditional, and this is the case it declines: a PUBLISHED
// branch. Renaming one desyncs its local name from the remote it tracks and from
// any open PR, which is a worse outcome than not creating the session — so the
// #2129 refusal stays in force, and says so.
func TestReserveCreate_PublishedArchivedBranchStillRefuses(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")
	branch := manager.branchForTitle("foo")

	// A real upstream, built without a network: a bare repo added as a remote,
	// pushed to, and set as the branch's tracking ref.
	remote := t.TempDir()
	out, err := exec.Command("git", "-C", remote, "init", "-q", "--bare", "-b", "main").CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", repoPath, "remote", "add", "origin", remote).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", repoPath, "push", "-q", "origin", branch).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", repoPath, "branch", "--set-upstream-to=origin/"+branch, branch).CombinedOutput()
	require.NoError(t, err, string(out))

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		release()
	}

	require.Error(t, err, "a published archived branch must not be renamed behind the user's back")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")
	msg := err.Error()
	assert.Contains(t, msg, branch, "the error must name the branch that blocks the create")
	assert.Contains(t, msg, "published", "the error must say WHY the branch could not be moved aside")
	assert.Contains(t, msg, "af sessions kill", "the error must offer a way to release the branch")

	// And nothing moved: the refusal is still side-effect-free.
	assert.Equal(t, "foo", archived.Title, "the archived session must keep its name after a refusal")
	assert.Nil(t, recordFor(t, repoID, "foo (archived)"), "no renamed record may be persisted for a refused create")
	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	assert.Contains(t, held, branch, "the published branch must be exactly where it was")
}

// The reclaim's target name must be genuinely free, not merely un-checked-out
// (the P3 on #2465). If a plain branch already holds the disambiguated name
// "<prefix>foo-archived", `git branch -m` would refuse the rename — so the reclaim
// must decline and let the create refuse up front, rather than promise a name it
// cannot deliver and then fail mid-rename with a misleading message.
func TestReserveCreate_ReclaimDeclinesWhenTargetBranchExists(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")
	branch := manager.branchForTitle("foo")

	// The name the reclaim would rename onto already exists as a plain, idle branch
	// — checked out nowhere, so the checked-out map does not see it.
	target := manager.branchForTitle("foo (archived)")
	out, err := exec.Command("git", "-C", repoPath, "branch", target).CombinedOutput()
	require.NoError(t, err, string(out))

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		release()
	}

	require.Error(t, err, "the reclaim target is taken, so the create cannot succeed and must refuse up front")
	assert.NotContains(t, err.Error(), "failed to relocate its worktree",
		"a taken branch name must not be reported as a worktree-relocation failure")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")

	// Side-effect-free: neither the archived branch nor the pre-existing target moved.
	assert.Equal(t, "foo", archived.Title, "the archived session must keep its name after a refusal")
	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	assert.Contains(t, held, branch, "the archived branch must be exactly where it was")
	out, err = exec.Command("git", "-C", repoPath, "branch", "--list", target).CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), target, "the pre-existing target branch must be untouched")
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

// TestReserveCreate_ArchivedBranchMovesWithTheRename pins the rename's own
// contract, one level below reserveCreate: renaming an archived session now
// releases its branch as well as its title.
//
// This test previously asserted the OPPOSITE — that the branch survived the
// rename — because that was the fact #2129's guard existed because of. Its own
// comment said it would fail if option 1 or 2 on #2127 ever landed, "rather than
// the guard quietly refusing creates that would now work". Option 2 landed; this
// is that prediction being honoured rather than a lock being weakened.
func TestReserveCreate_ArchivedBranchMovesWithTheRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")
	branch := manager.branchForTitle("foo")

	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	require.Contains(t, held, branch, "the archived worktree must hold the branch for this test to say anything")

	manager.mu.Lock()
	diskData, lerr := loadRepoInstanceData(repoID)
	require.NoError(t, lerr)
	renamed, err := manager.renameArchivedForReuseLocked(repoID, repoPath, "foo", "claude", runtimeNamespaceLocalTmux, &diskData)
	manager.mu.Unlock()
	require.NoError(t, err)
	require.NotNil(t, renamed, "the rename must have run for this test to say anything")

	held, herr = sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	assert.NotContains(t, held, branch,
		"renaming an archived session must now free its BRANCH as well as its title (#2127)")

	archivedBranch := manager.branchForTitle("foo (archived)")
	holder, stillHeld := held[archivedBranch]
	assert.True(t, stillHeld, "the archived worktree must be on the renamed branch, not detached")
	assert.Contains(t, holder, "(archived)",
		"the branch follows the relocated archived worktree to its new path")
	assert.Equal(t, archivedBranch, renamed.Branch,
		"the event published for the rename must carry the branch it actually has")
}
