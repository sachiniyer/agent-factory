package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

// Regression coverage for the worktree-reaper inherited-cwd false positive: the
// daemon can be auto-started with a managed worktree as its cwd and never chdirs,
// so a git child it forks for an UNRELATED session inherits that worktree as its
// cwd during git's startup window (before `git -C path` takes effect). The reaper
// selects purely by /proc/<pid>/cwd and would SIGTERM (grace = 0) that unrelated
// git command during a concurrent reap of the inherited worktree.
//
// The fix sets cmd.Dir = path alongside `git -C path` so the child's cwd is the
// target from process start, removing the false positive at its source. The tests
// here verify that fix at three levels:
//
//  1. runGitCommandContext starts its git child cwd'd at the target path, not at
//     the inherited daemon cwd (the test process's own cwd).
//  2. runBoundedWorktreeGit does the same for the reset-path runner.
//  3. End-to-end: reaping the inherited worktree does not SIGTERM a git child the
//     runner started for a different session — the actual bug scenario.
//
// Why a fake `git` on PATH is the right seam: `git -C path` resolves the repo from
// `path`, so git's OWN output is invariant to the kernel cwd — a real git would
// print the same toplevel whether its cwd was inherited or set. The ONLY
// observable that distinguishes the fix from the bug is /proc/<pid>/cwd, which a
// fake `git` that runs `pwd` reports directly. This mirrors stallingGitOnPath and
// stallGitSubcommand, the package's existing fake-git-on-PATH pattern.

// fakeGitPrintsCwdOnPath puts a `git` earlier on PATH that prints its current
// working directory (the kernel cwd after exec, which is cmd.Dir when the runner
// sets it, or the inherited daemon cwd when it does not) and exits. The script
// ignores its arguments, so the runner's `git -C path <args>` invocation reaches
// the fake verbatim and `pwd` reports the kernel cwd rather than a -C-resolved
// repo toplevel.
//
// The directory the fake reports (got) is compared against the path the runner
// was asked to target (want): with cmd.Dir set, got == want; without it, got is the
// test process's cwd (the inherited daemon cwd), which is never want. That makes
// the assertion deterministic in both directions — the test fails the moment the
// fix is removed.
func fakeGitPrintsCwdOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\npwd\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRunGitCommandContext_ChildCwdIsPathNotInherited verifies the main git
// runner sets cmd.Dir = path so the git process's /proc/<pid>/cwd is the target
// path from process start. Without cmd.Dir the child inherits the daemon's cwd
// (modeled by the test process's own cwd) during its startup window and would be
// a false positive for the worktree writer-reaper's cwd match.
func TestRunGitCommandContext_ChildCwdIsPathNotInherited(t *testing.T) {
	// Canonical so `pwd` (which reports the resolved cwd) matches on macOS, where
	// t.TempDir() lives under /var -> /private/var.
	target := testguard.CanonicalTempDir(t)
	fakeGitPrintsCwdOnPath(t)

	g := &GitWorktree{}
	out, err := g.runGitCommandContext(context.Background(), target, "version")
	require.NoError(t, err)

	got := strings.TrimSpace(out)
	testCwd, _ := os.Getwd()
	require.NotEqual(t, testCwd, got,
		"the git child must not report the inherited (test-process) cwd; cmd.Dir must target the path")
	require.Equal(t, target, got,
		"the git child's cwd must be the target path (cmd.Dir = path), not the inherited daemon cwd")
}

