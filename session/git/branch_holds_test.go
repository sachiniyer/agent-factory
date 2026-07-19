package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archivedHoldRepo builds the exact shape #2091 rots on: a repo whose branches
// `foo` and `foo-2` are checked out by worktrees that have been MOVED out of
// their original location — what teardownArchive does when a session is archived
// (#2013 relocates the worktree and repairs its registration rather than
// removing it, so the archived session keeps its branch checked out).
//
// It returns the repo root and the archive directory holding the relocated
// worktrees.
func archivedHoldRepo(t *testing.T, heldBranches ...string) (repoRoot, archiveDir string) {
	t.Helper()
	sandboxHome(t)
	repoRoot = createGitRepo(t)
	runGitInPlaceTest(t, repoRoot, "commit", "--allow-empty", "-m", "init")

	archiveDir = filepath.Join(filepath.Dir(repoRoot), "archived")
	require.NoError(t, os.MkdirAll(archiveDir, 0755))

	for _, branch := range heldBranches {
		live := filepath.Join(filepath.Dir(repoRoot), "live-"+branch)
		runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", branch, live)
		// The archive move: relocate the directory, then repair the two-way
		// registration. The name mirrors the real archive layout, spaces and all.
		dest := filepath.Join(archiveDir, branch+" (archived)")
		runGitInPlaceTest(t, repoRoot, "worktree", "move", live, dest)
	}
	return repoRoot, archiveDir
}

// firstFreeSuffix walks the `<base>`, `<base>-2`, `<base>-3`, … ladder the
// session-title resolver walks, skipping every candidate whose branch a
// registered worktree already holds. It models the daemon's
// nextAvailableTitleLocked walk over the authoritative answer this file provides.
func firstFreeSuffix(t *testing.T, repoRoot, base string) string {
	t.Helper()
	held, err := BranchesHeldByWorktrees(repoRoot)
	require.NoError(t, err)
	for i := 1; i <= 10; i++ {
		candidate := base
		if i > 1 {
			candidate = base + "-" + strconv.Itoa(i)
		}
		if _, taken := held[candidate]; !taken {
			return candidate
		}
	}
	t.Fatalf("no free suffix for %q within 10 candidates", base)
	return ""
}

// TestBranchesHeldByWorktrees_ReportsArchivedHolds is the #2091 root-cause
// observation: `git branch` alone cannot answer "is this name usable" — every
// held branch is also just a branch — but `git worktree list --porcelain` names
// the worktree holding each one, including worktrees that have been moved into
// the archive.
func TestBranchesHeldByWorktrees_ReportsArchivedHolds(t *testing.T) {
	repoRoot, archiveDir := archivedHoldRepo(t, "foo", "foo-2")

	held, err := BranchesHeldByWorktrees(repoRoot)
	require.NoError(t, err)

	require.Contains(t, held, "foo")
	require.Contains(t, held, "foo-2")
	assert.Equal(t, filepath.Join(archiveDir, "foo (archived)"), held["foo"])
	assert.Equal(t, filepath.Join(archiveDir, "foo-2 (archived)"), held["foo-2"])
	// The next rung of the ladder is free, and nothing invented a hold for it.
	assert.NotContains(t, held, "foo-3")
}

// TestBranchesHeldByWorktrees_WalkSkipsArchivedSuffixes is the regression lock
// for #2091. A recurring task derives `<name>[-N]` afresh on every run; once its
// archived predecessors hold `foo` and `foo-2`, the walk must land on `foo-3`
// and that name must actually be usable. Before the fix the resolver consulted
// only session records, handed back a name an archived worktree still held, and
// `git worktree add` failed hard — the same failure asserted below for the held
// rungs.
func TestBranchesHeldByWorktrees_WalkSkipsArchivedSuffixes(t *testing.T) {
	repoRoot, archiveDir := archivedHoldRepo(t, "foo", "foo-2")

	// A held rung is genuinely unusable — this is the failure the daily task hit
	// every run, and the reason the walk cannot stop at a name `git branch`
	// alone would call taken-but-reusable.
	//
	// The assertion is on the SUBSTANCE, not on git's wording: git says "already
	// used by worktree at <path>" on some versions and "'<branch>' is already
	// checked out at <path>" on others. What every version does is refuse, and
	// name the worktree holding the branch — which is the fact the resolver
	// depends on.
	for _, held := range []string{"foo", "foo-2"} {
		out, err := runGitAllowFail(t, repoRoot, "worktree", "add",
			filepath.Join(t.TempDir(), "collide-"+held), held)
		require.Error(t, err, "expected %q to be unusable while an archived worktree holds it", held)
		assert.Contains(t, out, filepath.Join(archiveDir, held+" (archived)"),
			"expected the refusal to name the archived worktree holding %q, got: %s", held, out)
	}

	assert.Equal(t, "foo-3", firstFreeSuffix(t, repoRoot, "foo"))

	// The resolved name is not merely unheld on paper: the worktree it exists to
	// create actually gets created.
	dest := filepath.Join(t.TempDir(), "run")
	runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", "foo-3", dest)
	assert.DirExists(t, dest)
}

