package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func integrityGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s: %s", cmd.String(), string(out))
	return string(out)
}

func worktreeIntegrityFixture(t *testing.T, files int) (repo, holder, sibling string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	for i := 0; i < files; i++ {
		path := filepath.Join(repo, fmt.Sprintf("file-%02d.txt", i))
		require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o644))
	}
	integrityGit(t, repo, "add", "--all")
	integrityGit(t, repo, "commit", "-q", "-m", "base")

	holder = filepath.Join(filepath.Dir(repo), "holder")
	sibling = filepath.Join(filepath.Dir(repo), "sibling")
	integrityGit(t, repo, "worktree", "add", "-q", "-b", "shared", holder, "HEAD")
	integrityGit(t, repo, "worktree", "add", "-q", "-b", "takeover", sibling, "HEAD")
	return repo, holder, sibling
}

func moveSharedBranchFromSibling(t *testing.T, holder, sibling string, files int) {
	t.Helper()
	// This is the #4092 door: ordinary checkout refuses a held branch. The
	// fixture opts through that guard explicitly so Git 2.43 and 2.55 construct
	// the same collided state that AF must detect.
	ordinary := exec.Command("git", "-C", sibling, "checkout", "shared")
	ordinary.Env = append(os.Environ(), "LC_ALL=C")
	out, err := ordinary.CombinedOutput()
	require.Error(t, err, "ordinary checkout must preserve Git's held-worktree refusal")
	assert.Contains(t, string(out), "already used by worktree")
	integrityGit(t, sibling, "checkout", "-q", "--ignore-other-worktrees", "-B", "shared", "shared")
	for i := 0; i < files; i++ {
		path := filepath.Join(sibling, fmt.Sprintf("file-%02d.txt", i))
		require.NoError(t, os.WriteFile(path, []byte("new\n"), 0o644))
	}
	integrityGit(t, sibling, "add", "--all")
	integrityGit(t, sibling, "commit", "-q", "-m", "advance shared elsewhere")
	assert.NotEqual(t, integrityGit(t, holder, "rev-parse", "HEAD@{0}"), integrityGit(t, holder, "rev-parse", "HEAD"),
		"precondition: the holder's worktree-local reflog must not record the sibling's branch move")
}

func TestInspectWorktreeIntegrityReportsMassRevertShape(t *testing.T) {
	_, holder, sibling := worktreeIntegrityFixture(t, 25)
	moveSharedBranchFromSibling(t, holder, sibling, 25)

	got, err := InspectWorktreeIntegrity(holder)
	require.NoError(t, err)
	assert.True(t, got.MassRevert, "25 staged paths with zero unstaged paths is the armed mass-revert shape")
	assert.Equal(t, 25, got.StagedPaths)
	assert.Zero(t, got.UnstagedPaths)
}

func TestInspectWorktreeIntegrityReportsHeadMovedWithoutLocalReflog(t *testing.T) {
	_, holder, sibling := worktreeIntegrityFixture(t, 1)
	moveSharedBranchFromSibling(t, holder, sibling, 1)

	got, err := InspectWorktreeIntegrity(holder)
	require.NoError(t, err)
	assert.True(t, got.HeadMovedWithoutReflog)
	assert.NotEmpty(t, got.HeadSHA)
	assert.NotEmpty(t, got.ReflogHeadSHA)
	assert.NotEqual(t, got.HeadSHA, got.ReflogHeadSHA)
	assert.False(t, got.MassRevert, "one moved path is below the deliberately narrow mass-revert threshold")
}

func TestInspectWorktreeIntegrityMassRevertRequiresMoreThanTwentyPaths(t *testing.T) {
	_, holder, sibling := worktreeIntegrityFixture(t, 20)
	moveSharedBranchFromSibling(t, holder, sibling, 20)

	got, err := InspectWorktreeIntegrity(holder)
	require.NoError(t, err)
	assert.Equal(t, 20, got.StagedPaths)
	assert.Zero(t, got.UnstagedPaths)
	assert.False(t, got.MassRevert)
	assert.True(t, got.HeadMovedWithoutReflog)
}

func TestInspectWorktreeIntegrityIgnoresOrdinaryUnstagedEdits(t *testing.T) {
	_, holder, _ := worktreeIntegrityFixture(t, 25)
	for i := 0; i < 25; i++ {
		path := filepath.Join(holder, fmt.Sprintf("file-%02d.txt", i))
		require.NoError(t, os.WriteFile(path, []byte("ordinary local edit\n"), 0o644))
	}

	got, err := InspectWorktreeIntegrity(holder)
	require.NoError(t, err)
	assert.False(t, got.MassRevert, "ordinary unstaged work must never trip the mass-revert detector")
	assert.Zero(t, got.StagedPaths)
	assert.Equal(t, 25, got.UnstagedPaths)
	assert.False(t, got.HeadMovedWithoutReflog)
}
