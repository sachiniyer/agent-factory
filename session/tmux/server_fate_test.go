package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
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
	// original is what the creation-time pid names now. Not consulted for
	// the same-server case.
	original    serverProcessState
	want        serverFate
	contains    string
	notContains []string
}

const (
	refusedDiag = "no server running on /tmp/af-tmux-1/tmux-1000/default"
	missingDiag = "error connecting to /tmp/af-tmux-1/tmux-1000/default (No such file or directory)"
)

func refused(t *testing.T) error { return tmuxExitOneError(t, refusedDiag) }
func missing(t *testing.T) error { return tmuxExitOneError(t, missingDiag) }

// The socket alone never proves an exit (#2875, ServerPIDOnSocket): every case
// whose original server is not shown exited must not say it exited or that the
// session died with it.
var noExitClaim = []string{"has exited", "died with", "the server died"}

func fateCases() []fateCase {
	return []fateCase{
		{
			name: "the same server still answers: only the pane died", serverPID: "111", want: serverFateSameServer,
			contains: "the tmux server it was created on (pid 111) is still running, so only the session's own pane exited",
		},

		{
			name: "nothing listens, original exited", serverErr: refused, original: serverProcessExited, want: serverFateNoServer,
			contains: "the tmux server it was created on (pid 111) has exited, and nothing answers on its socket (" + refusedDiag + ")",
		},
		{
			name: "nothing listens, original still a live tmux process", serverErr: refused, original: serverProcessRunning, want: serverFateNoServer,
			contains:    "nothing answers on its socket (" + refusedDiag + "), though the tmux server it was created on (pid 111) is still a live tmux process",
			notContains: noExitClaim,
		},
		{
			name: "nothing listens, original unknown", serverErr: refused, original: serverProcessUnknown, want: serverFateNoServer,
			contains:    "nothing answers on the socket of the tmux server it was created on (pid 111) (" + refusedDiag + ")",
			notContains: noExitClaim,
		},

		{
			name: "socket missing, original exited", serverErr: missing, original: serverProcessExited, want: serverFateUnreachable,
			contains: "the tmux server it was created on (pid 111) has exited, and its socket is missing (" + missingDiag + ")",
		},
		{
			name: "socket missing, original still a live tmux process", serverErr: missing, original: serverProcessRunning, want: serverFateUnreachable,
			contains:    "is still a live tmux process, but its socket is missing (" + missingDiag + "): the socket was removed while the server keeps running",
			notContains: noExitClaim,
		},
		{
			name: "socket missing, original unknown", serverErr: missing, original: serverProcessUnknown, want: serverFateUnreachable,
			contains:    "the socket of the tmux server it was created on (pid 111) is missing (" + missingDiag + "), so that server cannot be reached",
			notContains: noExitClaim,
		},

		{
			name: "another server answers, original exited", serverPID: "222", original: serverProcessExited, want: serverFateReplaced,
			contains: "the tmux server it was created on (pid 111) has exited, and a different tmux server (pid 222) now answers on its socket",
		},
		{
			name: "another server answers, original still a live tmux process", serverPID: "222", original: serverProcessRunning, want: serverFateReplaced,
			contains:    "a different tmux server (pid 222) now answers on its socket, though the tmux server it was created on (pid 111) is still a live tmux process: the session is out of reach, not proven gone",
			notContains: noExitClaim,
		},
		{
			name: "another server answers, original unknown", serverPID: "222", original: serverProcessUnknown, want: serverFateReplaced,
			contains:    "a different tmux server (pid 222) now answers on the socket of the tmux server it was created on (pid 111)",
			notContains: noExitClaim,
		},
	}
}

// stageOriginalServer pins what the creation-time pid names, so a fake pid in a
// mock tmux never consults the real process table.
func stageOriginalServer(t *testing.T, state serverProcessState) *int {
	t.Helper()
	calls := 0
	prev := serverProcessStateOf
	serverProcessStateOf = func(pid string) serverProcessState {
		calls++
		require.Equal(t, "111", pid, "liveness is read for the creation-time pid")
		return state
	}
	t.Cleanup(func() { serverProcessStateOf = prev })
	return &calls
}

