//go:build linux

package systemdunit

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeSystemctl points the job read at a shell script standing in for
// systemctl, until the test ends.
func fakeSystemctl(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := systemctlCommand
	systemctlCommand = path
	t.Cleanup(func() { systemctlCommand = previous })
}

// unitSystemctl is a systemctl whose unit has mainPID as its MainPID and the
// given list-jobs output.
func unitSystemctl(mainPID int, jobs string) string {
	return `case "$*" in
*"show"*"--property=MainPID"*) echo ` + strconv.Itoa(mainPID) + ` ;;
*"list-jobs"*) printf '%s\n' "` + jobs + `" ;;
esac`
}

func readWithin(t *testing.T, unit string, pid int) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return RestartJobPending(ctx, unit, pid)
}

// TestRestartJobPendingReadsTheUnitsJob: only a restart job of this unit reads
// as a restart. A stop job, another unit's restart, no job, and a failed or
// absent systemctl are each a final stop.
func TestRestartJobPendingReadsTheUnitsJob(t *testing.T) {
	const unit = "agent-factory-daemon.service"
	const pid = 4242
	for _, tc := range []struct {
		name   string
		script string
		want   bool
	}{
		{"a restart job", unitSystemctl(pid, "42 "+unit+" restart running"), true},
		{"a try-restart job", unitSystemctl(pid, "42 "+unit+" try-restart running"), true},
		{"a stop job", unitSystemctl(pid, "42 "+unit+" stop running"), false},
		{"another unit's restart", unitSystemctl(pid, "42 other.service restart running"), false},
		{"no job", unitSystemctl(pid, ""), false},
		{"a failed systemctl", unitSystemctl(pid, "42 "+unit+" restart running") + "\nexit 1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeSystemctl(t, tc.script)
			if got := readWithin(t, unit, pid); got != tc.want {
				t.Fatalf("RestartJobPending = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("no systemctl", func(t *testing.T) {
		previous := systemctlCommand
		systemctlCommand = filepath.Join(t.TempDir(), "absent")
		t.Cleanup(func() { systemctlCommand = previous })
		if readWithin(t, unit, pid) {
			t.Fatal("an absent systemctl read as a restart")
		}
	})
}

// TestRestartJobPendingIsOnlyTheMainPIDsToRead: a process in the unit that is
// not its MainPID never reads the unit's restart job as its own (#7480), nor
// does any process while the unit has no main process.
func TestRestartJobPendingIsOnlyTheMainPIDsToRead(t *testing.T) {
	const unit = "agent-factory-daemon.service"
	for _, tc := range []struct {
		name    string
		mainPID int
		pid     int
	}{
		{"another process in the unit", 4242, 5151},
		{"a unit with no main process", 0, 4242},
		{"no pid of our own", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeSystemctl(t, unitSystemctl(tc.mainPID, "42 "+unit+" restart running"))
			if readWithin(t, unit, tc.pid) {
				t.Fatalf("pid %d read the restart job of a unit whose MainPID is %d", tc.pid, tc.mainPID)
			}
		})
	}
}

// TestRestartJobPendingKillsAStuckSystemctlAtItsBound: a systemctl that never
// answers (a wedged user manager) is killed when the read's context ends, and
// the stop is a final one. The stop never waits on systemd past the bound.
func TestRestartJobPendingKillsAStuckSystemctlAtItsBound(t *testing.T) {
	fakeSystemctl(t, `exec sleep 30`)
	const bound = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	start := time.Now()
	if RestartJobPending(ctx, "agent-factory-daemon.service", 4242) {
		t.Fatal("a stuck systemctl read as a restart")
	}
	if elapsed := time.Since(start); elapsed > bound+time.Second {
		t.Fatalf("the job read took %s against a %s bound: a wedged systemd held the stop", elapsed, bound)
	}
}
