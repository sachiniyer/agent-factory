package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// Regressions from the fourth Codex round on #5337 (#5329). Like the earlier
// rounds' tests, they reach the sweep only through seams that predate the
// fixes, so they compile on the unfixed tree and fail there for the
// behavioral reason.

// groupSignal is one group signal the sweep sent.
type groupSignal struct {
	pgid int
	sig  syscall.Signal
}

// TestSweepRefusesSIGTERMOnceMemberLeftGroup: group 600's only marked member
// calls setsid while the sweep is signaling group 500 first. It keeps its pid
// and start stamp, so identity alone still says "alive" — but it is no longer
// in 600, and once the leader is gone 600 is a number another group may reuse.
// The sweep must not signal 600; the member itself is still an orphan, now in
// group 650, and the follow-up scan reaps it there.
func TestSweepRefusesSIGTERMOnceMemberLeftGroup(t *testing.T) {
	home := t.TempDir()
	procs := []fakeOrphanProc{
		{proc: proctree.Process{PID: 500, PPID: 1, SID: 500, Comm: "sh", StartID: 1}, pgid: 500, env: markedOrphanEnv(t, home)},
		{proc: proctree.Process{PID: 601, PPID: 1, SID: 600, Comm: "sh", StartID: 2}, pgid: 600, env: markedOrphanEnv(t, home)},
	}
	var left bool
	termed := sweepFakeTableWith(t, home, procs, func(*watcherSupervisor) {
		pgidOf, kill := watchOrphanPGID, watchOrphanKillGroup
		watchOrphanPGID = func(pid int) (int, error) {
			if pid == 601 && left {
				return 650, nil
			}
			return pgidOf(pid)
		}
		watchOrphanKillGroup = func(pgid int, sig syscall.Signal) error {
			if pgid == 500 && sig == syscall.SIGTERM {
				left = true
			}
			return kill(pgid, sig)
		}
	})

	if slices.Contains(termed, 600) {
		t.Fatalf("SIGTERMed groups %v: group 600 was signaled after its only marked member left it", termed)
	}
	if !slices.Contains(termed, 500) || !slices.Contains(termed, 650) {
		t.Fatalf("SIGTERMed groups %v, want 500 and the member's new group 650", termed)
	}
}

// TestSweepRefusesSIGKILLOnceMemberLeftGroup: the marked member survives
// SIGTERM by leaving the group, while an unmarked process keeps 600 occupied.
// The escalation must not SIGKILL 600 on the strength of a member that is no
// longer in it.
func TestSweepRefusesSIGKILLOnceMemberLeftGroup(t *testing.T) {
	home := t.TempDir()
	procs := []fakeOrphanProc{
		{proc: proctree.Process{PID: 600, PPID: 1, SID: 600, Comm: "sh", StartID: 1}, pgid: 600, env: markedOrphanEnv(t, home)},
	}
	var (
		mu     sync.Mutex
		left   bool
		reaped bool
		sent   []groupSignal
	)
	sweepFakeTableWith(t, home, procs, func(s *watcherSupervisor) {
		s.stopGrace = 0
		// The helper's table also feeds its environ lookups, so it still runs;
		// the member then stays in every snapshot, wherever its group is.
		table := watchOrphanSnapshot
		watchOrphanSnapshot = func() (map[int]proctree.Process, error) {
			if _, err := table(); err != nil {
				return nil, err
			}
			return map[int]proctree.Process{600: procs[0].proc}, nil
		}
		watchOrphanPGID = func(pid int) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case pid == os.Getpid():
				return 42, nil
			case pid == 600 && left:
				return 650, nil
			case pid == 600:
				return 600, nil
			}
			return 0, syscall.ESRCH
		}
		watchOrphanSameIdentity = func(proctree.Process) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return !reaped, nil
		}
		watchOrphanKillGroup = func(pgid int, sig syscall.Signal) error {
			mu.Lock()
			defer mu.Unlock()
			if sig == 0 {
				if pgid == 600 {
					return nil // the unmarked occupant
				}
				return syscall.ESRCH
			}
			sent = append(sent, groupSignal{pgid, sig})
			switch {
			case pgid == 600 && sig == syscall.SIGTERM:
				left = true
			case pgid == 650:
				reaped = true
			}
			return nil
		}
	})

	for _, s := range sent {
		if s.pgid == 600 && s.sig == syscall.SIGKILL {
			t.Fatalf("signals %v: group 600 was SIGKILLed after its only marked member left it", sent)
		}
	}
	if !slices.Contains(sent, groupSignal{650, syscall.SIGTERM}) {
		t.Fatalf("signals %v: the member's new group 650 was never reaped", sent)
	}
}

