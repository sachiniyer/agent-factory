package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// TestHookCancellation_ChildProcessOrphaned is the regression test for #610.
// Before the process-group fix, cancelling the context only SIGKILL'd the
// `sh -c` shell, leaving any backgrounded grandchildren (e.g. `sleep 30 &`)
// reparented to init and alive indefinitely. With the fix, sh runs as the
// leader of its own process group and the watchdog signals the whole group,
// so the grandchild dies along with its shell.
func TestHookCancellation_ChildProcessOrphaned(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	repoPath := freshRepoConfig(t, []string{
		fmt.Sprintf(`sleep 30 & echo $! > %q; wait`, pidFile),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	RunPostWorktreeHooksAsync(ctx, repoPath, t.TempDir())

	pid := waitForPidFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	cancel()

	if !waitForProcessExit(pid, 3*time.Second) {
		t.Fatalf("grandchild pid %d survived ctx cancellation — process-group kill did not reach it", pid)
	}
}

// TestHookCancellation_BackgroundedGrandchildKilledByGroupSignal codifies the
// process-group contract beyond the single-child case: a hook script that
// backgrounds multiple processes should have all of them reaped on
// cancellation, not just the shell or the first backgrounded child.
func TestHookCancellation_BackgroundedGrandchildKilledByGroupSignal(t *testing.T) {
	pidDir := t.TempDir()
	pidFiles := []string{
		filepath.Join(pidDir, "pid1"),
		filepath.Join(pidDir, "pid2"),
		filepath.Join(pidDir, "pid3"),
	}

	script := fmt.Sprintf(
		"sleep 30 & echo $! > %q\nsleep 30 & echo $! > %q\nsleep 30 & echo $! > %q\nwait",
		pidFiles[0], pidFiles[1], pidFiles[2],
	)
	repoPath := freshRepoConfig(t, []string{script})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	RunPostWorktreeHooksAsync(ctx, repoPath, t.TempDir())

	pids := make([]int, len(pidFiles))
	for i, f := range pidFiles {
		pids[i] = waitForPidFile(t, f, 5*time.Second)
	}
	t.Cleanup(func() {
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	})

	cancel()

	for _, p := range pids {
		if !waitForProcessExit(p, 3*time.Second) {
			t.Fatalf("backgrounded grandchild pid %d survived ctx cancellation — process group not killed", p)
		}
	}
}

// TestHookCompletion_BackgroundedGrandchildKilled is the regression test for
// #769. A hook that backgrounds a process and exits immediately (no `wait`)
// used to leak the grandchild: the watchdog exited via doneCh on normal
// completion before any cancellation, so nothing ever signalled the process
// group. With the fix, the group is SIGKILL'd on every exit path — including
// normal completion with no cancellation at all — so the grandchild dies.
func TestHookCompletion_BackgroundedGrandchildKilled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// No `wait`: the shell backgrounds sleep, records its pid, and exits 0
	// immediately. The context is never cancelled, so the leak is only caught
	// by the completion-path group kill.
	repoPath := freshRepoConfig(t, []string{
		fmt.Sprintf(`sleep 30 & echo $! > %q`, pidFile),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	RunPostWorktreeHooksAsync(ctx, repoPath, t.TempDir())

	pid := waitForPidFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	// cmd.Wait unblocks ~hookWaitDelay after the shell exits, then the group
	// kill fires; allow margin over that bound for the grandchild to be reaped.
	if !waitForProcessExit(pid, 6*time.Second) {
		t.Fatalf("backgrounded grandchild pid %d survived hook completion — process group not killed on the success path", pid)
	}
}

// freshRepoConfig isolates the per-test config dir via AGENT_FACTORY_HOME,
// writes a repo config with the given post-worktree commands, and returns a
// repo path the test should hand to RunPostWorktreeHooksAsync. The repo path
// itself never needs to exist on disk — hooks.go only passes it through
// RepoIDFromRoot to locate the per-repo config file.
func freshRepoConfig(t *testing.T, postCmds []string) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := filepath.Join(t.TempDir(), "repo")
	repoID := config.RepoIDFromRoot(repoPath)
	require.NoError(t, config.SaveRepoConfig(repoID, &config.RepoConfig{
		PostWorktreeCommands: postCmds,
	}))
	return repoPath
}

// waitForPidFile polls until pidFile contains a parseable positive pid and
// returns it, or fails the test on timeout.
func waitForPidFile(t *testing.T, pidFile string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s did not appear with a valid pid within %s", pidFile, timeout)
	return 0
}

// waitForProcessExit polls until syscall.Kill(pid, 0) reports ESRCH. Signal 0
// is a permission/existence probe that delivers nothing; ESRCH means the
// process has been reaped (init reaps the orphaned grandchild after SIGKILL).
func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
