package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// holdBranchInArchivedWorktree reproduces what archiving leaves behind (#2013):
// a worktree on `branch`, MOVED into the AF home's archive layout and still
// registered with git, so the branch stays checked out under a path no session
// record points at. It returns the archived worktree path.
func holdBranchInArchivedWorktree(t *testing.T, repoPath, branch, archiveName string) string {
	t.Helper()
	live := filepath.Join(t.TempDir(), "live")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, live).CombinedOutput()
	require.NoError(t, err, string(out))

	archived := filepath.Join(t.TempDir(), archiveName+" (archived)")
	out, err = exec.Command("git", "-C", repoPath, "worktree", "move", live, archived).CombinedOutput()
	require.NoError(t, err, string(out))
	return archived
}

// TestNextAvailableTitle_SkipsBranchHeldByArchivedWorktree is the #2091
// regression lock at the production call site: the derived-title walk that every
// session-creating scheduled task goes through (taskrun passes TitleBase, which
// routes here).
//
// The rot: a recurring task creates "sweep", archives it, and the archived
// worktree keeps branch <prefix>sweep checked out. Nothing in the title walk
// consulted git, so the next run happily handed back a title whose branch a
// registered worktree already held — and `git worktree add` then failed hard,
// every run, forever.
//
// The seeded worktrees deliberately have NO session records: that is the field
// shape (#2091 shows archived worktrees whose rows had been renamed out from
// under them), and it is the case only git can answer.
func TestNextAvailableTitle_SkipsBranchHeldByArchivedWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle("sweep"), "sweep")
	holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle("sweep-2"), "sweep-2")

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, "sweep", "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()
	require.NoError(t, err)

	assert.Equal(t, "sweep-3", title,
		"the walk must skip every suffix an archived worktree still holds")

	// Not merely unheld on paper — the worktree the create exists to build
	// actually gets built under the resolved name.
	dest := filepath.Join(t.TempDir(), "run")
	out, addErr := exec.Command("git", "-C", repoPath, "worktree", "add", "-b",
		manager.branchForTitle(title), dest).CombinedOutput()
	require.NoError(t, addErr, "resolved title %q must be usable: %s", title, string(out))
}

// TestNextAvailableTitle_UncontestedNameKeepsBareForm is the no-regression side:
// consulting git must not push an uncontested name off its bare form.
func TestNextAvailableTitle_UncontestedNameKeepsBareForm(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, "sweep", "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()

	require.NoError(t, err)
	assert.Equal(t, "sweep", title)
}

// TestNextAvailableTitle_LongTitleWalkConverges is the #2528 P3-b regression at
// the daemon call site. A long base title whose derived branch is already held
// used to make the walk NON-CONVERGENT: branch truncation collapsed every
// suffixed rung ("base-2", "base-3", …) to the SAME branch as the held base, so
// the walk skipped all 10,000 rungs under m.mu and failed with "could not find an
// available title". Bounding the base so the suffix survives lets it resolve at a
// low rung.
func TestNextAvailableTitle_LongTitleWalkConverges(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	base := strings.Repeat("a", 300)
	// Hold the base's derived branch exactly as an archived session would, so the
	// bare base (rung 1) is taken and the walk must find a distinct suffix.
	holdBranchInArchivedWorktree(t, repoPath, manager.branchForTitle(base), "held")

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, base, "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()
	require.NoError(t, err, "the walk must converge, not exhaust 10,000 rungs")

	require.NotEqual(t, base, title, "the held bare base must be skipped")
	assert.NotEqual(t, manager.branchForTitle(base), manager.branchForTitle(title),
		"the resolved title must derive a DISTINCT branch, not one truncation re-collides")

	// The resolved branch is actually creatable — not merely unheld on paper.
	dest := filepath.Join(t.TempDir(), "run")
	out, addErr := exec.Command("git", "-C", repoPath, "worktree", "add", "-b",
		manager.branchForTitle(title), dest).CombinedOutput()
	require.NoError(t, addErr, "resolved title %q must be usable: %s", title, string(out))
}

// TestNextAvailableTitle_ExistingBranchIsNotAHold keeps the new check as narrow
// as the failure it fixes. AF reuses an existing branch when one matches the
// derived name (setupFromExistingBranch), so a branch that merely EXISTS must
// not cost the session its bare title — only a branch some worktree has
// checked out is unusable.
func TestNextAvailableTitle_ExistingBranchIsNotAHold(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	out, err := exec.Command("git", "-C", repoPath, "branch", manager.branchForTitle("sweep")).CombinedOutput()
	require.NoError(t, err, string(out))

	manager.mu.Lock()
	title, err := manager.nextAvailableTitleLocked(repoID, repoPath, "sweep", "claude", runtimeNamespaceLocalTmux, nil)
	manager.mu.Unlock()

	require.NoError(t, err)
	assert.Equal(t, "sweep", title)
}
