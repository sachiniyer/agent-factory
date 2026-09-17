package tmux

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// #4506 review: tmux leaves pane_dead_status EMPTY for a command killed by a
// signal while still filling pane_dead_time. A whitespace split collapsed the
// empty field and read the timestamp as the exit status.
func TestProbePaneExitKeepsAnEmptyStatusUnknown(t *testing.T) {
	gone := exitedProcess(t).PID
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
