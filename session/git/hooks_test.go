package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hooklog"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

func TestPostWorktreeHookEnvironmentRequiresExplicitCredentialNames(t *testing.T) {
	const (
		customName = "CUSTOM_PACKAGE_TOKEN"
		deniedName = "AF_TEST_UNRELATED_SECRET"
	)
	t.Setenv("OPENAI_API_KEY", "unit-test-credential")
	t.Setenv(customName, "unit-test-credential")
	t.Setenv(deniedName, "unit-test-credential")

	namesPath := filepath.Join(t.TempDir(), "hook-environment-names")
	repoPath := freshRepoConfig(t, []string{
		fmt.Sprintf(`env | sed 's/=.*//' | sort > %q`, namesPath),
	})
	done := RunPostWorktreeHooksAsyncWithEnvironment(context.Background(), repoPath, t.TempDir(), []string{customName})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("post-worktree hook did not finish")
	}
	data, err := os.ReadFile(namesPath)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Fields(string(data))
	for _, want := range []string{"PATH", customName} {
		if !slices.Contains(names, want) {
			t.Fatalf("post-worktree hook omitted allowed variable %s", want)
		}
	}
	if slices.Contains(names, "OPENAI_API_KEY") {
		t.Fatal("repo-controlled post-worktree hook inherited selected-agent credentials without explicit pass-through")
	}
	if slices.Contains(names, deniedName) {
		t.Fatalf("post-worktree hook inherited disallowed variable %s", deniedName)
	}
}

func TestPostWorktreeHookFailureSurfacesBoundedTailAndLogPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	payloadPath := filepath.Join(t.TempDir(), "hook-output")
	payload := "discarded beginning\n" + strings.Repeat("x", hooklog.TailLimit) + "\nkept ending\n"
	if err := os.WriteFile(payloadPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(t.TempDir(), "repo")
	writeLegacyRepoConfig(t, config.RepoIDFromRoot(repoPath), &config.RepoConfig{
		PostWorktreeCommands: []string{fmt.Sprintf("cat %q; exit 23", payloadPath)},
	})

	var errors logtest.Buffer
	previous := aflog.ErrorLog.Writer()
	aflog.ErrorLog.SetOutput(&errors)
	t.Cleanup(func() { aflog.ErrorLog.SetOutput(previous) })
	<-RunPostWorktreeHooksAsyncWithEnvironment(context.Background(), repoPath, t.TempDir(), nil)

	logs, err := filepath.Glob(filepath.Join(home, "logs", "hooks", "post-worktree-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("found %d post-worktree logs, want 1", len(logs))
	}
	got := errors.String()
	for _, want := range []string{"exit status 23", logs[0], "[output truncated to last 65536 bytes]", "kept ending"} {
		if !strings.Contains(got, want) {
			t.Fatalf("post-worktree failure omitted %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "discarded beginning") {
		t.Fatalf("post-worktree failure surfaced more than the bounded tail:\n%s", got)
	}
	full, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"discarded beginning", "kept ending"} {
		if !strings.Contains(string(full), want) {
			t.Fatalf("full hook log omitted %q", want)
		}
	}
}

func TestPostWorktreeHookSuccessRemovesOutputLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := filepath.Join(t.TempDir(), "repo")
	writeLegacyRepoConfig(t, config.RepoIDFromRoot(repoPath), &config.RepoConfig{
		PostWorktreeCommands: []string{"printf success"},
	})

	<-RunPostWorktreeHooksAsyncWithEnvironment(context.Background(), repoPath, t.TempDir(), nil)

	logs, err := filepath.Glob(filepath.Join(home, "logs", "hooks", "post-worktree-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("successful post-worktree hook retained output logs: %v", logs)
	}
}

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

	RunPostWorktreeHooksAsyncWithEnvironment(ctx, repoPath, t.TempDir(), nil)

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

	RunPostWorktreeHooksAsyncWithEnvironment(ctx, repoPath, t.TempDir(), nil)

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

	RunPostWorktreeHooksAsyncWithEnvironment(ctx, repoPath, t.TempDir(), nil)

	pid := waitForPidFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	// The hook's direct output file lets cmd.Wait return as soon as the shell
	// exits; allow margin for the group kill and reaping that follow.
	if !waitForProcessExit(pid, 6*time.Second) {
		t.Fatalf("backgrounded grandchild pid %d survived hook completion — process group not killed on the success path", pid)
	}
}

// freshRepoConfig isolates the per-test config dir via AGENT_FACTORY_HOME,
// writes a repo config with the given post-worktree commands, and returns a
// repo path the test should hand to RunPostWorktreeHooksAsyncWithEnvironment. The repo path
// itself never needs to exist on disk — hooks.go only passes it through
// RepoIDFromRoot to locate the per-repo config file.
func freshRepoConfig(t *testing.T, postCmds []string) string {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := filepath.Join(t.TempDir(), "repo")
	repoID := config.RepoIDFromRoot(repoPath)
	writeLegacyRepoConfig(t, repoID, &config.RepoConfig{
		PostWorktreeCommands: postCmds,
	})
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

// waitForProcessExit polls until the process is terminated. Signal 0 is a
// permission/existence probe that delivers nothing; ESRCH means the process has
// been reaped.
//
// A ZOMBIE counts as terminated, and that is the load-bearing part. These tests
// kill an orphaned grandchild, so whether it is reaped promptly is up to
// whatever inherited it — init on a normal box, but in a container PID 1 is
// often the test harness or a shell that never reaps. There, kill(pid, 0) keeps
// succeeding on a process that cannot run, and an ESRCH-only oracle reports the
// process-group kill as having failed when it did exactly what it should. Read
// the state field of /proc/<pid>/stat instead, which is unambiguous.
func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		if processIsZombie(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// processIsZombie reads /proc/<pid>/stat, whose third space-separated field
// after the comm is the state character. comm is parenthesized and may itself
// contain spaces and parentheses, so the scan starts after the LAST ')'.
func processIsZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// ANY read failure means "cannot tell", never "exited". On a host without
		// procfs — macOS, or a container that does not mount it — every pid returns
		// ENOENT, and treating that as exit would make waitForProcessExit accept a
		// live process instantly and pass these tests without testing anything.
		// "Gone" is the ESRCH probe's answer to give, and it already gives it.
		return false
	}
	closing := bytes.LastIndexByte(data, ')')
	if closing < 0 || closing+2 >= len(data) {
		return false
	}
	return data[closing+2] == 'Z'
}

// waitForProcessExit must never accept a LIVE process. It reads /proc to tell a
// zombie from a runnable process, and a host without procfs — macOS, or a
// container that does not mount it — fails that read for every pid; treating
// that as exit would make every caller of this helper pass instantly without
// testing anything (#3650 review). CI runs this on macOS, where the read fails.
func TestWaitForProcessExitDoesNotAcceptALiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if waitForProcessExit(cmd.Process.Pid, 300*time.Millisecond) {
		t.Fatalf("a live process (pid %d) was reported as exited", cmd.Process.Pid)
	}
}
