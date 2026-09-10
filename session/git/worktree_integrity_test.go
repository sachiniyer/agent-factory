package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestInspectWorktreeIntegrityRejectsEmptyHeadReflog(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	integrityGit(t, repo, "config", "core.logAllRefUpdates", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644))
	integrityGit(t, repo, "add", "--all")
	integrityGit(t, repo, "commit", "-q", "-m", "base")

	_, err := InspectWorktreeIntegrity(repo)
	require.Error(t, err, "an absent local HEAD reflog is unknown, not evidence of a clean checkout")
	assert.Contains(t, err.Error(), "HEAD reflog")
}

func TestInspectWorktreeIntegrityRejectsHeadChangeDuringProbe(t *testing.T) {
	binDir := t.TempDir()
	statusSeen := filepath.Join(t.TempDir(), "status-seen")
	fakeGit := filepath.Join(binDir, "git")
	oldHead := "1111111111111111111111111111111111111111"
	newHead := "2222222222222222222222222222222222222222"
	script := fmt.Sprintf(`#!/bin/sh
if [ "$3" = "status" ]; then
	if [ -e %q ]; then
		printf '%%s\n' '# branch.oid %s' '# branch.head shared'
	else
		: > %q
		printf '%%s\n' '# branch.oid %s' '# branch.head shared'
	fi
	exit 0
fi
if [ "$3" = "log" ]; then
	printf '%%s\n' %s
	exit 0
fi
exit 2
`, statusSeen, newHead, statusSeen, oldHead, newHead)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := InspectWorktreeIntegrity(t.TempDir())
	require.Error(t, err, "a HEAD change between status and reflog is an incomplete observation, not takeover evidence")
	assert.False(t, got.HeadMovedWithoutReflog)
}

func TestWorktreeBranchAtPathReadsMovedUnrepairedCheckout(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	integrityGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	oldPath := filepath.Join(filepath.Dir(repo), "old")
	newPath := filepath.Join(filepath.Dir(repo), "new")
	integrityGit(t, repo, "worktree", "add", "-q", "-b", "topic", oldPath)
	require.NoError(t, os.Rename(oldPath, newPath),
		"leave Git's registration at the old path, matching an interrupted repair")

	branch, detached, err := WorktreeBranchAtPath(newPath)
	require.NoError(t, err)
	assert.Equal(t, "topic", branch)
	assert.False(t, detached)
	bindings, err := WorktreeBranchBindings(repo)
	require.NoError(t, err)
	for _, binding := range bindings {
		assert.NotEqual(t, newPath, binding.Path,
			"precondition: the repository-wide listing still names the old path")
	}
}

func TestWorktreeBranchAtPathDistinguishesLegalDetachedName(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, exec.Command("git", "init", "-q", repo).Run())
	integrityGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	integrityGit(t, repo, "branch", "-m", "(detached)")

	branch, detached, err := WorktreeBranchAtPath(repo)
	require.NoError(t, err)
	assert.Equal(t, "(detached)", branch)
	assert.False(t, detached)

	integrityGit(t, repo, "checkout", "-q", "--detach")
	branch, detached, err = WorktreeBranchAtPath(repo)
	require.NoError(t, err)
	assert.Empty(t, branch)
	assert.True(t, detached)
}

func TestParseIntegrityStatusRejectsMissingBranchObservation(t *testing.T) {
	_, err := parseIntegrityStatus("# branch.oid 0123456789012345678901234567890123456789\n")
	require.Error(t, err, "missing branch metadata is unknown, not a detached checkout")
	assert.Contains(t, err.Error(), "branch")
}

func TestInspectWorktreeIntegrityContextCancelsOutstandingProbe(t *testing.T) {
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "entered")
	fakeGit := filepath.Join(binDir, "git")
	script := fmt.Sprintf("#!/bin/sh\n: > %q\nexec sleep 30\n", marker)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := InspectWorktreeIntegrityContext(ctx, t.TempDir())
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake Git probe never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Git probe did not return within its WaitDelay bound")
	}
}

func TestIntegrityProbeCancellationKillsDescendants(t *testing.T) {
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	entered := filepath.Join(t.TempDir(), "entered")
	fakeGit := filepath.Join(binDir, "git")
	script := fmt.Sprintf(`#!/bin/sh
sleep 30 &
child=$!
printf '%%s' "$child" > %q
: > %q
wait "$child"
`, pidFile, entered)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runIntegrityGit(ctx, t.TempDir(), "status")
		done <- err
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(entered)
		return err == nil
	}, time.Second, 5*time.Millisecond, "fake Git probe never started")
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Git probe did not return within its WaitDelay bound")
	}

	rawPID, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	require.Eventually(t, func() bool {
		err := syscall.Kill(childPID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, time.Second, 10*time.Millisecond,
		"canceling an integrity probe must kill the helper process, not only the direct git child")
}

func TestIntegrityProbeReapsPipeHolderAfterGitExits(t *testing.T) {
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	fakeGit := filepath.Join(binDir, "git")
	script := fmt.Sprintf(`#!/bin/sh
(trap '' HUP; sleep 30) &
child=$!
printf '%%s' "$child" > %q
printf '%%s\n' '# branch.oid 1111111111111111111111111111111111111111' '# branch.head shared'
exit 0
`, pidFile)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := runIntegrityGit(context.Background(), t.TempDir(), "status")
	require.NoError(t, err, "a pipe holder after successful Git exit must not discard its complete output")
	rawPID, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	require.Eventually(t, func() bool {
		err := syscall.Kill(childPID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, time.Second, 10*time.Millisecond,
		"a helper that kept Git's output pipe open must not survive the bounded probe")
}

func TestIntegrityProbeReapsPipeHolderAfterNonzeroGitExit(t *testing.T) {
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child-pid")
	fakeGit := filepath.Join(binDir, "git")
	script := fmt.Sprintf(`#!/bin/sh
(trap '' HUP; sleep 30) &
child=$!
printf '%%s' "$child" > %q
printf 'probe failed\n' >&2
exit 7
`, pidFile)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := runIntegrityGit(context.Background(), t.TempDir(), "status")
	require.Error(t, err)
	rawPID, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	require.Eventually(t, func() bool {
		err := syscall.Kill(childPID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, time.Second, 10*time.Millisecond,
		"a helper holding Git's output pipe must be killed after Git exits nonzero")
}