// TestBranchesHeldByWorktrees_NoCollisionKeepsBareName guards the other
// direction: adding the worktree-hold check must not push an uncontested name
// off its bare form. A repo with no session worktrees holds only its own
// checked-out branch.
func TestBranchesHeldByWorktrees_NoCollisionKeepsBareName(t *testing.T) {
	repoRoot, _ := archivedHoldRepo(t)

	held, err := BranchesHeldByWorktrees(repoRoot)
	require.NoError(t, err)
	assert.NotContains(t, held, "foo")

	assert.Equal(t, "foo", firstFreeSuffix(t, repoRoot, "foo"))
}

// TestBranchesHeldByWorktrees_UnheldExistingBranchStaysFree keeps the check
// narrow. AF deliberately REUSES an existing branch when one matches the derived
// name (setupFromExistingBranch), so a branch that merely exists must not read as
// taken — only a branch some worktree has CHECKED OUT is unusable.
func TestBranchesHeldByWorktrees_UnheldExistingBranchStaysFree(t *testing.T) {
	repoRoot, _ := archivedHoldRepo(t)
	runGitInPlaceTest(t, repoRoot, "branch", "foo")

	held, err := BranchesHeldByWorktrees(repoRoot)
	require.NoError(t, err)
	assert.NotContains(t, held, "foo", "an existing but unchecked-out branch is reusable, not held")
	assert.Equal(t, "foo", firstFreeSuffix(t, repoRoot, "foo"))
}

// TestBranchesHeldByWorktrees_NonRepoErrors pins the answer AF must not
// fabricate: a repo it cannot ask returns an error, never an empty "nothing is
// held" map that would read as a confident all-clear.
func TestBranchesHeldByWorktrees_NonRepoErrors(t *testing.T) {
	held, err := BranchesHeldByWorktrees(t.TempDir())
	require.Error(t, err)
	assert.Nil(t, held)

	held, err = BranchesHeldByWorktrees("")
	require.Error(t, err)
	assert.Nil(t, held)
}

func TestParseWorktreeBranchHolds(t *testing.T) {
	// A detached worktree contributes no hold, a bare main worktree has neither
	// HEAD nor branch, and a path with spaces (every archived worktree has one)
	// survives intact.
	porcelain := strings.Join([]string{
		"worktree /repos/main",
		"HEAD 1111111111111111111111111111111111111111",
		"branch refs/heads/master",
		"",
		"worktree /home/u/.agent-factory/archived/abc/Simplify Abstractions-2 (archived)",
		"HEAD 2222222222222222222222222222222222222222",
		"branch refs/heads/siyer/simplify-abstractions-2",
		"",
		"worktree /repos/detached",
		"HEAD 3333333333333333333333333333333333333333",
		"detached",
		"",
	}, "\n")

	held := parseWorktreeBranchHolds(porcelain)

	assert.Equal(t, map[string]string{
		"master": "/repos/main",
		"siyer/simplify-abstractions-2": "/home/u/.agent-factory/archived/abc/" +
			"Simplify Abstractions-2 (archived)",
	}, held)
}

// runGitAllowFail runs git and returns its combined output plus the error,
// instead of failing the test — for the assertions that a git command MUST fail.
func runGitAllowFail(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
