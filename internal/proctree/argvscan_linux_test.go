//go:build linux

package proctree

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// startArgvlessSleeper starts a real process of ours whose command line reads as
// nothing to classify for its whole life. That is the reading a launcher gives
// for a moment mid-execve (#7239). It is exec'd with an EMPTY argv. Linux 5.18
// and later substitute argv = [""], so /proc/<pid>/cmdline is "\0"; older
// kernels leave argc at 0, so it is empty. Either way readArgv answers
// ErrNoArgv. The sleeper needs no arguments and does not dispatch on argv[0].
func startArgvlessSleeper(t *testing.T) int {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.StartProcess(sleeper.Path(t), []string{}, &os.ProcAttr{
		Dir:   t.TempDir(),
		Files: []*os.File{devNull, devNull, devNull},
	})
	_ = devNull.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The fixture this test started and recorded, and nothing else.
		_ = fixture.Kill()
		_, _ = fixture.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, lookupErr := Lookup(fixture.Pid)
		if lookupErr == nil && p.Comm == "sleeper" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture: the argv-less sleeper never finished exec: %+v %v", p, lookupErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := readArgv(fixture.Pid); !errors.Is(err, ErrNoArgv) {
		t.Fatalf("fixture: an empty argv must read as ErrNoArgv, got %v", err)
	}
	return fixture.Pid
}

// End to end on the real process table: a live process of ours with no argv
// is reported as UNKNOWN, so RunningHookLaunchers' callers hold off. It is
// never left out of a list that then reads as "no launcher" (#7239).
func TestProcessesMatchingArgv_ReportsALiveArgvlessProcessOfOurs(t *testing.T) {
	pid := startArgvlessSleeper(t)
	if p, err := Lookup(pid); err != nil || p.KernelThread {
		t.Fatalf("fixture: %+v, %v; it must be an ordinary process, not a kernel thread", p, err)
	}

	// The scan stops at the first process it cannot classify, in map order,
	// so another one of ours on this machine could be named first. Retry a
	// few times. If that other one is just as persistent, this machine cannot
	// answer the question cleanly, and that is a skip, not a pass.
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		_, err = ProcessesMatchingArgv(func([]string) bool { return false })
		var unreadable *ArgvUnreadableError
		if !errors.As(err, &unreadable) {
			t.Fatalf("err = %v; a live argv-less process of ours must fail the scan as unknown, "+
				"not vanish from a list that reads as absence", err)
		}
		if unreadable.PID == pid {
			if !errors.Is(err, ErrNoArgv) || !unreadable.OwnedByUs {
				t.Fatalf("err = %v (owned by us: %v); want ErrNoArgv, attributed to a process of ours", err, unreadable.OwnedByUs)
			}
			return
		}
	}
	t.Skipf("another process of ours on this machine is unclassifiable too, and kept being named first: %v", err)
}

// PF_KTHREAD is read from the stat line's flags field. Pinned on synthetic
// lines, plus the live kthreadd where this kernel's /proc shows it.
func TestParseStatReadsTheKernelThreadFlag(t *testing.T) {
	// pid (comm) state ppid pgrp session tty tpgid flags … starttime (field 22)
	line := func(comm, flags string) []byte {
		return []byte("7 (" + comm + ") S 2 0 0 0 -1 " + flags + " 0 0 0 0 0 0 0 0 20 0 1 0 15 0 0")
	}
	kernel, _, err := parseStat(7, line("kworker/0:1", "69238880"))
	if err != nil || !kernel.KernelThread {
		t.Fatalf("flags 0x%x carry PF_KTHREAD: got %+v, %v", 69238880, kernel, err)
	}
	user, pgrp, err := parseStat(7, line("a) (b c", "4194560"))
	if err != nil || user.KernelThread || user.StartID != 15 || user.Comm != "a) (b c" || pgrp != 0 {
		t.Fatalf("an ordinary task: got %+v, %v", user, err)
	}
	if _, _, err := parseStat(7, line("x", "nope")); err == nil {
		t.Fatal("a malformed flags field must fail the parse, not read as \"not a kernel thread\"")
	}

	self, err := Lookup(os.Getpid())
	if err != nil || self.KernelThread {
		t.Fatalf("this test process is not a kernel thread: %+v, %v", self, err)
	}
	if kthreadd, err := Lookup(2); err == nil && kthreadd.Comm == "kthreadd" && !kthreadd.KernelThread {
		t.Fatalf("pid 2 is kthreadd, a kernel thread, but its flag was not read: %+v", kthreadd)
	}
}
