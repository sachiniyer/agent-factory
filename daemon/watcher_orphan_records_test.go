package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// These tests reach the watch child record only through its file at the AF
// home root — never through the record type — so they compile against a tree
// without the record channel and fail there for the behavioral reason (#5329).

// writeWatchRecordFile writes raw records to <home>/watch-children.json, filling
// in this kernel's boot and PID namespace identity for any record lacking them.
func writeWatchRecordFile(t *testing.T, home string, records ...map[string]any) {
	t.Helper()
	bootID, err := proctree.BootID()
	if err != nil {
		t.Skipf("no boot identity on this platform: %v", err)
	}
	pidNamespace, err := proctree.PIDNamespaceID()
	if err != nil {
		t.Skipf("no PID namespace identity on this platform: %v", err)
	}
	for _, r := range records {
		if _, ok := r["boot_id"]; !ok {
			r["boot_id"] = bootID
		}
		if _, ok := r["pid_namespace_id"]; !ok {
			r["pid_namespace_id"] = pidNamespace
		}
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "watch-children.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func watchRecord(pid int, startID uint64, generation string) map[string]any {
	return map[string]any{
		"task_id": "rec-task", "pid": pid, "pgid": pid, "start_id": startID, "generation": generation,
	}
}

// shellOnlyWatchGroup is the SIP shape: a /bin/sh leader and a /bin/sleep child
// whose environs the kernel withholds (env nil → ErrEnvUnreadable in the fake).
func shellOnlyWatchGroup(leader int, startID uint64) []fakeOrphanProc {
	return []fakeOrphanProc{
		{proc: proctree.Process{PID: leader, PPID: 1, SID: 77, Comm: "sh", StartID: startID}, pgid: leader},
		{proc: proctree.Process{PID: leader + 1, PPID: leader, SID: 77, Comm: "sleep", StartID: startID + 1}, pgid: leader},
	}
}

// envMarkedControl is an ordinary environ-marked orphan group: proof that the
// sweep ran, so a test asserting "spared" cannot pass on a sweep that did
// nothing.
func envMarkedControl(t *testing.T, home string, pgid int) fakeOrphanProc {
	return fakeOrphanProc{proc: proctree.Process{PID: pgid, PPID: 1, SID: pgid, Comm: "sh", StartID: 1}, pgid: pgid, env: markedOrphanEnv(t, home)}
}

// TestSweepReapsRecordedGroupWithUnreadableEnv is the Codex P1 on #5337: on
// SIP-enabled macOS no member of a shell-only watch command has a readable
// environ, so only the record af wrote at spawn can prove the group is ours.
func TestSweepReapsRecordedGroupWithUnreadableEnv(t *testing.T) {
	home := t.TempDir()
	writeWatchRecordFile(t, home, watchRecord(700, 7001, "previous-generation"))

	termed := sweepFakeTable(t, home, shellOnlyWatchGroup(700, 7001))

	if len(termed) != 1 || termed[0] != 700 {
		t.Fatalf("SIGTERMed groups %v, want the recorded env-unreadable group 700", termed)
	}
}

// TestSweepSparesRecordUnderRecycledPID: the recorded pid now names a
// different process instance, so the record proves nothing about it.
func TestSweepSparesRecordUnderRecycledPID(t *testing.T) {
	home := t.TempDir()
	writeWatchRecordFile(t, home, watchRecord(700, 7001, "previous-generation"))
	procs := append(shellOnlyWatchGroup(700, 9999), envMarkedControl(t, home, 900))

	termed := sweepFakeTable(t, home, procs)

	if len(termed) != 1 || termed[0] != 900 {
		t.Fatalf("SIGTERMed groups %v, want only the control group 900", termed)
	}
}

// TestSweepSparesRecordOfCurrentGeneration: this generation's records describe
// its own live watchers.
func TestSweepSparesRecordOfCurrentGeneration(t *testing.T) {
	home := t.TempDir()
	procs := append(shellOnlyWatchGroup(700, 7001), envMarkedControl(t, home, 900))

	termed := sweepFakeTableWith(t, home, procs, func(s *watcherSupervisor) {
		writeWatchRecordFile(t, home, watchRecord(700, 7001, s.generation))
	})

	if len(termed) != 1 || termed[0] != 900 {
		t.Fatalf("SIGTERMed groups %v, want only the control group 900", termed)
	}
}

// TestSweepSparesRecordOfForeignUID: a record matching a process another user
// owns authorizes nothing.
func TestSweepSparesRecordOfForeignUID(t *testing.T) {
	home := t.TempDir()
	writeWatchRecordFile(t, home, watchRecord(700, 7001, "previous-generation"))
	procs := append(shellOnlyWatchGroup(700, 7001), envMarkedControl(t, home, 900))

	termed := sweepFakeTableWith(t, home, procs, func(*watcherSupervisor) {
		watchOrphanUID = func(pid int) (int, bool) {
			if pid == 700 {
				return 1001, true
			}
			return 1000, true
		}
	})

	if len(termed) != 1 || termed[0] != 900 {
		t.Fatalf("SIGTERMed groups %v, want only the control group 900", termed)
	}
}

// TestSweepSparesRecordedGroupHoldingInfrastructure: a record-verified group
// still goes through the infrastructure check the environ channel does.
func TestSweepSparesRecordedGroupHoldingInfrastructure(t *testing.T) {
	home := t.TempDir()
	writeWatchRecordFile(t, home, watchRecord(700, 7001, "previous-generation"))
	procs := append(shellOnlyWatchGroup(700, 7001),
		fakeOrphanProc{proc: proctree.Process{PID: 702, PPID: 700, SID: 77, Comm: "tmux: server", StartID: 3}, pgid: 700},
		envMarkedControl(t, home, 900))

	termed := sweepFakeTable(t, home, procs)

	if len(termed) != 1 || termed[0] != 900 {
		t.Fatalf("SIGTERMed groups %v, want only the control group 900", termed)
	}
}

// TestSweepSurvivesCorruptRecordFile: a damaged record file disables only the
// record channel; the environ channel still reaps.
func TestSweepSurvivesCorruptRecordFile(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "watch-children.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	termed := sweepFakeTable(t, home, []fakeOrphanProc{envMarkedControl(t, home, 900)})

	if len(termed) != 1 || termed[0] != 900 {
		t.Fatalf("SIGTERMed groups %v, want the environ-marked group 900", termed)
	}
}

// TestWatchSpawnWritesChildRecord: a reaping supervisor records each watch
// command it starts — leader pid, its own pgid, its start stamp, and this
// generation — so a later daemon can prove ownership without its environ.
func TestWatchSpawnWritesChildRecord(t *testing.T) {
	s, home := newOrphanSweepSupervisor(t)
	s.loadTasks = staticTasks(watchTask("rec1", "sleep 300", t.TempDir()))
	s.reapOrphans = true
	if _, err := proctree.BootID(); err != nil {
		t.Skipf("no boot identity on this platform: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}

	type record struct {
		TaskID     string `json:"task_id"`
		PID        int    `json:"pid"`
		PGID       int    `json:"pgid"`
		StartID    uint64 `json:"start_id"`
		Generation string `json:"generation"`
	}
	var got []record
	waitUntil(t, 5*time.Second, "watch child record", func() bool {
		raw, err := os.ReadFile(filepath.Join(home, "watch-children.json"))
		if err != nil || json.Unmarshal(raw, &got) != nil {
			return false
		}
		return len(got) == 1
	})
	r := got[0]
	if r.TaskID != "rec1" || r.Generation != s.generation || r.PGID != r.PID {
		t.Fatalf("record %+v: want task rec1, generation %q, pgid == pid", r, s.generation)
	}
	live, err := proctree.Lookup(r.PID)
	if err != nil || live.StartID != r.StartID {
		t.Fatalf("recorded pid %d start %d does not name the live watch command (lookup %+v, %v)", r.PID, r.StartID, live, err)
	}
}
