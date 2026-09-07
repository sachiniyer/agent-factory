//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const archiveRestartHelperEnv = "AF_TEST_ARCHIVE_RESTART_HELPER"

// TestArchiveHookOutputSurvivesRunnerExit is the archive-runner half of #4010.
// See the post-worktree twin: the killed helper owns the capture readers, while
// the hook's separately redirected status file remains writable after that
// simulated daemon exit.
func TestArchiveHookOutputSurvivesRunnerExit(t *testing.T) {
	if os.Getenv(archiveRestartHelperEnv) == "1" {
		runArchiveRestartHelper(t)
		return
	}

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	worktree := filepath.Join(dir, "worktree")
	pidFile := filepath.Join(dir, "hook.pid")
	releaseFile := filepath.Join(dir, "release")
	statusFile := filepath.Join(dir, "writer.status")
	writer := filepath.Join(dir, "writer.sh")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writer, []byte("#!/bin/sh\nprintf 'archive stdout after restart\\n'\nprintf 'archive stderr after restart\\n' >&2\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf(
		"printf '%%s\\n' \"$$\" > %q; while [ ! -f %q ]; do sleep 0.02; done; %q; status=$?; printf '%%s\\n' \"$status\" > %q; exit \"$status\"",
		pidFile, releaseFile, writer, statusFile,
	)

	runner := exec.Command(os.Args[0], "-test.run=^TestArchiveHookOutputSurvivesRunnerExit$")
	runner.Env = append(os.Environ(),
		archiveRestartHelperEnv+"=1",
		"AF_TEST_RESTART_HOME="+home,
		"AF_TEST_RESTART_WORKTREE="+worktree,
		"AF_TEST_RESTART_COMMAND="+command,
	)
	if err := runner.Start(); err != nil {
		t.Fatalf("start archive-hook helper: %v", err)
	}

	hookPID := waitForArchiveRestartValue(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(-hookPID, syscall.SIGKILL) })
	if err := runner.Process.Kill(); err != nil {
		t.Fatalf("stop helper daemon generation: %v", err)
	}
	_ = runner.Wait()
	if err := os.WriteFile(releaseFile, nil, 0o600); err != nil {
		t.Fatalf("release hook writer: %v", err)
	}

	if got := waitForArchiveRestartValue(t, statusFile, 5*time.Second); got != 23 {
		t.Fatalf("hook writer exited %d, want 23; a SIGPIPE-derived 141 means its output still depended on the exited daemon", got)
	}
	if !waitForArchiveRestartExit(hookPID, 3*time.Second) {
		t.Fatalf("hook pid %d did not exit after recording its writer status", hookPID)
	}

	logs, err := filepath.Glob(filepath.Join(home, "logs", "hooks", "on-archive-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("found %d on-archive hook logs, want 1 under %s", len(logs), filepath.Join(home, "logs", "hooks"))
	}
	tail, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatalf("read surviving hook output: %v", err)
	}
	for _, want := range []string{"archive stdout after restart", "archive stderr after restart"} {
		if !strings.Contains(string(tail), want) {
			t.Fatalf("surviving hook log %s omitted %q; got %q", logs[0], want, tail)
		}
	}
}

func runArchiveRestartHelper(t *testing.T) {
	home := os.Getenv("AF_TEST_RESTART_HOME")
	worktree := os.Getenv("AF_TEST_RESTART_WORKTREE")
	command := os.Getenv("AF_TEST_RESTART_COMMAND")
	if home == "" || worktree == "" || command == "" {
		t.Fatal("restart helper environment is incomplete")
	}
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv(autostartSystemdMarker, "")
	t.Setenv("SYSTEMD_EXEC_PID", "")
	writeOnArchiveCommand(t, command)
	err := runOnArchiveHook(onArchiveHookContext{
		sessionID:   "4010-restart-probe",
		title:       "restart probe",
		repoRoot:    filepath.Dir(worktree),
		worktree:    worktree,
		archivePath: filepath.Join(filepath.Dir(worktree), "archive"),
	})
	if err != nil {
		t.Fatalf("archive hook returned: %v", err)
	}
}

func waitForArchiveRestartValue(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if value, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil && value > 0 {
				return value
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not contain a positive integer within %s", path, timeout)
	return 0
}

func waitForArchiveRestartExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH || processIsZombie(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
