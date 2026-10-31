package session

import (
	"os/exec"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

// absentPID is a pid no process can have: above the largest pid Linux allows
// (2^22) and far above darwin's 99999. It stands in for a finished pane's
// reaped root without the flake of reusing a real exited pid.
const absentPID = 1<<22 + 1

// finishedPane is a held dead pane as the session-package tmux mocks answer it
// (#4506 review). tmux keeps reporting a finished pane's pane_pid, and that pid
// is gone from the process table, which is what the teardown must survive.
type finishedPane struct {
	// pid defaults to absentPID.
	pid int
	// status, signal and at are pane_dead_status, pane_dead_signal and
	// pane_dead_time; tmux fills at least one once it has reaped the root.
	status, signal, at string
	// unstamped models a pane af did not launch — one the user created by
	// splitting the window and then focused. It carries no @af_pane_pid
	// stamp, no deaths counter, and no @af_pane_cmd pin, so its collected
	// exit status is not evidence of the recorded command's exit. Under
	// remain-on-exit such a pane can be dead, reaped, and the session's
	// active pane — exactly the shape the exit probe must not attribute to
	// the command.
	unstamped bool
	// start is the pane's recorded pane_start_command: afPaneStart for a pane
	// af launched — the launch-provenance gate reads its exec marker and the
	// recorded command as the last word — and a foreign command for a pane
	// the user created. An unstamped pane reports its own foreign start
	// command; a stamped one must carry its program's.
	start string
}

// afPaneStart is the pane_start_command a pane af launched reports: the exec
// marker and the recorded command as the wrapper's last word — the two halves
// of launch provenance the exit probe checks. It is also a runnable command
// (the marker travels as an environment variable), so fixtures backed by a
// real tmux can launch it verbatim.
func afPaneStart(program string) string {
	return "env " + sessionenv.ExecMarker + "=1 sh -c " + shellquote.Quote(program)
}

// answer expands a pane query's format the way tmux would for this pane, and
// reports false for commands that are not pane queries.
func (p finishedPane) answer(c *exec.Cmd) (string, bool) {
	joined := strings.Join(c.Args, " ")
	if !strings.Contains(joined, "list-panes") && !strings.Contains(joined, "display-message") {
		return "", false
	}
	pid := p.pid
	if pid == 0 {
		pid = absentPID
	}
	stamp, deaths, tty, cmdMatch, start := strconv.Itoa(pid)+":0:/dev/pts/0", "1", "/dev/pts/0", "1", p.start
	if p.unstamped {
		// A pane af did not launch: no @af_pane_pid stamp, no deaths counter,
		// no @af_pane_cmd pin, and its own start command does not byte-match
		// the unset pin and is not af's launch.
		stamp, deaths, tty, cmdMatch = "", "", "/dev/pts/3", "0"
		if start == "" {
			start = "sh -c 'exit " + p.status + "'"
		}
	}
	return strings.NewReplacer(
		"#{pane_pid}", strconv.Itoa(pid),
		"#{pane_dead}", "1",
		"#{pane_dead_status}", p.status,
		"#{pane_dead_signal}", p.signal,
		"#{pane_dead_time}", p.at,
		"#{@af_pane_pid}", stamp,
		"#{@af_pane_deaths}", deaths,
		"#{pane_tty}", tty,
		"#{==:#{@af_pane_cmd},#{pane_start_command}}", cmdMatch,
		// After the == pair so its inner #{pane_start_command} token is
		// consumed whole: the raw start command is the probe's last field.
		"#{pane_start_command}", start,
	).Replace(c.Args[len(c.Args)-1]) + "\n", true
}

// paneExitAnswer answers a pane query for a finished pane with this status,
// death time and recorded program — the pane afPaneStart(program) leaves af's
// launch on. dead must be "1": a live pane has a real pid, which these mocks
// never hand out.
func paneExitAnswer(c *exec.Cmd, dead, status, at, program string) string {
	if dead != "1" {
		panic("paneExitAnswer only models a finished pane")
	}
	answer, _ := finishedPane{status: status, at: at, start: afPaneStart(program)}.answer(c)
	return answer
}