func (tc fateCase) tmux(t *testing.T) (cmd_test.MockCmdExec, *int) {
	t.Helper()
	var serverErr error
	if tc.serverErr != nil {
		serverErr = tc.serverErr(t)
	}
	return fateTmux(t, tc.serverPID, serverErr)
}

func (tc fateCase) check(t *testing.T, text string) {
	t.Helper()
	require.Contains(t, text, tc.contains)
	for _, claim := range tc.notContains {
		require.NotContains(t, text, claim)
	}
}

// Each answer #6581 needs, and the ones that claim nothing.
func TestServerFateSince(t *testing.T) {
	for _, tc := range fateCases() {
		t.Run(tc.name, func(t *testing.T) {
			calls := stageOriginalServer(t, tc.original)
			exec, _ := tc.tmux(t)
			session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
			report := session.serverFateSince("111")
			require.Equal(t, tc.want, report.fate)
			tc.check(t, report.clause())
			if tc.want == serverFateSameServer {
				require.Zero(t, *calls, "the answering pid is the answer; the process table is not read")
			}
		})
	}

	t.Run("an unrelated failure claims nothing", func(t *testing.T) {
		stageOriginalServer(t, serverProcessExited)
		exec, _ := fateTmux(t, "", tmuxExitOneError(t, "server exited unexpectedly"))
		session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
		report := session.serverFateSince("111")
		require.Equal(t, serverFateUnknown, report.fate)
		require.Empty(t, report.clause())
	})
	t.Run("no creation-time pid: no probe, nothing claimed", func(t *testing.T) {
		calls := stageOriginalServer(t, serverProcessExited)
		exec, probes := fateTmux(t, "111", nil)
		session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
		report := session.serverFateSince("")
		require.Equal(t, serverFateUnknown, report.fate)
		require.Zero(t, *probes, "without a pid to compare there is nothing to ask the server")
		require.Zero(t, *calls)
	})
}

// The real liveness read: this process is alive and not tmux (as a recycled
// pid would be, so its server is gone), a child that ran and was reaped is
// gone, and a non-pid is unknown.
func TestServerProcessStateOf(t *testing.T) {
	require.Equal(t, serverProcessExited, serverProcessStateOf(strconv.Itoa(os.Getpid())),
		"a pid that names a non-tmux program no longer names the server")
	reaped := exec.Command("true")
	require.NoError(t, reaped.Run())
	require.Equal(t, serverProcessExited, serverProcessStateOf(strconv.Itoa(reaped.ProcessState.Pid())),
		"a reaped pid names no process")
	require.Equal(t, serverProcessUnknown, serverProcessStateOf("not-a-pid"))
	require.Equal(t, serverProcessUnknown, serverProcessStateOf(""))
}

// Start's attach found the session gone: the error names what died instead of
// guessing that the program exited.
func TestVanishedBeforeAttachErrorNamesWhatDied(t *testing.T) {
	for _, tc := range fateCases() {
		t.Run(tc.name, func(t *testing.T) {
			stageOriginalServer(t, tc.original)
			exec, _ := tc.tmux(t)
			session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
			cause := errors.New(`tmux session "af_fate" does not exist`)
			err := session.vanishedBeforeAttachError(session.serverFateSince("111"), cause)
			require.ErrorIs(t, err, cause)
			msg := err.Error()
			require.Contains(t, msg, "tmux session af_fate vanished before attach")
			require.Contains(t, msg, `(program: "claude")`)
			tc.check(t, msg)
			if tc.want == serverFateReplaced {
				require.NotContains(t, msg, "check that it runs",
					"another server on the socket is not the program's fault")
				if tc.original == serverProcessExited {
					require.Contains(t, msg, "so the session died with the server that held it")
				}
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
			stageOriginalServer(t, tc.original)
			exec, _ := tc.tmux(t)
			session := newTmuxSession("af_fate", "claude", NewMockPtyFactory(t), exec)
			session.monitor = newStatusMonitor()
			session.monitor.generation = &tmuxGeneration{sessionID: "$3", serverPID: "111", created: "1700000000"}

			_, err := session.CapturePaneContentContext(context.Background())
			require.ErrorIs(t, err, ErrSessionGone)
			require.Contains(t, err.Error(), "tmux session no longer exists: capture-pane")
			tc.check(t, err.Error())
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
