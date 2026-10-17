package daemon

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// Regressions from the sixth Codex round on #5337 (#5329). Like the earlier
// rounds' tests, they reach the code only through seams and methods that
// predate the fixes, so they compile on the unfixed tree and fail there for
// the behavioral reason.

// TestSweepFollowsUpWhenOnlyCandidateMovedBeforeSIGTERM: the pass's only
// candidate, 600, calls setsid between the scan that found it and the
// membership check before its SIGTERM. The pass then signals nothing, but the
// process is still a marked orphan, now leading group 650; the sweep must scan
// again and reap it there rather than end with it alive.
func TestSweepFollowsUpWhenOnlyCandidateMovedBeforeSIGTERM(t *testing.T) {
	home := t.TempDir()
	procs := []fakeOrphanProc{
		{proc: proctree.Process{PID: 600, PPID: 1, SID: 600, Comm: "sh", StartID: 1}, pgid: 600, env: markedOrphanEnv(t, home)},
	}
	var identityChecks int
	var moved bool
	termed := sweepFakeTableWith(t, home, procs, func(*watcherSupervisor) {
		pgidOf := watchOrphanPGID
		watchOrphanPGID = func(pid int) (int, error) {
			if pid == 600 && moved {
				return 650, nil
			}
			return pgidOf(pid)
		}
		// The scan checks 600's identity once; the next check is the
		// membership check right before the signal. It moves in between.
		watchOrphanSameIdentity = func(p proctree.Process) (bool, error) {
			if p.PID == 600 {
				identityChecks++
				if identityChecks == 2 {
					moved = true
				}
			}
			return true, nil
		}
	})

	if !slices.Equal(termed, []int{650}) {
		t.Fatalf("SIGTERMed groups %v, want [650]: the candidate that moved before its signal survived the sweep", termed)
	}
}

// TestSweepSharesOneSIGKILLDeadlineAcrossGroups: two groups survive SIGTERM and
// then SIGKILL (a member stuck in uninterruptible I/O). Both must be SIGKILLed
// before the sweep waits on either, so the wait costs one watchOrphanKillWait
// per pass rather than one per group.
func TestSweepSharesOneSIGKILLDeadlineAcrossGroups(t *testing.T) {
	home := t.TempDir()
	procs := []fakeOrphanProc{
		{proc: proctree.Process{PID: 600, PPID: 1, SID: 600, Comm: "sh", StartID: 1}, pgid: 600, env: markedOrphanEnv(t, home)},
		{proc: proctree.Process{PID: 700, PPID: 1, SID: 700, Comm: "sh", StartID: 2}, pgid: 700, env: markedOrphanEnv(t, home)},
	}
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	sweepFakeTableWith(t, home, procs, func(s *watcherSupervisor) {
		s.stopGrace = 0
		kill := watchOrphanKillGroup
		watchOrphanKillGroup = func(pgid int, sig syscall.Signal) error {
			switch sig {
			case 0:
				return nil // never dies
			case syscall.SIGKILL:
				record("kill")
				return nil
			}
			return kill(pgid, sig)
		}
		watchOrphanSleep = func(d time.Duration) {
			record("sleep")
			time.Sleep(d)
		}
	})

	mu.Lock()
	defer mu.Unlock()
	first := slices.Index(events, "kill")
	if first < 0 || first+1 >= len(events) || events[first+1] != "kill" {
		t.Fatalf("events %v: the sweep waited on one SIGKILLed group before SIGKILLing the next", compactEvents(events))
	}
}

// compactEvents run-length encodes an event log for a readable failure.
func compactEvents(events []string) []string {
	var out []string
	for i := 0; i < len(events); {
		j := i
		for j < len(events) && events[j] == events[i] {
			j++
		}
		out = append(out, fmt.Sprintf("%s×%d", events[i], j-i))
		i = j
	}
	return out
}

// TestWatchRecordPersistKeepsNewerChildWithReusedPID: a delayed persist for a
// child that already exited runs after its pid was reused by a newer watch
// child whose own record is on disk. The stale persist must drop only itself,
// not the newer child's live record.
func TestWatchRecordPersistKeepsNewerChildWithReusedPID(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	bootID, err := proctree.BootID()
	if err != nil {
		t.Skipf("no boot identity on this platform: %v", err)
	}
	pidNamespace, err := proctree.PIDNamespaceID()
	if err != nil {
		t.Skipf("no PID namespace identity on this platform: %v", err)
	}
	live := exec.Command("sleep", "300")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	p, err := proctree.Lookup(live.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	newer := watchRecord(p.PID, p.StartID, "newer-generation")
	newer["task_id"] = "newer"
	writeWatchRecordFile(t, home, newer)

	stale := watchChildRecord{
		TaskID: "stale", PID: p.PID, PGID: p.PID, StartID: p.StartID + 1,
		BootID: bootID, PIDNamespace: pidNamespace, Generation: s.generation,
	}
	if err := s.persistWatchChildRecord(stale); err != nil {
		t.Fatal(err)
	}

	records, err := readWatchChildRecords(filepath.Join(home, watchChildRecordName))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].TaskID != "newer" || records[0].StartID != p.StartID {
		t.Fatalf("records %+v, want only the newer child's live record for pid %d", records, p.PID)
	}
}

// TestRestartSweepsOrphansBeforeFirstSpawn: an explicit restart can be the
// supervisor's first spawn — RestartTask is admissible before any reconcile in
// the startup window — so it must sweep the previous generation's orphans
// before starting its replacement, exactly as reconcile does.
func TestRestartSweepsOrphansBeforeFirstSpawn(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	s.reapOrphans = true
	orphan, _ := spawnWatchOrphan(t, "sleep 300", watchOrphanEnv(home, "previous-generation"))

	if err := s.restart(watchTask("restart1", "sleep 300", t.TempDir())); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 5*time.Second, "restart to reap the previous generation's orphan", func() bool {
		return !orphanStillAlive(orphan)
	})
}
