package tmux

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// A tmux server that holds NO sessions answers every targeted query with `no
// current target` (#3469). cmd_find_target fails to establish a current session
// before it ever looks at `-t`, so the diagnostic names neither the session nor
// a socket, and the two spellings missingTmuxSession matches — `can't find
// session: <name>` and `no server running on <socket>` — both miss it.
//
// The tests below hold the server in that state deliberately with `exit-empty
// off`, tmux's documented option for keeping a server up with no sessions. That
// is not a contrivance to reach a corner: a server passes through the same state
// on its way out after its last session is killed, which is the intermittent form
// this suite kept failing on, and users who set exit-empty off live there
// permanently. Pinning it makes the classification testable without a race.

// sessionlessTmuxServer leaves the test's private tmux server running with zero
// sessions and returns a session name that is definitively not on it.
func sessionlessTmuxServer(t *testing.T) string {
	t.Helper()
	testguard.IsolateTmux(t)

	const name = "af_sessionless_server_probe"
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session: %s", out)
	// Server option: without it the server exits with its last session and the
	// probes below would race the shutdown instead of observing the state.
	out, err = exec.Command("tmux", "set", "-s", "exit-empty", "off").CombinedOutput()
	require.NoError(t, err, "tmux set -s exit-empty off: %s", out)
	out, err = exec.Command("tmux", "kill-session", "-t", exactTarget(name)).CombinedOutput()
	require.NoError(t, err, "tmux kill-session: %s", out)

	// The precondition, asserted rather than assumed: the server is still up
	// (an authoritative listing) and holds nothing.
	names, err := ListSessionNames(cmd.MakeExecutor())
	require.NoError(t, err, "the private server must still answer after its last session was killed")
	require.Empty(t, names, "the private server must hold no sessions")
	return name
}

// probeSessionStrict is the gate CleanupSessions consults before it may treat a
// session as gone. On a sessionless server it used to report UNKNOWN, so `af
// reset` refused to clean up a session that was definitively absent.
func TestProbeSessionStrictConfirmsAbsenceOnSessionlessServer(t *testing.T) {
	name := sessionlessTmuxServer(t)

	exists, known, err := probeSessionStrict(cmd.MakeExecutor(), name)
	require.NoError(t, err, "a server that answered must not leave the session unknown")
	require.True(t, known, "a server holding no sessions has definitively answered about this one")
	require.False(t, exists)
}

// The same answer reaches the pre-teardown capture. Misclassified, it degrades
// to a generic "cannot list panes" failure, and the vanished-session recovery
// that sweeps marked orphans never runs.
func TestCaptureSessionProcessTreesReportsVanishedOnSessionlessServer(t *testing.T) {
	name := sessionlessTmuxServer(t)

	procs, err := captureSessionProcessTrees(cmd.MakeExecutor(), name)
	require.Empty(t, procs)
	require.ErrorIs(t, err, ErrSessionVanishedBeforeCapture,
		"a session absent from a server that answered must classify as vanished, not as an unreadable capture")
}

// The corroboration contract, without tmux: an exit-1 answer this package cannot
// classify is resolved by tmux's own listing, and ONLY by an authoritative one.
func TestTmuxProvedSessionAbsentCorroboratesWithTheSessionListing(t *testing.T) {
	const name = "af_corroborated"

	for _, tc := range []struct {
		label    string
		failure  error
		listing  []byte
		listErr  error
		expected bool
	}{
		{
			label:    "unclassified exit 1, listing omits the session",
			failure:  tmuxExitOneError(t, "no current target"),
			listing:  []byte(""),
			expected: true,
		},
		{
			label:    "unclassified exit 1, listing still holds the session",
			failure:  tmuxExitOneError(t, "no current target"),
			listing:  []byte("af_other\n" + name + "\n"),
			expected: false,
		},
		{
			label:    "unclassified exit 1, listing names other sessions only",
			failure:  tmuxExitOneError(t, "server exited unexpectedly"),
			listing:  []byte("af_other\naf_" + name + "\n" + name + "_suffix\n"),
			expected: true,
		},
		{
			label:   "unclassified exit 1, listing did not answer",
			failure: tmuxExitOneError(t, "no current target"),
			listErr: errors.New("injected listing failure"),
		},
		{
			label:    "tmux's own absence diagnostic still needs no listing",
			failure:  tmuxExitOneError(t, "can't find session: "+name),
			listErr:  errors.New("the listing must not be consulted"),
			expected: true,
		},
		{
			label:   "a status tmux documents no diagnostic for is never absence",
			failure: tmuxExitTwoError(t, "no current target"),
			listing: []byte(""),
		},
		{
			label:   "not an exit error at all",
			failure: errors.New("fork/exec tmux: resource temporarily unavailable"),
			listing: []byte(""),
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			cmdExec := cmd_test.MockCmdExec{
				RunFunc: func(*exec.Cmd) error { return nil },
				OutputFunc: func(command *exec.Cmd) ([]byte, error) {
					require.Equal(t, "ls", command.Args[1],
						"only the session listing may be issued to corroborate a failed read")
					return tc.listing, tc.listErr
				},
			}
			require.Equal(t, tc.expected, tmuxProvedSessionAbsent(cmdExec, tc.failure, name))
		})
	}
}

// The unclassifiable answer must still surface WHAT tmux said. An
// (*exec.ExitError).Error() is only "exit status 1", and #3469 spent a
// round trip on a refusal that reported exactly that and nothing else.
func TestProbeSessionStrictNamesTheDiagnosticItCouldNotClassify(t *testing.T) {
	const name = "af_unclassifiable"
	failure := tmuxExitOneError(t, "error connecting to /run/tmux/x (Permission denied)")
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, failure },
	}

	exists, known, err := probeSessionStrict(cmdExec, name)
	require.False(t, exists)
	require.False(t, known, "a listing that did not answer either leaves the session unknown")
	require.ErrorContains(t, err, "Permission denied",
		"the refusal must name what tmux said, not only its exit status")
}

// tmuxExitOneError builds a genuine *exec.ExitError carrying diagnostic on
// stderr, which is the only place tmux's reason ever lives.
func tmuxExitOneError(t *testing.T, diagnostic string) error {
	t.Helper()
	return exitErrorWithStderr(t, 1, diagnostic)
}

func tmuxExitTwoError(t *testing.T, diagnostic string) error {
	t.Helper()
	return exitErrorWithStderr(t, 2, diagnostic)
}

func exitErrorWithStderr(t *testing.T, code int, diagnostic string) error {
	t.Helper()
	command := exec.Command("sh", "-c", `printf '%s\n' "$1" >&2; exit "$2"`,
		"sh", diagnostic, strconv.Itoa(code))
	_, err := command.Output()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, code, exitErr.ExitCode())
	require.Equal(t, diagnostic, strings.TrimSpace(string(exitErr.Stderr)),
		"the fixture must reproduce tmux's stderr exactly")
	return err
}
