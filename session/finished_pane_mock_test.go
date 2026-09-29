package session

import (
	"os/exec"
	"strconv"
	"strings"
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
	return strings.NewReplacer(
		"#{pane_pid}", strconv.Itoa(pid),
		"#{pane_dead}", "1",
		"#{pane_dead_status}", p.status,
		"#{pane_dead_signal}", p.signal,
		"#{pane_dead_time}", p.at,
	).Replace(c.Args[len(c.Args)-1]) + "\n", true
}

// paneExitAnswer answers a pane query for a finished pane with this status and
// death time. dead must be "1": a live pane has a real pid, which these mocks
// never hand out.
func paneExitAnswer(c *exec.Cmd, dead, status, at string) string {
	if dead != "1" {
		panic("paneExitAnswer only models a finished pane")
	}
	answer, _ := finishedPane{status: status, at: at}.answer(c)
	return answer
}
