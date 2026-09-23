package tmux

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #4506 review: tmux leaves pane_dead_status EMPTY for a command killed by a
// signal while still filling pane_dead_time. A whitespace split collapsed the
// empty field and read the timestamp as the exit status.
func TestProbePaneExitKeepsAnEmptyStatusUnknown(t *testing.T) {
	shrinkPaneStatusWait(t)
	gone := exitedProcess(t).PID
	uncollected := uncollectedProcess(t)
	for _, tc := range []struct {
		name                      string
		status, signal, deadTime  string
		pid                       int
		wantDead, wantKnownStatus bool
		wantStatus                int
		wantAt                    time.Time
	}{
		{name: "signal-killed", signal: "9", deadTime: "1726000000", pid: gone,
			wantDead: true, wantAt: time.Unix(1726000000, 0)},
		{name: "exited", status: "3", deadTime: "1726000000", pid: gone,
			wantDead: true, wantKnownStatus: true, wantStatus: 3, wantAt: time.Unix(1726000000, 0)},
		{name: "exited zero", status: "0", deadTime: "1726000000", pid: gone,
			wantDead: true, wantKnownStatus: true, wantAt: time.Unix(1726000000, 0)},
		{name: "no death time", status: "5", pid: gone,
			wantDead: true, wantKnownStatus: true, wantStatus: 5},
		// tmux reports nothing about the root: the pane pid decides.
		{name: "unreported, root gone", pid: gone, wantDead: true},
		{name: "unreported, root running", pid: os.Getpid()},
		// tmux has not collected the root yet, and does not within the wait:
		// it has still finished.
		{name: "unreported, root exited but uncollected", pid: uncollected, wantDead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := NewTmuxSessionWithDeps("pane-exit", "true", nil,
				heldPaneExec(tc.pid, tc.status, tc.signal, tc.deadTime, nil))
			dead, status, statusKnown, at, known := ts.ProbePaneExit()
			assert.True(t, known)
			assert.Equal(t, tc.wantDead, dead)
			assert.Equal(t, tc.wantKnownStatus, statusKnown)
			assert.Equal(t, tc.wantStatus, status)
			assert.True(t, at.Equal(tc.wantAt), "at = %v, want %v", at, tc.wantAt)
		})
	}
}

// #4682: tmux marks a pane dead when its terminal closes, which can come before
// it collects the root and fills pane_dead_status. A probe in that window must
// wait for the status rather than record a plain exit as unknown: a process
// tab's immediate-failure check and its durable exit record both read it.
func TestProbePaneExitWaitsForTheStatusOfAnUncollectedRoot(t *testing.T) {
	uncollected := uncollectedProcess(t)
	reads := 0
	before := heldPaneExec(uncollected, "", "", "", nil)
	after := heldPaneExec(uncollected, "7", "", "1726000000", nil)
	ts := NewTmuxSessionWithDeps("pane-exit-pending", "true", nil, cmd_test.MockCmdExec{
		RunFunc: before.RunFunc,
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			reads++
			if reads < 3 {
				return before.OutputFunc(c)
			}
			return after.OutputFunc(c)
		},
	})
	dead, status, statusKnown, at, known := ts.ProbePaneExit()
	require.True(t, known)
	require.True(t, dead)
	require.True(t, statusKnown, "the probe answered before tmux reported the status")
	require.Equal(t, 7, status)
	require.True(t, at.Equal(time.Unix(1726000000, 0)), "at = %v", at)
	require.Equal(t, 3, reads)
}

// shrinkPaneStatusWait bounds ProbePaneExit's wait for an uncollected root, so a
// fixture whose root tmux never collects gives up quickly.
func shrinkPaneStatusWait(t *testing.T) {
	t.Helper()
	old := paneStatusWait
	paneStatusWait = 100 * time.Millisecond
	t.Cleanup(func() { paneStatusWait = old })
}

// A live pane is answered live whatever the other fields hold.
func TestProbePaneExitLivePane(t *testing.T) {
	ts := NewTmuxSessionWithDeps("pane-live", "true", nil, cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return []byte("0||||123\n"), nil
		},
	})
	dead, _, statusKnown, _, known := ts.ProbePaneExit()
	assert.True(t, known)
	assert.False(t, dead)
	assert.False(t, statusKnown)
}

// uncollectedProcess returns the pid of a child that has exited and that
// nothing has collected yet, which is what a pane root is between its exit and
// tmux reaping it.
func uncollectedProcess(t *testing.T) int {
	t.Helper()
	c := exec.Command("sleep", "300")
	require.NoError(t, c.Start())
	t.Cleanup(func() { _, _ = c.Process.Wait() })
	require.NoError(t, c.Process.Kill())
	require.Eventually(t, func() bool {
		_, err := proctree.Lookup(c.Process.Pid)
		return errors.Is(err, proctree.ErrProcessExited)
	}, 3*time.Second, 10*time.Millisecond, "the child never became an uncollected exit")
	return c.Process.Pid
}

// tmux runs a pane command as `<default-shell> -c <command line>`, and af's
// command line starts its launch shim, which then execs /bin/sh -c <command>.
// Only the first two stages mean the command has not started.
func TestArgvIsLaunchShim(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"zsh", "-c", "/opt/af __af-session-env-exec-account-environment claude 0 work '' 0 ./deploy.sh"}, true},
		{[]string{"/opt/af", "__af-session-env-exec-account-environment", "claude", "0", "work", "", "0", "./deploy.sh"}, true},
		{[]string{"/opt/af", "__af-session-env-exec", "claude", "0", "claude"}, true},
		{[]string{"/opt/af", "__af-session-env-exec-account", "claude", "0", "work", "", "0", "claude"}, true},
		{[]string{"/bin/sh", "-c", "./deploy.sh"}, false},
		{[]string{"/bin/sh", "./deploy.sh"}, false},
		{[]string{"sleep", "300"}, false},
		{nil, false},
	} {
		assert.Equal(t, tc.want, argvIsLaunchShim(tc.argv), "%q", tc.argv)
	}
}
