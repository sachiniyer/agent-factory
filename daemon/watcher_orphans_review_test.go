package daemon

import (
	"sync"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// Regressions from the third Codex round on #5337 (#5329). Like the record
// tests, they reach the sweep only through seams that predate the fixes, so
// they compile on the unfixed tree and fail there for the behavioral reason.

// TestSweepReapsGroupRunningTmuxClient: a watch script that is running an
// ordinary tmux client at snapshot time is still an orphan. Only a tmux SERVER
// is infrastructure; sparing the group on a client that exits a moment later
// would leave the orphan beside its replacement, since the sweep runs once.
func TestSweepReapsGroupRunningTmuxClient(t *testing.T) {
	cases := []struct {
		name   string
		client proctree.Process
	}{
		{name: "retitled client", client: proctree.Process{PID: 201, PPID: 200, SID: 200, Comm: "tmux: client"}},
		// darwin cannot retitle: a client there is a plain `tmux` comm with a
		// live parent, inside the script's session.
		{name: "un-retitled client", client: proctree.Process{PID: 201, PPID: 200, SID: 200, Comm: "tmux"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			procs := []fakeOrphanProc{
				{proc: proctree.Process{PID: 200, PPID: 1, SID: 200, Comm: "sh"}, pgid: 200, env: markedOrphanEnv(t, home)},
				{proc: tc.client, pgid: 200},
			}

			termed := sweepFakeTable(t, home, procs)

			if len(termed) != 1 || termed[0] != 200 {
				t.Fatalf("SIGTERMed groups %v, want the orphan group 200 despite its tmux client", termed)
			}
		})
	}
}

// TestSweepIgnoresRecordsUnderFallbackBootID: under a subset=pid procfs the
// boot identity is the PID-namespace fallback, which can repeat after a host
// reboot, so a stale record could line up with an unrelated process. The
// record channel must stand down there; the environ channel still reaps.
func TestSweepIgnoresRecordsUnderFallbackBootID(t *testing.T) {
	const fallback = "pidns:1:2"
	home := t.TempDir()
	record := watchRecord(700, 7001, "previous-generation")
	record["boot_id"] = fallback
	writeWatchRecordFile(t, home, record)
	procs := append(shellOnlyWatchGroup(700, 7001), envMarkedControl(t, home, 900))

	bootID := watchRecordBootID
	t.Cleanup(func() { watchRecordBootID = bootID })
	termed := sweepFakeTableWith(t, home, procs, func(*watcherSupervisor) {
		watchRecordBootID = func() (string, error) { return fallback, nil }
	})

	if len(termed) != 1 || termed[0] != 900 {
		t.Fatalf("SIGTERMed groups %v, want only the environ-marked control 900", termed)
	}
}

// TestSweepRecordNeverOverrulesReadableEnviron: the record is a fallback for a
// leader whose environ the kernel withholds. A readable environ without the
// markers is a definite "not ours", and a record that happens to match the
// pid, start stamp and pgid must not overrule it.
func TestSweepRecordNeverOverrulesReadableEnviron(t *testing.T) {
	home := t.TempDir()
	writeWatchRecordFile(t, home, watchRecord(700, 7001, "previous-generation"))
	group := shellOnlyWatchGroup(700, 7001)
	group[0].env = []string{"PATH=/usr/bin:/bin", "HOME=/home/someone"}
	procs := append(group, envMarkedControl(t, home, 900))

	termed := sweepFakeTable(t, home, procs)

	if len(termed) != 1 || termed[0] != 900 {
		t.Fatalf("SIGTERMed groups %v, want only the control 900", termed)
	}
}

// rescanFixture stages the SIGTERM-handler shape: leader 600 is the only
// marked member the first snapshot sees; once its group is SIGTERMed it exits
// and child 601, forked by its handler and ignoring SIGTERM, is all that holds
// the group. childMarked says whether 601 carries the markers; unmarked, its
// environ is unreadable. It returns the signals sent to group 600, in order.
func rescanFixture(t *testing.T, childMarked bool) []syscall.Signal {
	t.Helper()
	home := t.TempDir()
	var childEnv []string
	if childMarked {
		childEnv = markedOrphanEnv(t, home)
	}
	procs := []fakeOrphanProc{
		{proc: proctree.Process{PID: 600, PPID: 1, SID: 600, Comm: "sh", StartID: 1}, pgid: 600, env: markedOrphanEnv(t, home)},
		{proc: proctree.Process{PID: 601, PPID: 1, SID: 600, Comm: "sh", StartID: 2}, pgid: 600, env: childEnv},
	}
	var (
		mu     sync.Mutex
		sent   []syscall.Signal
		termed bool
		killed bool
	)
	sweepFakeTableWith(t, home, procs, func(s *watcherSupervisor) {
		s.stopGrace = 0
		table := watchOrphanSnapshot
		watchOrphanSnapshot = func() (map[int]proctree.Process, error) {
			snap, err := table()
			mu.Lock()
			defer mu.Unlock()
			if termed {
				delete(snap, 600)
			} else {
				delete(snap, 601)
			}
			return snap, err
		}
		watchOrphanSameIdentity = func(p proctree.Process) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			if p.PID == 600 {
				return !termed, nil
			}
			return !killed, nil
		}
		watchOrphanKillGroup = func(pgid int, sig syscall.Signal) error {
			mu.Lock()
			defer mu.Unlock()
			if pgid != 600 {
				return syscall.ESRCH
			}
			switch sig {
			case 0:
				if killed {
					return syscall.ESRCH
				}
				return nil
			case syscall.SIGTERM:
				termed = true
			case syscall.SIGKILL:
				killed = true
			}
			sent = append(sent, sig)
			return nil
		}
	})
	return sent
}

// TestSweepRescansGroupBeforeRefusingSIGKILL: the snapshotted leader exits on
// SIGTERM, but the marked child its handler forked still holds the group. A
// fresh scan proves that child ours, so the group is SIGKILLed rather than
// left beside its replacement.
func TestSweepRescansGroupBeforeRefusingSIGKILL(t *testing.T) {
	sent := rescanFixture(t, true)

	if len(sent) != 2 || sent[0] != syscall.SIGTERM || sent[1] != syscall.SIGKILL {
		t.Fatalf("signals sent to group 600: %v, want [SIGTERM SIGKILL]", sent)
	}
}

// TestSweepRescanStillRefusesUnprovenSurvivor: the fail-closed half. When the
// fresh scan finds only a survivor it cannot prove ours, SIGKILL stays refused.
func TestSweepRescanStillRefusesUnprovenSurvivor(t *testing.T) {
	sent := rescanFixture(t, false)

	if len(sent) != 1 || sent[0] != syscall.SIGTERM {
		t.Fatalf("signals sent to group 600: %v, want only [SIGTERM]", sent)
	}
}