// TestRunGitCommandContext_RelativePathResolvedAgainstDaemonLaunchCwd locks in
// the chdir interaction the #5206 fix introduced: runDaemon now chdirs to the AF
// home, so a relative `-C path` would resolve beneath the AF home instead of the
// spawner's cwd. chdirToNeutralHome captures the pre-chdir cwd via
// SetDaemonLaunchCwd, and the runner resolves a relative path against it before
// the existing-directory gate — so a restored session whose persisted path
// NewGitWorktreeFromStorage stored verbatim still targets the launch-cwd repo
// AND gets cmd.Dir (closing the inherited-cwd window for relative paths too).
//
// Deterministic in both directions: with the resolution the fake git reports the
// launch-cwd-resolved path (and cmd.Dir is set to it); without it a relative path
// stays relative, the IsAbs gate skips cmd.Dir, and the child reports the inherited
// (test-process) cwd.
func TestRunGitCommandContext_RelativePathResolvedAgainstDaemonLaunchCwd(t *testing.T) {
	launchCwd := testguard.CanonicalTempDir(t)
	target := testguard.CanonicalTempDir(t)
	rel := relPathFrom(t, launchCwd, target)
	fakeGitPrintsCwdOnPath(t)

	prev := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	t.Cleanup(func() { daemonLaunchCwd = prev })

	g := &GitWorktree{}
	out, err := g.runGitCommandContext(context.Background(), rel, "version")
	require.NoError(t, err)

	got := strings.TrimSpace(out)
	testCwd, _ := os.Getwd()
	require.NotEqual(t, testCwd, got,
		"a relative path resolved against the launch cwd must set cmd.Dir, not leave the child on the inherited (test-process) cwd")
	require.Equal(t, target, got,
		"a relative path must resolve against the daemon's launch cwd (%s) to %s, not the inherited or post-chdir cwd", launchCwd, target)
}

// relPathFrom returns a relative path naming target from base, or skips the test
// if the two do not share a common ancestor (t.TempDir siblings on the same
// volume always do).
func relPathFrom(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	require.NoError(t, err, "filepath.Rel(%s, %s)", base, target)
	return rel
}

// TestRunBoundedWorktreeGit_ChildCwdIsRepoRootNotInherited verifies the
// bounded reset-path runner sets cmd.Dir = repoRoot for the same reason. The
// reset path is not itself reachable by the bug (`af reset` stops the daemon
// before reaping), but the fix keeps the two runners consistent with the
// package convention (hooks.go sets cmd.Dir = run.worktreePath) and removes the
// inherited-cwd exposure should a future caller keep the daemon alive.
func TestRunBoundedWorktreeGit_ChildCwdIsRepoRootNotInherited(t *testing.T) {
	// The bounded runner does not exit instantly under the production 60s timeout
	// only because the fake git is fast; still, shorten the bound so a hung script
	// (a regression that stopped honouring cmd fast-exit) fails in milliseconds.
	shortenLocalTimeout(t, 5*time.Second)
	repoRoot := testguard.CanonicalTempDir(t)
	fakeGitPrintsCwdOnPath(t)

	out, err := runBoundedWorktreeGit(repoRoot, false, "rev-parse", "--show-toplevel")
	require.NoError(t, err, "runBoundedWorktreeGit: %s", string(out))

	got := strings.TrimSpace(string(out))
	testCwd, _ := os.Getwd()
	require.NotEqual(t, testCwd, got,
		"the bounded-runner git child must not report the inherited (test-process) cwd")
	require.Equal(t, repoRoot, got,
		"the bounded-runner git child's cwd must be repoRoot (cmd.Dir = repoRoot)")
}

