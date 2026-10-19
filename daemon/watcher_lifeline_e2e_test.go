package daemon

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

// This file is the #5329 regression at the process level, and it deliberately
// uses nothing the fix added: it reaches the watcher only through APIs that
// predate the lifeline, so the same file fails on a tree without it.

// watchDaemonStandInEnv makes this test binary run a minimal watch-task daemon
// instead of the test suite: a watcherSupervisor armed with one watch task
// running the script in the variable, until it is killed.
const watchDaemonStandInEnv = "AF_TEST_WATCH_DAEMON_SCRIPT"

func init() {
	script := os.Getenv(watchDaemonStandInEnv)
	if script == "" {
		return
	}
	// Not inherited by the watch command (or by a lifeline re-exec of this
	// same binary, which would otherwise start another stand-in daemon).
	_ = os.Unsetenv(watchDaemonStandInEnv)
	runWatchDaemonStandIn(script)
	os.Exit(0)
}

func runWatchDaemonStandIn(script string) {
	dir, err := os.MkdirTemp("", "af-watch-standin-")
	if err != nil {
		os.Exit(3)
	}
	s := newWatcherSupervisor()
	s.loadTasks = func() ([]task.Task, error) {
		return []task.Task{{ID: "standin", Name: "standin", WatchCmd: script, ProjectPath: dir, Enabled: true}}, nil
	}
	s.deliver = func(string, string, watchDeliveryOptions) error { return nil }
	s.setStatus = func(string, string) {}
	s.recordDrops = func(string, int, time.Time) error { return nil }
	s.logPath = func(id string) (string, error) { return filepath.Join(dir, "task-"+id+".log"), nil }
	s.queueDir = func() (string, error) { return dir, nil }
	s.shell = "sh"
	s.stopGrace = 300 * time.Millisecond
	if err := s.Reload(); err != nil {
		os.Exit(4)
	}
	_, _ = os.Stdout.WriteString("armed\n")
	select {}
}

func readPIDFile(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid, err == nil && pid > 1
}

// processGone reports that pid is no longer running. A zombie counts: a
// killed process whose new parent has not reaped it yet still answers signal
// 0, and on a host whose PID 1 reaps slowly it can stay that way for a while.
func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) || isZombie(pid)
}

// isZombie reports a process that has exited but not yet been reaped by its
// new parent; it is no longer running.
func isZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// TestWatchCommandDiesWithItsDaemon is #5329: a daemon SIGKILLed while its
// watch command runs must not leave that command — or anything it backgrounded
// — running, to hold a lock against the next daemon's replacement. The script
// ignores SIGTERM, so only the escalation to SIGKILL can end it.
func TestWatchCommandDiesWithItsDaemon(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	shellPIDFile := filepath.Join(dir, "shell.pid")
	childPIDFile := filepath.Join(dir, "child.pid")
	script := `trap "" TERM; sleep 300 & echo $! > ` + childPIDFile + `; echo $$ > ` + shellPIDFile +
		`; while :; do sleep 0.05; done`

	daemon := exec.Command(exe, "-test.run=^$")
	daemon.Env = append(os.Environ(), watchDaemonStandInEnv+"="+script, "AGENT_FACTORY_HOME="+t.TempDir())
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	var shell, child int
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		for _, pid := range []int{shell, child} {
			if pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "armed\n" {
		t.Fatalf("stand-in daemon did not arm its watch task (wrote %q)", line)
	}
	waitUntil(t, 10*time.Second, "watch command to start", func() bool {
		var okShell, okChild bool
		shell, okShell = readPIDFile(shellPIDFile)
		child, okChild = readPIDFile(childPIDFile)
		return okShell && okChild
	})
	time.Sleep(100 * time.Millisecond) // the trap is installed before the pid is written

	if err := daemon.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Wait()

	waitUntil(t, 10*time.Second, "the dead daemon's watch command and its backgrounded child to exit", func() bool {
		return processGone(shell) && processGone(child)
	})
}
