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

// stubScanUID stands in for the kernel's ownership answer. A pid absent from
// owners reports "cannot tell", which is a third answer and not a synonym for
// "not ours".
func stubScanUID(t *testing.T, owners map[int]int) {
	t.Helper()
	previous := scanUID
	t.Cleanup(func() { scanUID = previous })
	scanUID = func(pid int) (int, bool) {
		uid, ok := owners[pid]
		return uid, ok
	}
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
//
// It runs on EVERY platform, and the macOS job is where it earns its keep: XNU
// answers kern.procargs2 for another user's process with EINVAL, so before the
// ownership scoping this failed on the first root-owned daemon it reached (pid
// 460 on the runner, measured). A failure about a process we do not own is the
// bug and is reported as one; a failure about a process we could own is the
// documented contract — darwin restricts some same-uid processes too — and is
// reported as a skip naming the pid rather than as a pass or a red.
func TestProcessesMatchingArgvFindsALiveProcessByItsArgv(t *testing.T) {
	self := os.Getpid()
	matched, err := ProcessesMatchingArgv(func(argv []string) bool {
		return len(argv) > 0 && strings.Contains(argv[0], ".test")
	})
	if err != nil {
		var unreadable *ArgvUnreadableError
		if !errors.As(err, &unreadable) {
			t.Fatalf("the scan of a live process table failed for a reason other than one process's argv: %v", err)
		}
		if uid, known := readUID(unreadable.PID); known && uid != os.Getuid() {
			t.Fatalf("the scan failed on pid %d, owned by uid %d and not by us (%d): %v; "+
				"a process we do not own cannot be one we spawned, and refusing its argv is routine — "+
				"every foreign pid under hidepid=1, and every foreign pid on darwin. Failing here makes "+
				"every caller refuse permanently on those machines (#3667 review).",
				unreadable.PID, uid, os.Getuid(), err)
		}
		t.Skipf("this host restricts the argv of pid %d, which we may own — the contract permits that "+
			"failure, so the live-table half of this test cannot run here: %v", unreadable.PID, err)
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
	// Ownership is pinned rather than borrowed from the live /proc: whether a
	// refusal fails the scan depends on it, and pid 11 is a root-owned kernel
	// thread on an ordinary Linux box — which would make this pass for the wrong
	// reason (skipped as foreign) on a non-root runner and for the right one on
	// a root runner. See TestARefusedArgvFromAnotherUidDoesNotFailTheScan.
	stubScanUID(t, map[int]int{target: os.Getuid(), 11: os.Getuid()})
	if _, err := ProcessesMatchingArgv(func(argv []string) bool { return argv[0] == "systemd-run" }); err == nil {
		t.Fatal("an argv the kernel REFUSED was skipped like a process that had exited; the scan then reports a partial list as complete")
	}
}

// A refused argv is UNKNOWN only about a process that could BE ours.
//
// On a hidepid=1 host every other user's /proc/<pid>/cmdline is EACCES, so a
// scan that failed on the first foreign pid would refuse every rebuild, cleanup
// and archive on a shared box — permanently, on every retry. A guard whose
// failure mode is permanent is worse than the hazard it prevents, and this one
// would fire on machines that have no af launcher at all.
//
// Ownership is what makes the skip a fact rather than a shrug: a process owned
// by another uid cannot be a child this one spawned, so its refusal is not
// evidence about us.
func TestARefusedArgvFromAnotherUidDoesNotFailTheScan(t *testing.T) {
	// The seam below stands in for the kernel, so first check the real oracle
	// reports this process honestly — otherwise the two tests here would be
	// agreeing with a fiction.
	if uid, ok := readUID(os.Getpid()); !ok || uid != os.Getuid() {
		t.Fatalf("readUID(self) = %d, %v; want %d — the uid oracle these tests stand on does not work here", uid, ok, os.Getuid())
	}

	const (
		target  = 42
		foreign = 43
	)
	// Every spelling the platforms actually use, because the rule is written
	// against ownership and must not quietly depend on an errno list: EACCES is
	// linux/hidepid, EINVAL is what XNU returns for another user's process
	// (measured on the macOS runner), EPERM is the one people expect it to.
	for _, refusal := range []error{syscall.EACCES, syscall.EINVAL, syscall.EPERM} {
		stubScan(t, fixedSnapshot(target, foreign), func(pid int) ([]string, error) {
			if pid == target {
				return []string{"systemd-run", "--unit=af-hook-s1-g-0.scope"}, nil
			}
			return nil, fmt.Errorf("reading argv for pid %d: %w", pid, refusal)
		})
		stubScanUID(t, map[int]int{target: os.Getuid(), foreign: os.Getuid() + 1})

		matched, err := ProcessesMatchingArgv(func(argv []string) bool { return argv[0] == "systemd-run" })
		if err != nil {
			t.Fatalf("another user's argv refused with %v failed the whole scan; that is every foreign pid on a hidepid=1 host and on darwin, and the sweep it feeds then refuses forever: %v", refusal, err)
		}
		if len(matched) != 1 || matched[0].Process.PID != target {
			t.Fatalf("%v: matches = %v, want just the live launcher", refusal, matched)
		}
	}
}

// The other half, and the reason the skip above is scoped rather than blanket:
// a process running as US could be the launcher, so an argv we were refused for
// one of those is a genuine UNKNOWN and fails the scan. Same for a pid whose
// ownership we could not establish at all — "I could not tell whether it is
// ours" is not "it is not ours".
func TestARefusedArgvFromOurOwnUidFailsTheScan(t *testing.T) {
	const (
		target  = 42
		refused = 44
	)
	refuse := func(pid int) ([]string, error) {
		if pid == target {
			return []string{"systemd-run", "--unit=af-hook-s1-g-0.scope"}, nil
		}
		return nil, &fs.PathError{Op: "open", Path: "/proc/44/cmdline", Err: syscall.EACCES}
	}
	match := func(argv []string) bool { return argv[0] == "systemd-run" }

	stubScan(t, fixedSnapshot(target, refused), refuse)
	stubScanUID(t, map[int]int{target: os.Getuid(), refused: os.Getuid()})
	if _, err := ProcessesMatchingArgv(match); err == nil {
		t.Fatal("an argv we were refused for a process running as US was skipped; it could be the launcher, and the scan then reports a partial list as complete")
	}

	// Ownership unknown: not readable is not a negative.
	stubScan(t, fixedSnapshot(target, refused), refuse)
	stubScanUID(t, map[int]int{target: os.Getuid()})
	if _, err := ProcessesMatchingArgv(match); err == nil {
		t.Fatal("an argv refusal on a pid whose owner could not be read was treated as another user's; unknown ownership is not proof the process is not ours")
	}
}
