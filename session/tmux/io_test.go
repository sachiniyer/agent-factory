package tmux

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// TapEnter / TapDAndEnter moved off the attach PTY onto clientless `send-keys`
// commands in #1592 Phase 2 PR7 (the tmux-server-mediated attach client, whose
// master they used to write CR to, was retired). These tests pin the exact argv
// so a regression back to a raw PTY write — or a wrong key name — is caught, and
// pin the ErrSessionGone mapping daemon-poll callers depend on.

// recordTapCommands captures every tmux invocation the given tap func issues,
// with has-session (ExistsOrUnknown) reporting `alive`.
func recordTapCommands(t *testing.T, alive bool, tap func(s *TmuxSession) error) ([]string, error) {
	t.Helper()
	var cmds []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			cmds = append(cmds, joined)
			// The tap's send-keys fails, then ExistsOrUnknown's has-session probe
			// reports whether the session is gone.
			if strings.Contains(joined, "has-session") {
				if alive {
					return nil
				}
				return fmt.Errorf("can't find session")
			}
			if !alive {
				return fmt.Errorf("send-keys failed")
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}
	session := newTmuxSession(toTmuxName("io", ""), "claude", NewMockPtyFactory(t), cmdExec)
	return cmds, tap(session)
}

func TestTapEnterSendsClientlessEnterKey(t *testing.T) {
	cmds, err := recordTapCommands(t, true, (*TmuxSession).TapEnter)
	require.NoError(t, err)
	require.Contains(t, cmds, "tmux send-keys -t =af_io: Enter",
		"TapEnter must inject a bare Enter via a clientless send-keys command; got %v", cmds)
}

func TestTapDAndEnterSendsClientlessDThenEnter(t *testing.T) {
	cmds, err := recordTapCommands(t, true, (*TmuxSession).TapDAndEnter)
	require.NoError(t, err)
	require.Contains(t, cmds, "tmux send-keys -t =af_io: D Enter",
		"TapDAndEnter must inject 'D' then Enter via a clientless send-keys command; got %v", cmds)
}

func TestTapEnterReturnsErrSessionGoneWhenSessionMissing(t *testing.T) {
	_, err := recordTapCommands(t, false, (*TmuxSession).TapEnter)
	require.ErrorIs(t, err, ErrSessionGone,
		"a send-keys failure against a vanished session must surface ErrSessionGone, not a bare error")
}

func TestTapDAndEnterReturnsErrSessionGoneWhenSessionMissing(t *testing.T) {
	_, err := recordTapCommands(t, false, (*TmuxSession).TapDAndEnter)
	require.ErrorIs(t, err, ErrSessionGone)
}

func TestReadPaneCursorStateCarriesVisibilityWithoutChangingTerminalModes(t *testing.T) {
	for _, tt := range []struct {
		name    string
		wire    string
		visible bool
	}{
		{name: "visible", wire: "7 11 1", visible: true},
		{name: "hidden", wire: "7 11 0", visible: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requestedFormat string
			cmdExec := cmd_test.MockCmdExec{
				OutputFunc: func(c *exec.Cmd) ([]byte, error) {
					requestedFormat = c.Args[len(c.Args)-1]
					return []byte(tt.wire), nil
				},
			}
			session := newTmuxSession("af_cursor", ProgramClaude, NewMockPtyFactory(t), cmdExec)

			state, err := session.readPaneCursorState()
			require.NoError(t, err)
			require.Equal(t, paneCursorState{Row: 7, Col: 11, Visible: tt.visible}, state)
			require.Equal(t, paneCursorStateFormat, requestedFormat,
				"cursor visibility must ride its own focused query, not alter terminalStateFormat")
		})
	}
}
