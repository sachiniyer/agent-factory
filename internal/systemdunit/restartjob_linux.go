//go:build linux

package systemdunit

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// systemctlCommand is the systemctl a job read runs. A seam for tests.
var systemctlCommand = "systemctl"

// restartJobWaitDelay bounds the wait for a job read's output pipes once its
// systemctl has been killed at the context's end.
const restartJobWaitDelay = 50 * time.Millisecond

// RestartJobPending reports whether systemd is restarting unit with pid as its
// main process: whether, read while pid handles the stop's SIGTERM, the unit's
// MainPID is pid and its pending job is a restart (#7474, #7480). systemd runs a
// `systemctl restart` as one restart job whose stop phase is that SIGTERM, so
// the job reads `restart` for the whole stop, and a `systemctl stop` reads
// `stop`. Only the unit's main process may read the job as its own. Any other
// process in the unit's cgroup, such as a foreground `af --daemon` started from
// a pane trapped there, is not the process systemd is restarting.
//
// The reads end with ctx: each systemctl is killed then, whatever systemd is
// doing. A cancelled or expired ctx, an error, an absent systemctl, a MainPID
// that is not pid, no job at all, or any job but a restart reads false, a final
// stop. That is the fail-closed answer: a client refused by a final stop
// reports the refusal instead of retrying.
func RestartJobPending(ctx context.Context, unit string, pid int) bool {
	if pid <= 0 {
		return false
	}
	mainPID, ok := readSystemctl(ctx, "show", "--property=MainPID", "--value", unit)
	if !ok || strings.TrimSpace(mainPID) != strconv.Itoa(pid) {
		return false
	}
	jobs, ok := readSystemctl(ctx, "list-jobs", "--no-legend", "--plain", unit)
	return ok && restartJobListed(jobs, unit)
}

// readSystemctl runs one `systemctl --user` read, killed when ctx ends.
func readSystemctl(ctx context.Context, args ...string) (string, bool) {
	cmd := exec.CommandContext(ctx, systemctlCommand,
		append([]string{"--user", "--no-pager", "--no-ask-password"}, args...)...)
	cmd.WaitDelay = restartJobWaitDelay
	out, err := cmd.Output()
	if err != nil || ctx.Err() != nil {
		return "", false
	}
	return string(out), true
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
