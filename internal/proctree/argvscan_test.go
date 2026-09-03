package proctree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
)

func stubScan(t *testing.T, snap func() (map[int]Process, error), argv func(int) ([]string, error)) {
	t.Helper()
	previousSnapshot, previousArgv := scanSnapshot, scanArgv
	t.Cleanup(func() { scanSnapshot, scanArgv = previousSnapshot, previousArgv })
	scanSnapshot, scanArgv = snap, argv
}

func fixedSnapshot(pids ...int) func() (map[int]Process, error) {
	return func() (map[int]Process, error) {
		snap := make(map[int]Process, len(pids))
		for _, pid := range pids {
			snap[pid] = Process{PID: pid, StartID: uint64(pid)}
		}
		return snap, nil
	}
}

// The scan has to work against the real process table, not only a stubbed one —
// and this process is the one entry it can name with certainty.
func TestProcessesMatchingArgvFindsALiveProcessByItsArgv(t *testing.T) {
	self := os.Getpid()
	matched, err := ProcessesMatchingArgv(func(argv []string) bool {
		return len(argv) > 0 && strings.Contains(argv[0], ".test")
	})
	if err != nil {
		t.Fatalf("ProcessesMatchingArgv: %v", err)
	}
	found := false
	for _, process := range matched {
		if process.Process.PID == self {
			found = true
			if len(process.Argv) == 0 {
				t.Fatal("a match came back with no argv; callers read the arguments they matched on")
			}
		}
	}
	if !found {
		t.Fatalf("the scan did not find this test binary (pid %d) among %d matches", self, len(matched))
	}
}

// A process table that cannot be READ is UNKNOWN. Every caller of this is
// deciding whether something may be torn down, and an empty slice would say
// "nothing is running" about a table nobody managed to look at (#2874).
func TestProcessesMatchingArgvReportsAnUnreadableProcessTable(t *testing.T) {
	stubScan(t,
		func() (map[int]Process, error) { return nil, errors.New("reading /proc: permission denied") },
		func(int) ([]string, error) { return []string{"systemd-run"}, nil })
	matched, err := ProcessesMatchingArgv(func([]string) bool { return true })
	if err == nil {
		t.Fatalf("an unreadable process table came back as %d matches and no error", len(matched))
	}
}

// The same rule one level down: a refused argv fails the scan, while a process
// that is GONE or has nothing to classify is skipped. Those are different facts
// and collapsing them is how a live process becomes invisible.
func TestProcessesMatchingArgvSeparatesGoneFromRefused(t *testing.T) {
	const target = 42
	skippable := map[int]error{
		7:  fmt.Errorf("%w: pid 7", ErrNoArgv),
		8:  ErrProcessExited,
		9:  &fs.PathError{Op: "open", Path: "/proc/9/cmdline", Err: syscall.ENOENT},
		10: syscall.ESRCH,
	}
	for pid, readErr := range skippable {
		stubScan(t, fixedSnapshot(target, pid), func(p int) ([]string, error) {
			if p == target {
				return []string{"systemd-run", "--unit=af-hook-s1-g-0.scope"}, nil
			}
			return nil, readErr
		})
		matched, err := ProcessesMatchingArgv(func(argv []string) bool { return argv[0] == "systemd-run" })
		if err != nil {
			t.Fatalf("pid %d (%v) failed the whole scan; it is gone, not unreadable: %v", pid, readErr, err)
		}
		if len(matched) != 1 || matched[0].Process.PID != target {
			t.Fatalf("pid %d (%v): matches = %v, want just the live launcher", pid, readErr, matched)
		}
	}

	stubScan(t, fixedSnapshot(target, 11), func(p int) ([]string, error) {
		if p == target {
			return []string{"systemd-run", "--unit=af-hook-s1-g-0.scope"}, nil
		}
		return nil, &fs.PathError{Op: "open", Path: "/proc/11/cmdline", Err: syscall.EACCES}
	})
	if _, err := ProcessesMatchingArgv(func(argv []string) bool { return argv[0] == "systemd-run" }); err == nil {
		t.Fatal("an argv the kernel REFUSED was skipped like a process that had exited; the scan then reports a partial list as complete")
	}
}