// TestSweepFollowsUpOnGroupCreatedDuringSweep: orphan 600's SIGTERM handler
// daemonizes a marked child into a brand-new group 700 before exiting. No
// per-group rescan can see 700, and the sweep runs once per generation, so a
// follow-up full scan has to find and reap it.
func TestSweepFollowsUpOnGroupCreatedDuringSweep(t *testing.T) {
	home := t.TempDir()
	procs := []fakeOrphanProc{
		{proc: proctree.Process{PID: 600, PPID: 1, SID: 600, Comm: "sh", StartID: 1}, pgid: 600, env: markedOrphanEnv(t, home)},
		{proc: proctree.Process{PID: 700, PPID: 1, SID: 700, Comm: "sh", StartID: 2}, pgid: 700, env: markedOrphanEnv(t, home)},
	}
	var detached bool
	termed := sweepFakeTableWith(t, home, procs, func(*watcherSupervisor) {
		table, kill := watchOrphanSnapshot, watchOrphanKillGroup
		watchOrphanSnapshot = func() (map[int]proctree.Process, error) {
			snap, err := table()
			if !detached {
				delete(snap, 700)
			}
			return snap, err
		}
		watchOrphanKillGroup = func(pgid int, sig syscall.Signal) error {
			if pgid == 600 && sig == syscall.SIGTERM {
				detached = true
			}
			return kill(pgid, sig)
		}
	})

	if !slices.Equal(termed, []int{600, 700}) {
		t.Fatalf("SIGTERMed groups %v, want [600 700]: the group detached during the sweep survived it", termed)
	}
}

// TestWatchRecordWriteDoesNotBlockSupervision: the record write touches the AF
// home's filesystem, and a stalled one must not keep the just-started child
// from being supervised. The test holds recordMu, which the record write takes
// before touching the file, so the write stalls for as long as the test wants;
// Stop must still SIGTERM the child and return.
func TestWatchRecordWriteDoesNotBlockSupervision(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	s.reapOrphans = true
	if _, err := proctree.BootID(); err != nil {
		t.Skipf("no boot identity on this platform: %v", err)
	}
	// The startup sweep runs first, with nothing held.
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	s.recordMu.Lock()
	// Registered after newTestSupervisor's Cleanup(s.Stop), so it runs first and
	// an unfixed tree's blocked watcher can drain instead of hanging the binary.
	// It then waits for the released write to land, so it cannot race the
	// removal of the temp home.
	t.Cleanup(func() {
		s.recordMu.Unlock()
		record := filepath.Join(home, watchChildRecordName)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(record); err == nil {
				return
			}
		}
	})

	started := filepath.Join(t.TempDir(), "started")
	s.loadTasks = staticTasks(watchTask("stall1", "touch "+started+"; exec sleep 300", t.TempDir()))
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	// The child runs only after cmd.Start, which the record write follows, so
	// once it has touched the file the write is stalled on recordMu.
	waitUntil(t, 5*time.Second, "watch command to start", func() bool {
		_, err := os.Stat(started)
		return err == nil
	})

	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(s.stopGrace + 5*time.Second):
		t.Fatal("Stop did not return while the record write was stalled: the stalled write blocked child supervision")
	}
}
