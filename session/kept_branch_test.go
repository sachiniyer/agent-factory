package session

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keptGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, string(out))
}

// #6233: the branch is read from the worktree's HEAD, not the recorded name.
func TestKeptBranchIsReadFromTheWorktreeHead(t *testing.T) {
	dir := t.TempDir()
	keptGit(t, dir, "init", "-q", "-b", "dev/beta")
	keptGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x")

	assert.False(t, keptBranchFor(dir, "dev/beta").Any(), "HEAD on the recorded branch: nothing extra is kept")

	keptGit(t, dir, "checkout", "-q", "-b", "feature/done-flag")
	k := keptBranchFor(dir, "dev/beta")
	assert.Equal(t, "feature/done-flag", k.Name)
	assert.Contains(t, k.Note(), "'feature/done-flag'")
	assert.Contains(t, k.Note(), "kept in the repo")

	keptGit(t, dir, "checkout", "-q", "--detach")
	assert.False(t, keptBranchFor(dir, "dev/beta").Any(), "detached HEAD names no branch")
}

func TestKeptBranchSaysWhenHeadCannotBeRead(t *testing.T) {
	k := keptBranchFor(t.TempDir(), "dev/beta") // not a repo
	assert.True(t, k.Unreadable)
	assert.Empty(t, k.Name)
	assert.Contains(t, k.Note(), "Could not read")
}

// A tag that shares the recorded branch's name must not make --short answer
// "heads/dev/beta": the af-owned branch is still not an extra kept branch.
func TestKeptBranchIgnoresATagSharingTheBranchName(t *testing.T) {
	dir := t.TempDir()
	keptGit(t, dir, "init", "-q", "-b", "dev/beta")
	keptGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x")
	keptGit(t, dir, "tag", "dev/beta")

	assert.False(t, keptBranchFor(dir, "dev/beta").Any(), "HEAD on the recorded branch: nothing extra is kept")
	keptGit(t, dir, "checkout", "-q", "-b", "feature/x")
	assert.Equal(t, "feature/x", keptBranchFor(dir, "dev/beta").Name)
}
