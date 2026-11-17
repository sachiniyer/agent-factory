//go:build linux

package systemdunit

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// systemctlCommand is the systemctl a job read runs. A seam for tests.
var systemctlCommand = "systemctl"

// restartJobWaitDelay bounds the wait for the job read's output pipes once its
// systemctl has been killed at the bound.
const restartJobWaitDelay = 50 * time.Millisecond

// RestartJobPending reports whether systemd is restarting unit: whether the
// unit's pending job, read while its process handles the stop's SIGTERM, is a
// restart (#7474). systemd runs a `systemctl restart` as one restart job whose
// stop phase is that SIGTERM, so the job reads `restart` for the whole stop, and
// a `systemctl stop` reads `stop`.
//
// The read is bounded: its systemctl is killed at bound, whatever systemd is
// doing. A timeout, an error, an absent systemctl, no job at all, or any job
// but a restart reads false, a final stop. That is the fail-closed answer: a
// client refused by a final stop reports the refusal instead of retrying.
func RestartJobPending(unit string, bound time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, systemctlCommand,
		"--user", "--no-pager", "--no-ask-password", "list-jobs", "--no-legend", "--plain", unit)
	cmd.WaitDelay = restartJobWaitDelay
	out, err := cmd.Output()
	if err != nil || ctx.Err() != nil {
		return false
	}
	return restartJobListed(string(out), unit)
}

// restartJobListed reports whether list-jobs output names a restart job for
// unit. Each line is `JOB UNIT TYPE STATE`; try-restart is a restart of an
// active unit.
func restartJobListed(out, unit string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != unit {
			continue
		}
		switch fields[2] {
		case "restart", "try-restart":
			return true
		}
	}
	return false
}
