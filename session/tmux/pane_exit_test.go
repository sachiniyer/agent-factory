package tmux

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// #4506 review: tmux leaves pane_dead_status EMPTY for a command killed by a
// signal while still filling pane_dead_time. A whitespace split collapsed the
// empty field and read the timestamp as the exit status.
func TestProbePaneExitKeepsAnEmptyStatusUnknown(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status, at    string
		wantStatus    int
		wantKnown     bool
		wantAt        time.Time
		wantDeadKnown bool
	}{
		{name: "signal-killed", status: "", at: "1726000000", wantAt: time.Unix(1726000000, 0)},
		{name: "exited", status: "3", at: "1726000000", wantStatus: 3, wantKnown: true, wantAt: time.Unix(1726000000, 0)},
		{name: "exited zero", status: "0", at: "1726000000", wantStatus: 0, wantKnown: true, wantAt: time.Unix(1726000000, 0)},
		{name: "no death time", status: "5", at: "", wantStatus: 5, wantKnown: true},
		{name: "nothing but dead", status: "", at: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := NewTmuxSessionWithDeps("pane-exit", "true", nil, cmd_test.MockCmdExec{
				RunFunc: func(*exec.Cmd) error { return nil },
				OutputFunc: func(c *exec.Cmd) ([]byte, error) {
					format := c.Args[len(c.Args)-1]
					sep := " "
					if strings.Contains(format, "|") {
						sep = "|"
					}
					return []byte("1" + sep + tc.status + sep + tc.at + "\n"), nil
				},
			})
			dead, status, statusKnown, at, known := ts.ProbePaneExit()
			assert.True(t, known)
			assert.True(t, dead)
			assert.Equal(t, tc.wantKnown, statusKnown)
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
			return []byte("0||\n"), nil
		},
	})
	dead, _, statusKnown, _, known := ts.ProbePaneExit()
	assert.True(t, known)
	assert.False(t, dead)
	assert.False(t, statusKnown)
}
