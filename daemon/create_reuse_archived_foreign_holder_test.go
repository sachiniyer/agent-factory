package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvedPath is the test's own canonicalization, deliberately NOT
// pathutil.ResolveForCompare: computing the expectation with the same helper the
// production code compares with would pass even if that helper were wrong. Every
// path here exists, so a plain EvalSymlinks is enough.
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	if path == "" {
		// "no path" resolves to itself: filepath.EvalSymlinks("") answers ".",
		// which would print as a plausible-looking path in a failure diff.
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err, "resolve %s", path)
	return resolved
}

// TestReserveCreate_ForeignlyHeldBranchIsNotReclaimed is #3404: the reclaim must
// not move a branch that belongs to somebody ELSE'S worktree.
//
// #2127 lets a create take an archived session's title by moving the archived
// session's branch aside with it. The hold check asked only whether SOME worktree
// had <prefix>foo checked out — never whether the archived one did — and those
// come apart in a shape the code itself documents as supported: detach the
// archived worktree and its record still CACHES <prefix>foo, while any other
// worktree in the repo can be sitting on that branch for real.
//
// The reclaim then fired on the strength of the stranger's hold and renamed the
// stranger's branch: a live worktree went from <prefix>foo to <prefix>foo-archived
// mid-session, silently, with `git branch -m` dragging its HEAD along. Nobody
// asked for that, nothing announced it, and the session that "reclaimed" the name
// had no relationship to the worktree that paid for it.
//
// So: refuse the create — the branch really is in the way and af may not move it —
// and leave the other worktree exactly as it was found.
func TestReserveCreate_ForeignlyHeldBranchIsNotReclaimed(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	branch := manager.branchForTitle("foo")
	archivedBranch := manager.branchForTitle("foo (archived)")

	// The stranger: a live worktree checked out on the very branch the archived
	// record still names, at a path with nothing to do with the archive. Adding it
	// AFTER the detach is what makes this the #3404 shape rather than the ordinary
	// one — the archived worktree released the branch, and someone else took it.
	other := filepath.Join(t.TempDir(), "other-worktree")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", other, branch).CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", other).Run()
	})

	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	holderPath := held[branch]
	require.Equal(t, resolvedPath(t, other), resolvedPath(t, holderPath),
		"precondition: the branch must be held by the OTHER worktree, not the archived one; without that this test proves nothing")
	require.NotEqual(t, resolvedPath(t, other), resolvedPath(t, archived.GetWorktreePath()),
		"precondition: the holder must genuinely be a different worktree from the archived session's")

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "foo", Program: "claude"})
	if release != nil {
		release()
	}

	// The damage first, because it is what the bug DOES: whatever the create
	// decided, the stranger's worktree must be exactly as it was found. These are
	// asserts rather than requires so a run against the unguarded code reports the
	// rename it performed instead of stopping at the outcome check below.
	held, herr = sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	assert.Equal(t, resolvedPath(t, other), resolvedPath(t, held[branch]),
		"the unrelated worktree must still hold the branch it started on — renaming it out from under a live session is the bug")
	assert.NotContains(t, held, archivedBranch,
		"no worktree may have been moved onto the reclaim's target name; that branch is the rename this must not perform")

	// git's own answer, not just af's map: the other worktree's HEAD still names
	// the branch. `git branch -m` drags a holding worktree's HEAD with it, so this
	// is what the user in that worktree would actually see.
	head, headErr := exec.Command("git", "-C", other, "rev-parse", "--abbrev-ref", "HEAD").Output()
	require.NoError(t, headErr)
	assert.Equal(t, branch, strings.TrimSpace(string(head)),
		"the other worktree must still be on its own branch")

	// And the outcome: the create cannot succeed — the branch it would derive is
	// checked out elsewhere, so `git worktree add` would refuse it — and refusing
	// up front is the #2129 contract. What must NOT happen is taking the name by
	// rewriting a worktree af has no claim on.
	require.Error(t, err, "the derived branch is held by a worktree af may not touch, so the create must refuse rather than reclaim")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")
	msg := err.Error()
	assert.Contains(t, msg, branch, "the refusal must name the branch that blocks the create")
	assert.Contains(t, msg, holderPath, "the refusal must name the worktree actually holding it, or the user hunts the wrong session")

	// And the archived session is where it was: a refused create leaves no residue.
	assert.Equal(t, "foo", archived.Title, "the archived session must keep its name after a refusal")
	assert.Nil(t, recordFor(t, repoID, "foo (archived)"), "no renamed record may be persisted for a refused create")
	exists, err := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", "--quiet", archivedBranch).Output()
	assert.Error(t, err, "the reclaim's target branch must not exist at all: %s", strings.TrimSpace(string(exists)))
}

// The guard is about the HOLDER, not about detachment: an archived session that
// really is holding its own branch must still be reclaimable, or #3404's fix
// would have closed the leak by breaking the feature. TestReserveCreate_
// HeldArchivedBranchIsReclaimed covers the full success path; this pins the one
// predicate that changed, so a future tightening that makes it answer "no" for
// the archived worktree itself fails here with the reason named.
func TestReclaimArchivedBranch_ArchivedHolderStillQualifies(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")

	held, herr := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, herr)
	holder, ok := held[manager.branchForTitle("foo")]
	require.True(t, ok, "the archived worktree must still hold its branch (#2013 relocates rather than releases)")

	assert.True(t, archivedWorktreeHoldsBranch(archived, holder),
		"the archived session's own worktree must be recognized as the holder, or reuse-archived-name can never complete")

	manager.mu.Lock()
	candidate := manager.reclaimArchivedBranchLocked(repoPath, archived, "foo (archived)")
	manager.mu.Unlock()
	assert.Equal(t, manager.branchForTitle("foo (archived)"), candidate,
		"the reclaim must still offer the archived session's own branch a new name")
}

// Fail-closed, stated as its own case: an unresolvable holder is not a licence to
// rename. Both inputs are answers af might not have — git reporting no path, or an
// archived record with no worktree path of its own — and treating either as "ours"
// is how the leak above happened in the first place.
func TestArchivedWorktreeHoldsBranch_UnknownHolderDeclines(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")

	assert.False(t, archivedWorktreeHoldsBranch(archived, ""),
		"an empty holder is 'git named no path', which must never authorize a rename")
	assert.False(t, archivedWorktreeHoldsBranch(archived, filepath.Join(t.TempDir(), "somewhere-else")),
		"a path that is not the archived worktree must decline even when it does not exist")
	assert.True(t, archivedWorktreeHoldsBranch(archived, archived.GetWorktreePath()),
		"and the archived session's own path must still qualify, spelled exactly as af stores it")
}
