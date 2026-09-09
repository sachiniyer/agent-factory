//go:build linux

package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

const postWorktreeRestartHelperEnv = "AF_TEST_POST_WORKTREE_RESTART_HELPER"

// TestPostWorktreeHookOutputSurvivesRunnerExit is the #4010 regression. The
// helper process stands in for one daemon generation and starts the real hook
// runner. Killing it closes every capture-pipe reader that lived in that
// generation. The hook then runs an external writer: with a daemon-owned pipe
// it dies from SIGPIPE (status 141), while a daemon-independent output file lets
// it print both streams and reach its deliberate exit 23.
func TestPostWorktreeHookOutputSurvivesRunnerExit(t *testing.T) {
	if os.Getenv(postWorktreeRestartHelperEnv) == "1" {
		runPostWorktreeRestartHelper(t)
		return
	}

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	repo := filepath.Join(dir, "repo")
	worktree := filepath.Join(dir, "worktree")
	pidFile := filepath.Join(dir, "hook.pid")
	releaseFile := filepath.Join(dir, "release")
	statusFile := filepath.Join(dir, "writer.status")
	writer := filepath.Join(dir, "writer.sh")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writer, []byte("#!/bin/sh\nprintf 'post-worktree stdout after restart\\n'\nprintf 'post-worktree stderr after restart\\n' >&2\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	command := restartProbeCommand(pidFile, releaseFile, statusFile, writer)
	runner := exec.Command(os.Args[0], "-test.run=^TestPostWorktreeHookOutputSurvivesRunnerExit$")
	runner.Env = append(os.Environ(),
		postWorktreeRestartHelperEnv+"=1",
		"AF_TEST_RESTART_HOME="+home,
		"AF_TEST_RESTART_REPO="+repo,
		"AF_TEST_RESTART_WORKTREE="+worktree,
		"AF_TEST_RESTART_COMMAND="+command,
	)
	if err := runner.Start(); err != nil {
		t.Fatalf("start hook-runner helper: %v", err)
	}
	runnerStopped := false
	t.Cleanup(func() {
		if runnerStopped {
			return
		}
		_ = runner.Process.Kill()
		_ = runner.Wait()
	})

	hookPID := waitForPidFile(t, pidFile, 20*time.Second)
	t.Cleanup(func() { _ = killProcessGroup(hookPID) })
	if err := runner.Process.Kill(); err != nil {
		t.Fatalf("stop helper daemon generation: %v", err)
	}
	_ = runner.Wait()
	runnerStopped = true
	if err := os.WriteFile(releaseFile, nil, 0o600); err != nil {
		t.Fatalf("release hook writer: %v", err)
	}

	if got := waitForPidFile(t, statusFile, 5*time.Second); got != 23 {
		t.Fatalf("hook writer exited %d, want 23; a SIGPIPE-derived 141 means its output still depended on the exited daemon", got)
	}
	if !waitForProcessExit(hookPID, 3*time.Second) {
		t.Fatalf("hook pid %d did not exit after recording its writer status", hookPID)
	}

	logs, err := filepath.Glob(filepath.Join(home, "logs", "hooks", "post-worktree-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("found %d post-worktree hook logs, want 1 under %s", len(logs), filepath.Join(home, "logs", "hooks"))
	}
	tail, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatalf("read surviving hook output: %v", err)
	}
	for _, want := range []string{"post-worktree stdout after restart", "post-worktree stderr after restart"} {
		if !strings.Contains(string(tail), want) {
			t.Fatalf("surviving hook log %s omitted %q; got %q", logs[0], want, tail)
		}
	}
}

func runPostWorktreeRestartHelper(t *testing.T) {
	home := os.Getenv("AF_TEST_RESTART_HOME")
	repo := os.Getenv("AF_TEST_RESTART_REPO")
	worktree := os.Getenv("AF_TEST_RESTART_WORKTREE")
	command := os.Getenv("AF_TEST_RESTART_COMMAND")
	if home == "" || repo == "" || worktree == "" || command == "" {
		t.Fatal("restart helper environment is incomplete")
	}
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeLegacyRepoConfig(t, config.RepoIDFromRoot(repo), &config.RepoConfig{
		PostWorktreeCommands: []string{command},
	})
	<-RunPostWorktreeHooksAsyncWithEnvironment(context.Background(), repo, worktree, nil)
}

func restartProbeCommand(pidFile, releaseFile, statusFile, writer string) string {
	return fmt.Sprintf(
		"printf '%%s\\n' \"$$\" > %q; %s; %q; status=$?; printf '%%s\\n' \"$status\" > %q; exit \"$status\"",
		pidFile, boundedFileGate(releaseFile, "", gatedHookPollLimit, gatedHookPollInterval), writer, statusFile,
	)
}

func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
