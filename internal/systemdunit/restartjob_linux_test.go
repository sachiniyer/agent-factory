//go:build linux

package systemdunit

import (
	"os"
	"path/filepath"
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

// TestRestartJobPendingReadsTheUnitsJob: only a restart job of this unit reads
// as a restart. A stop job, another unit's restart, no job, and a failed or
// absent systemctl are each a final stop.
func TestRestartJobPendingReadsTheUnitsJob(t *testing.T) {
	const unit = "agent-factory-daemon.service"
	for _, tc := range []struct {
		name   string
		script string
		want   bool
	}{
		{"a restart job", `case "$*" in *"list-jobs"*"` + unit + `"*) echo "42 ` + unit + ` restart running";; esac`, true},
		{"a try-restart job", `echo "42 ` + unit + ` try-restart running"`, true},
		{"a stop job", `echo "42 ` + unit + ` stop running"`, false},
		{"another unit's restart", `echo "42 other.service restart running"`, false},
		{"no job", `true`, false},
		{"a failed systemctl", `echo "42 ` + unit + ` restart running"; exit 1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeSystemctl(t, tc.script)
			if got := RestartJobPending(unit, 2*time.Second); got != tc.want {
				t.Fatalf("RestartJobPending = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("no systemctl", func(t *testing.T) {
		previous := systemctlCommand
		systemctlCommand = filepath.Join(t.TempDir(), "absent")
		t.Cleanup(func() { systemctlCommand = previous })
		if RestartJobPending(unit, 2*time.Second) {
			t.Fatal("an absent systemctl read as a restart")
		}
	})
}

// TestRestartJobPendingKillsAStuckSystemctlAtItsBound: a systemctl that never
// answers (a wedged user manager) is killed at the bound, and the stop is a
// final one. The stop never waits on systemd past the bound.
func TestRestartJobPendingKillsAStuckSystemctlAtItsBound(t *testing.T) {
	fakeSystemctl(t, `exec sleep 30`)
	const bound = 200 * time.Millisecond
	start := time.Now()
	if RestartJobPending("agent-factory-daemon.service", bound) {
		t.Fatal("a stuck systemctl read as a restart")
	}
	if elapsed := time.Since(start); elapsed > bound+time.Second {
		t.Fatalf("the job read took %s against a %s bound: a wedged systemd held the stop", elapsed, bound)
	}
}
