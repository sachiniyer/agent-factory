package tmux

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// fateTmux is a tmux whose session is gone and whose server answers
// display-message -p '#{pid}' as the case says: a pid, or tmux's no-server
// diagnostic, or an unrelated failure.
func fateTmux(t *testing.T, serverPID string, serverErr error) (cmd_test.MockCmdExec, *int) {
	t.Helper()
	pidProbes := 0
	answer := func(c *exec.Cmd) ([]byte, error) {
		args := strings.Join(c.Args, " ")
		switch {
		case strings.Contains(args, "display-message") && strings.Contains(args, "#{pid}") && !strings.Contains(args, "session_id"):
			pidProbes++
			if serverErr != nil {
				return nil, serverErr
			}
			return []byte(serverPID + "\n"), nil
		case strings.Contains(args, "has-session"):
			return nil, tmuxExitOneError(t, "can't find session: af_fate")
		default:
			// capture-pane and anything else: tmux answered that the target is gone.
			return nil, tmuxExitOneError(t, "can't find pane: af_fate")
		}
	}
	return cmd_test.MockCmdExec{
		RunFunc:    func(c *exec.Cmd) error { _, err := answer(c); return err },
		OutputFunc: answer,
	}, &pidProbes
}

type fateCase struct {
	name      string
	serverPID string
	serverErr func(t *testing.T) error
	want      serverFate
	contains  string
}

func fateCases() []fateCase {
	return []fateCase{
		{
			name: "the same server still answers: only the pane died", serverPID: "111", want: serverFateSameServer,
			contains: "the tmux server it was created on (pid 111) is still running, so only the session's own pane exited",
		},
		{
			name: "nothing answers on the socket: the server exited",
			serverErr: func(t *testing.T) error {
				return tmuxExitOneError(t, "no server running on /tmp/af-tmux-1/tmux-1000/default")
			},
			want:     serverFateNoServer,
			contains: "the tmux server it was created on (pid 111) no longer answers (no server running on /tmp/af-tmux-1/tmux-1000/default): the server exited",
		},
		{
			name: "the socket is gone: the server exited",
			serverErr: func(t *testing.T) error {
				return tmuxExitOneError(t, "error connecting to /tmp/af-tmux-1/tmux-1000/default (No such file or directory)")
			},
			want:     serverFateNoServer,
			contains: "no longer answers (error connecting to /tmp/af-tmux-1/tmux-1000/default (No such file or directory))",
		},
		{
			name: "a different server answers: the server was replaced", serverPID: "222", want: serverFateReplaced,
			contains: "the tmux server it was created on (pid 111) is gone: a different tmux server (pid 222) now answers on its socket",
		},
	}
}

// Each of the three answers #6581 needs, and the one that claims nothing.
func TestServerFateSince(t *testing.T) {
	for _, tc := range fateCases() {
		t.Run(tc.name, func(t *testing.T) {
			var serverErr error
			if tc.serverErr != nil {
				serverErr = tc.serverErr(t)
			}
			exec, _ := fateTmux(t, tc.serverPID, serverErr)
			session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
			report := session.serverFateSince("111")
			require.Equal(t, tc.want, report.fate)
			require.Contains(t, report.clause(), tc.contains)
		})
	}

	t.Run("an unrelated failure claims nothing", func(t *testing.T) {
		exec, _ := fateTmux(t, "", tmuxExitOneError(t, "server exited unexpectedly"))
		session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
		report := session.serverFateSince("111")
		require.Equal(t, serverFateUnknown, report.fate)
		require.Empty(t, report.clause())
	})
	t.Run("no creation-time pid: no probe, nothing claimed", func(t *testing.T) {
		exec, probes := fateTmux(t, "111", nil)
		session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
		report := session.serverFateSince("")
		require.Equal(t, serverFateUnknown, report.fate)
		require.Zero(t, *probes, "without a pid to compare there is nothing to ask the server")
	})
}

// Start's attach found the session gone: the error names what died instead of
// guessing that the program exited.
func TestVanishedBeforeAttachErrorNamesWhatDied(t *testing.T) {
	for _, tc := range fateCases() {
		t.Run(tc.name, func(t *testing.T) {
			var serverErr error
			if tc.serverErr != nil {
				serverErr = tc.serverErr(t)
			}
			exec, _ := fateTmux(t, tc.serverPID, serverErr)
			session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
			cause := errors.New(`tmux session "af_fate" does not exist`)
			err := session.vanishedBeforeAttachError(session.serverFateSince("111"), cause)
			require.ErrorIs(t, err, cause)
			msg := err.Error()
			require.Contains(t, msg, "tmux session af_fate vanished before attach")
			require.Contains(t, msg, tc.contains)
			require.Contains(t, msg, `(program: "claude")`)
			if tc.want == serverFateReplaced {
				require.NotContains(t, msg, "check that it runs",
					"a replaced server is not the program's fault")
			}
		})
	}

	t.Run("unknown keeps the pre-#6581 wording", func(t *testing.T) {
		exec, _ := fateTmux(t, "", nil)
		session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
		err := session.vanishedBeforeAttachError(serverFateReport{}, errors.New("gone"))
		require.Equal(t,
			`tmux session af_fate vanished before attach; the pane program likely exited immediately after launch — check that it runs and accepts its flags (program: "claude"): gone`,
			err.Error())
	})
}

// The readiness poll's capture found the session gone (#6581 sightings 3 and
// 4): the ErrSessionGone it returns names what died, against the server pid
// the monitor was bound to when Start's attach confirmed the session.
func TestCaptureGoneErrorNamesWhatDied(t *testing.T) {
	for _, tc := range fateCases() {
		t.Run(tc.name, func(t *testing.T) {
			var serverErr error
			if tc.serverErr != nil {
				serverErr = tc.serverErr(t)
			}
			exec, _ := fateTmux(t, tc.serverPID, serverErr)
			session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
			session.monitor = newStatusMonitor()
			session.monitor.generation = &tmuxGeneration{sessionID: "$3", serverPID: "111", created: "1700000000"}

			_, err := session.CapturePaneContentContext(context.Background())
			require.ErrorIs(t, err, ErrSessionGone)
			require.Contains(t, err.Error(), "tmux session no longer exists: capture-pane")
			require.Contains(t, err.Error(), tc.contains)
		})
	}

	t.Run("an unbound monitor adds nothing and asks nothing", func(t *testing.T) {
		exec, probes := fateTmux(t, "111", nil)
		session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
		session.monitor = newStatusMonitor()

		_, err := session.CapturePaneContentContext(context.Background())
		require.ErrorIs(t, err, ErrSessionGone)
		require.NotContains(t, err.Error(), "tmux server it was created on")
		require.Zero(t, *probes)
	})
}
