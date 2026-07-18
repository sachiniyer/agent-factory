package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCleanup_ReapsSurvivingWriterBeforeRemoval is the #2025 fail-first
// regression: a session whose agent left a live process writing into the
// worktree orphans the tree, because teardown removes the worktree WITHOUT first
// ensuring the session's descendant processes are dead.
//
// The observable orphan (`git worktree remove -f` then the os.RemoveAll fallback
// both failing "directory not empty") is a genuine filesystem race — `git
// worktree remove -f` drains most of the tree before failing, so the fallback
// os.RemoveAll then usually wins against the near-empty remainder. What is NOT a
// race, and is the actual defect, is that a process still cwd'd in the worktree
// SURVIVES the kill: on current code nothing reaps it, so it is still alive after
// Cleanup returns and can (and on the maintainer's box did) win the removal race
// on a later attempt. This test asserts that deterministic invariant — teardown
// must have reaped the survivor — and then that the tree is fully removed.
//
// Every guard is a HARD cap so the writer can never wedge CI: the shell loop
// self-terminates after a finite iteration count; it truncates a single recurring
// file so it never fills the disk; exec.CommandContext SIGKILLs it at a deadline;
// t.Cleanup SIGKILLs its whole process group and reaps it.
func TestCleanup_ReapsSurvivingWriterBeforeRemoval(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	runGit(t, repoRoot, "worktree", "add", "-b", "af-reap-2025", worktreePath)
	require.DirExists(t, worktreePath)

	// A node_modules-style untracked tree gives the removal real work to do, so a
	// surviving writer has a window to race it — the shape the bug needs.
	seedLargeUntrackedTree(t, filepath.Join(worktreePath, "packages", "node_modules"))

	// A real, detached (own process group) survivor whose cwd is the worktree,
	// standing in for an installer/dev-server that outlived the session kill. It
	// keeps writing into the worktree AND beats a heartbeat to an absolute path
	// OUTSIDE it. The heartbeat is what makes the test sound: it means the writer
	// can only ever exit by being SIGNALLED, never by "its files vanished" — so a
	// closed exit channel unambiguously means teardown reaped it, not that Cleanup
	// deleted the tree out from under a shell that then self-terminated.
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	exited := startWorktreeWriter(t, worktreePath, heartbeat)

	// Prove the writer is actually alive and running before Cleanup starts, so a
	// pass can only mean the reap killed a LIVE writer — not that we raced an idle
	// one that had already exited.
	requireEventually(t, 5*time.Second, func() bool {
		_, err := os.Stat(heartbeat)
		return err == nil
	}, "the survivor never started running")

	// Long enough that real git never trips it, short enough to keep the test fast.
	shortenLocalTimeout(t, 30*time.Second)

	gw := &GitWorktree{
		repoPath:          repoRoot,
		worktreePath:      worktreePath,
		branchName:        "af-reap-2025",
		branchCreatedByUs: true,
	}

	state, err := gw.Cleanup()

	// THE fail-first assertion: teardown must have reaped the survivor. On current
	// code nothing kills a process merely cwd'd in the worktree, so it is still
	// looping here and this wait times out. The bound is generous versus the reap's
	// SIGTERM→SIGKILL escalation yet finite, so a red test fails fast, never hangs.
	select {
	case <-exited:
		// Reaped — the writer process exited during teardown.
	case <-time.After(5 * time.Second):
		t.Fatal("teardown did not reap the surviving writer (#2025): a process still " +
			"cwd'd in the worktree outlived the kill and can race the worktree removal")
	}

	// With the writer gone, the removal is no longer racing anything, so it must
	// have fully succeeded and left no orphan.
	require.NoError(t, err, "with the writer reaped, Cleanup must remove the tree cleanly")
	require.Equal(t, CleanupSettled, state, "a reaped-then-removed teardown establishes its outcome")
	assert.NoDirExists(t, worktreePath,
		"the worktree must be fully removed once the surviving writer has been reaped (#2025)")
}

// seedLargeUntrackedTree fills dir with enough small files that a recursive
// delete has measurable work to do — the window a racing writer needs.
func seedLargeUntrackedTree(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for i := 0; i < 16; i++ {
		sub := filepath.Join(dir, "d"+strconv.Itoa(i))
		require.NoError(t, os.MkdirAll(sub, 0o755))
		for j := 0; j < 256; j++ {
			require.NoError(t, os.WriteFile(filepath.Join(sub, "f"+strconv.Itoa(j)), []byte("x"), 0o644))
		}
	}
}

// startWorktreeWriter launches a background process whose working directory is
// worktreeDir. Each iteration it (1) writes a heartbeat to the absolute path
// heartbeat, OUTSIDE the worktree, which always succeeds, and (2) writes into the
// worktree, which may fail once Cleanup deletes it. Both use `echo` — a REGULAR
// built-in, so a redirection error only sets $? and the shell keeps looping; it
// never self-terminates when the worktree vanishes. It returns a channel closed
// when the process exits, so the caller can assert whether teardown reaped it.
// Aggressively self-bounding — see the guards in the test doc.
func startWorktreeWriter(t *testing.T, worktreeDir, heartbeat string) <-chan struct{} {
	t.Helper()
	// Bounded iteration count so it self-terminates even if every other guard
	// failed. No external command per iteration (echo is a built-in), so it loops
	// fast enough to keep the worktree genuinely busy for the removal race, while
	// the absolute heartbeat guarantees it never dies just because its cwd/target
	// disappeared.
	loop := `i=0
while [ "$i" -lt 2000000000 ]; do
  echo x > "` + heartbeat + `" 2>/dev/null || true
  echo x > .af-writer-keepalive 2>/dev/null || true
  i=$((i + 1))
done`

	// The context deadline is the outermost hard cap: even if every other guard
	// failed, the writer is SIGKILLed here. Comfortably longer than the reap
	// escalation and the removal, short enough to never hang CI.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, "sh", "-c", loop)
	cmd.Dir = worktreeDir
	// Own process group so the whole writer tree can be reaped as a group and so it
	// mimics a detached survivor that escaped the pane's foreground group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// On context cancel, kill the GROUP, not just the shell.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	require.NoError(t, cmd.Start())
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-exited
	})
	return exited
}

// requireEventually polls cond until it is true or timeout elapses, failing with
// msg otherwise. Bounded, so it can never hang the test.
func requireEventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
