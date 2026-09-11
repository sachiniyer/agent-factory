package tmux

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// The #703b4a70 fix latches ProvenNoPane at the conclusive return of
// closeAndWaitForPaneExit, so a redundant teardown (respawnFresh's outer
// stopForAccountSwap after finishRecoverTabFailure's inner close already
// killed the pane) skips re-closing the dead session instead of
// re-classifying it blind and wrapping ErrAccountSwapAgentTeardownBlind.
//
// These tests pin the latch and its scope directly on TmuxSession:
//   - conclusive AND observed (blind=false)  -> latch
//   - conclusive but blind (blind=true)      -> no latch (ancestry lost: #2998)
//   - inconclusive (PaneStateUnknown)       -> no latch (backstop must still run)

// TestCloseAndWaitForPaneExit_ConclusiveNonBlindLatchesProvenNoPane is the latch
// itself. A teardown that named the pane (display-message returned a pid) and
// captured its process tree before kill-session (list-panes returned an empty
// descendant set, so captureErr is nil) concluded the session is gone with the
// pane observed: blind is false. That conclusive, observed teardown leaves
// nothing behind the name, so ProvenNoPane latches and a redundant close is
// skipped instead of re-classifying the dead session blind.
func TestCloseAndWaitForPaneExit_ConclusiveNonBlindLatchesProvenNoPane(t *testing.T) {
	// A real, fully-reaped pane PID: tmux answered display-message with it, and
	// capturePaneProcess's pre-kill identity check resolves it as already gone
	// (syscall.Kill returns ESRCH) - the production outcome for a just-started
	// session whose pane dies on kill-session's SIGHUP. See exitedProcess in
	// close_wait_test.go for the same fixture shape.
	pid := exitedProcess(t).PID
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "display-message") {
				return []byte(fmt.Sprintf("%d\n", pid)), nil
			}
			// list-panes answers with an empty pane set: the replacement pane had
			// no descendants, so captureSessionProcessTrees returns (nil, nil) and
			// blind is false.
			return []byte(""), nil
		},
	}
	s := newTmuxSession(toTmuxName("close-conclusive-latch", ""), "claude", NewMockPtyFactory(t), cmdExec)
	require.False(t, s.ProvenNoPane(), "a fresh session has proved nothing about its pane")

	state, blind, err := s.CloseAndWaitForPaneExitReportingBlindness()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.False(t, blind, "a captured, no-survivor teardown of a live session observed the pane")
	require.True(t, s.ProvenNoPane(),
		"a conclusive non-blind teardown latches the pane-gone proof so the redundant close is skipped")
}

// TestCloseAndWaitForPaneExit_BlindConclusiveDoesNotLatchProvenNoPane pins the
// scope: a teardown that concluded the session is gone WITHOUT ever observing a
// pane (blind is true - display-message named none and list-panes proved the
// session vanished) lost the pane's ancestry to the marker scan and must NOT
// latch ProvenNoPane. A later destructive teardown still needs its occupancy
// check (#2998), and a later stopForAccountSwap still needs to re-check.
func TestCloseAndWaitForPaneExit_BlindConclusiveDoesNotLatchProvenNoPane(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	name := toTmuxName("close-blind-no-latch", "")
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "list-panes") {
				// tmux's real missing-session answer: exit 1 with the exact
				// diagnostic, so captureSessionProcessTrees maps it to
				// ErrSessionVanishedBeforeCapture.
				return nil, tmuxCantFindSessionError(t, name)
			}
			// display-message answers exit 0 with empty output for a session that
			// does not exist, so panePID yields errPaneQueryFoundNoPane.
			return nil, nil
		},
	}
	s := newTmuxSession(name, "claude", NewMockPtyFactory(t), cmdExec)

	state, blind, err := s.CloseAndWaitForPaneExitReportingBlindness()
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.True(t, blind, "a session gone with no pane observed is blind")
	require.False(t, s.ProvenNoPane(),
		"a blind close lost the ancestry and must not latch the pane-gone proof")
}

// TestCloseAndWaitForPaneExit_InconclusiveDoesNotLatchProvenNoPane pins the
// other half of the scope: an inconclusive teardown (PaneStateUnknown) latches
// nothing, so the backstop close in stopForAccountSwap still runs on the
// ts.Start-failure path where no conclusive inner close ever ran.
func TestCloseAndWaitForPaneExit_InconclusiveDoesNotLatchProvenNoPane(t *testing.T) {
	// list-panes answers with an unparseable pane set: captureSessionProcessTrees
	// cannot establish the process tree, so the close refuses with
	// PaneStateUnknown rather than latching absence.
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("not-a-pane-pid\n"), nil
		},
	}
	s := newTmuxSession(toTmuxName("close-inconclusive-no-latch", ""), "claude", NewMockPtyFactory(t), cmdExec)

	state, _, err := s.CloseAndWaitForPaneExitReportingBlindness()
	require.Error(t, err)
	require.Equal(t, PaneStateUnknown, state, "an unreadable pane set refuses rather than latching absence")
	require.False(t, s.ProvenNoPane(),
		"an inconclusive teardown proves nothing and must not latch the pane-gone proof")
}
