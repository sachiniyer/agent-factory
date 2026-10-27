package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// These tests pin the error classification of the EXPORTED
// CaptureSessionProcessTrees — the evidence-bearing half that `af doctor`'s
// escaped-process and runaway-CPU checks now consume. The doctor's live-session
// arm treats ANY non-nil error from this call as blindness (it cannot answer
// membership), but that is only correct because the errors that reach it are the
// LIVE-session "could not read" form and NOT a vanished classification: a session
// that `tmux ls` no longer names is dead, so it never enters the live arm at all.
//
// The three vectors below are the production-plausible non-vanished failures —
// a plain (non-ExitError) failure, a tripped list-panes deadline, and an
// unclassified exit 1 corroborated by a listing that still names the session.
// Each must collapse to the GENERIC "cannot list panes before teardown" branch
// (never ErrSessionVanishedBeforeCapture), so a still-present session's failed
// pane read is reported as blindness rather than misclassified as a vanished
// session.

// TestCaptureDegradesPlainListPanesErrorToGenericNotVanished: a plain fmt.Errorf
// from list-panes is the live-session generic-failure form. Its text deliberately
// coincides with the vanished string to prove the classification comes from the
// error SHAPE (it is not an *exec.ExitError, so missingTmuxSession rejects it),
// not a substring match on Error().
func TestCaptureDegradesPlainListPanesErrorToGenericNotVanished(t *testing.T) {
	const name = "af_present-but-list-panes-fails"
	plainErr := fmt.Errorf("can't find session: %s", name)
	require.NotErrorIs(t, plainErr, ErrSessionVanishedBeforeCapture)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, plainErr },
	}
	procs, err := CaptureSessionProcessTrees(cmdExec, name)
	require.Empty(t, procs)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSessionVanishedBeforeCapture,
		"a plain (non-ExitError) list-panes failure must not be read as a vanished session")
	require.ErrorContains(t, err, "cannot list panes before teardown")
}

// TestCaptureTimeoutListPanesIsGenericNotVanished: a tripped tmuxCommandTimeout
// on list-panes takes the ErrTmuxTimeout branch BEFORE tmuxProvedSessionAbsent is
// consulted. A timeout is not a vanished session; it is an unreadable capture.
func TestCaptureTimeoutListPanesIsGenericNotVanished(t *testing.T) {
	const name = "af_wedged-list-panes"
	shortTmuxTimeout(t, 50*time.Millisecond)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			time.Sleep(200 * time.Millisecond) // outlast the shortened deadline
			return nil, errors.New("injected list-panes failure")
		},
	}

	procs, err := CaptureSessionProcessTrees(cmdExec, name)
	require.Empty(t, procs, "a timed-out capture returns no processes")
	require.ErrorIs(t, err, ErrTmuxTimeout,
		"a tripped list-panes deadline must surface as ErrTmuxTimeout")
	require.NotErrorIs(t, err, ErrSessionVanishedBeforeCapture,
		"a timeout is not a vanished session; it is an unreadable capture")
}

// TestCaptureUnclassifiedExitOneOnLiveSessionIsGenericNotVanished: an
// *exec.ExitError with exit 1 and a diagnostic that is neither "can't find
// session: <name>" nor a no-server form is routed by tmuxProvedSessionAbsent to
// ListSessionNames. When the listing succeeds and still contains the session
// name, tmuxProvedSessionAbsent returns false, so CaptureSessionProcessTrees
// takes the generic branch. This is the verifiably-live-session form: `tmux ls`
// answered authoritatively and the session is present.
func TestCaptureUnclassifiedExitOneOnLiveSessionIsGenericNotVanished(t *testing.T) {
	const name = "af_present-but-list-panes-fails"

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) > 1 && c.Args[1] == "list-panes" {
				return nil, tmuxExitOneError(t, "server exited unexpectedly")
			}
			return []byte("af_other\n" + name + "\n"), nil // ls still names the session
		},
	}

	procs, err := CaptureSessionProcessTrees(cmdExec, name)
	require.Empty(t, procs, "a non-vanished failure returns no processes")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSessionVanishedBeforeCapture,
		"an unclassified exit-1 corroborated by a listing that still names the session is not vanished")
	require.NotErrorIs(t, err, ErrTmuxTimeout,
		"no deadline was tripped; this is a generic command failure")
	require.ErrorContains(t, err, "cannot list panes before teardown",
		"the generic branch names the failure rather than laundering it as absence")
}