// TestRunGitCommand_ReapedInheritedWorktreeSpareUnrelatedGitChild is the
// end-to-end guarantee: a git command the runner starts for a DIFFERENT session
// (target cwd) must survive a concurrent reap of the inherited worktree (the
// daemon's own cwd). This is the precise false positive the bug describes — the
// reaper's cwd match reads /proc/<pid>/cwd and, with the inherited cwd, would
// select the unrelated git child.
//
// It re-enters the test binary in a child whose cwd is the inherited worktree
// (modeling the daemon), so:
//   - With the fix (cmd.Dir = target): the fake git's cwd is `target`, which is NOT
//     under the reaped inherited worktree, so the reaper never selects it and the
//     child survives.
//   - Without the fix: the fake git inherits the child's cwd (the reaped worktree)
//     for its whole lifetime (it ignores `-C` and never chdirs), so the reaper
//     matches and SIGTERMs it with no grace — the child dies.
//
// Deterministic in both directions: the fake git ignores `-C`, so the inherited
// cwd persists for its entire run rather than only a startup window, which removes
// the timing dependency the production window would impose.
//
// The reaped path is a unique temp dir (the child's cwd), not the test process's
// own cwd, so the reap cannot touch any other process the host happens to have
// parked there — the same discipline the live reap tests (e.g.
// TestReapWorktreeWriters_DoesNotKillTmuxServer) follow.
func TestRunGitCommand_ReapedInheritedWorktreeSpareUnrelatedGitChild(t *testing.T) {
	const helperEnv = "AF_TEST_GIT_CWD_REAP"
	if os.Getenv(helperEnv) == "1" {
		// We are the "daemon": cwd is the inherited worktree (uniqueReaped), and a
		// fake git that sleeps is on PATH so it stays alive for the reap to observe.
		target := os.Getenv(helperEnv + "_TARGET")
		fakeDir := os.Getenv(helperEnv + "_FAKEDIR")
		pidFile := os.Getenv(helperEnv + "_PID")
		t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		inherited, err := os.Getwd()
		require.NoError(t, err)

		g := &GitWorktree{}
		done := make(chan error, 1)
		go func() {
			_, e := g.runGitCommand(target, "rev-parse", "--show-toplevel")
			done <- e
		}()

		// Wait for the fake git to have started and recorded its pid, then wait for
		// its cwd to be observable by proctree — the exact readiness gate the live
		// reap tests use.
		requireEventually(t, 5*time.Second, func() bool {
			_, err := os.Stat(pidFile)
			return err == nil
		}, "the fake git child never recorded its pid")
		raw, err := os.ReadFile(pidFile)
		require.NoError(t, err)
		fakePID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		require.NoError(t, err)
		proc, err := proctree.Lookup(fakePID)
		require.NoError(t, err)
		requireEventually(t, 5*time.Second, func() bool {
			_, ok := proctree.WorkingDir(proc.PID)
			return ok
		}, "the fake git child cwd never became observable")

		// Reap the inherited worktree — the exact teardown that, before the fix,
		// would SIGTERM an unrelated git child whose cwd was the inherited one.
		reapWorktreeWriters(inherited)

		alive := proctree.AliveSame(proc)
		// Tear down the fake git regardless of outcome so it never outlives the
		// test.
		_ = syscall.Kill(fakePID, syscall.SIGKILL)
		<-done
		require.True(t, alive,
			"a git child the runner started for a different session (cwd %s) must not be "+
				"SIGTERM'd by a reap of the inherited worktree (%s); the reaper's cwd match "+
				"must not see a false positive on an inherited daemon cwd", target, inherited)
		return
	}

	// Parent: set up the inherited worktree, the target (a different session's
	// path), and the fake git, then re-enter the test binary cwd'd at the
	// inherited worktree (modeling the daemon).
	inherited := testguard.CanonicalTempDir(t)
	target := testguard.CanonicalTempDir(t)
	fakeDir := t.TempDir()
	pidFile := filepath.Join(fakeDir, "child.pid")
	// echo $$ writes the shell pid; `exec sleep` replaces the shell with sleep
	// keeping the same pid, so the recorded pid is the one proctree observes and
	// SIGKILL later reaps — no orphaned sleep child. The pid file path is
	// shell-single-quoted in the redirect so a temp dir containing spaces or
	// shell metacharacters cannot split or reinterpret it (the git child's env
	// is filtered by repositoryPathEnvironment, so an env-var redirect would
	// not survive that filter).
	quotedPidFile := "'" + strings.ReplaceAll(pidFile, "'", `'\''`) + "'"
	script := "#!/bin/sh\necho $$ > " + quotedPidFile + "\nexec sleep 60\n"
	require.NoError(t, os.WriteFile(filepath.Join(fakeDir, "git"), []byte(script), 0o755))

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunGitCommand_ReapedInheritedWorktreeSpareUnrelatedGitChild$")
	cmd.Dir = inherited
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		helperEnv+"_TARGET="+target,
		helperEnv+"_FAKEDIR="+fakeDir,
		helperEnv+"_PID="+pidFile,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "subprocess failed:\n%s", out)
}
