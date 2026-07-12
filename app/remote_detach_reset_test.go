package app

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// swapRemoteDetachResetWriter points the post-remote-detach mode re-assert at
// a buffer for the duration of a test.
func swapRemoteDetachResetWriter(t *testing.T, w io.Writer) {
	t.Helper()
	prev := remoteDetachResetWriter
	remoteDetachResetWriter = w
	t.Cleanup(func() { remoteDetachResetWriter = prev })
}

// runAttachOverlayCallback drives the blocking attach lifecycle helper off
// the test goroutine, simulates a detach by closing ch, and returns the
// post-detach cmd.
func runAttachOverlayCallback(t *testing.T, h *home) tea.Cmd {
	t.Helper()
	ch := make(chan struct{})
	done := make(chan tea.Cmd, 1)
	go func() {
		done <- h.attachOverlayCallback("t1", "test-attach", "", func() (chan struct{}, error) {
			return ch, nil
		})
	}()
	require.Eventually(t, func() bool { return h.attached.Load() },
		time.Second, time.Millisecond, "attached flag must arm before <-ch blocks")
	close(ch)
	select {
	case cmd := <-done:
		return cmd
	case <-time.After(2 * time.Second):
		t.Fatalf("attachOverlayCallback did not return after detach")
		return nil
	}
}

func runAttachTransitionCmd(t *testing.T, h *home, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	var postAttachCmd tea.Cmd
	for _, msg := range drainCmd(t, cmd, time.Second) {
		if begin, ok := msg.(beginAttachMsg); ok {
			_, postAttachCmd = h.Update(begin)
		}
	}
	return postAttachCmd
}

func TestBeginAttachTransitionClearsFrameBeforeAttachStarts(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 80, 24)

	attachStarted := false
	cmd := h.beginAttachTransition(func() tea.Cmd {
		attachStarted = true
		return func() tea.Msg { return repaintAfterDetachMsg{} }
	})

	require.True(t, h.attachTransitioning, "attach transition must blank the next pre-attach frame")
	frame := h.View()
	require.Equal(t, 24, strings.Count(frame, "\n")+1,
		"pre-attach blank frame must cover the full terminal height")
	require.NotContains(t, frame, "help", "pre-attach frame must not repaint AF footer chrome")

	begin, ok := cmd().(beginAttachMsg)
	require.True(t, ok, "transition command must dispatch beginAttachMsg")

	_, postAttachCmd := h.Update(begin)
	require.True(t, attachStarted, "attach callback must start after the blank pre-attach frame")
	require.NotNil(t, postAttachCmd)
	_, isRepaint := postAttachCmd().(repaintAfterDetachMsg)
	require.True(t, isRepaint, "beginAttachMsg must return the attach callback's cmd")
}

// TestAttachOverlayCallback_ReassertsTerminalModes is the app half of the #845
// fix. After a full-screen attach returns, the attach driver (local WS PTY
// stream or remote hook) has handed the terminal back in a neutral state (main
// screen, cursor visible, all reporting modes off — see tmux.NeutralTerminalRestore),
// which is NOT the state this TUI's renderer assumes. The callback must re-assert
// bubbletea's startup modes synchronously — while the Update goroutine is still
// blocked here, before the renderer can emit a frame — and then route through
// tea.ClearScreen + the usual repaintAfterDetachMsg flow so the stale diff cache
// is invalidated and the screen fully repainted. Since #1592 Phase 2 PR7 this is
// uniform: local attach is a WS byte proxy that scribbles the terminal exactly
// like remote, so there is no longer a "local skips the reassert" path.
func TestAttachOverlayCallback_ReassertsTerminalModes(t *testing.T) {
	resetDetachWatchdog(t)
	h := newTestHome(t)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := runAttachOverlayCallback(t, h)
	require.NotNil(t, cmd)

	// The re-assert was written before the callback returned (i.e. before the
	// bubbletea event loop could resume), in full.
	require.Equal(t, remoteDetachTerminalReassert, out.String(),
		"detach must synchronously re-assert the TUI's terminal modes")
	for _, seq := range []struct{ esc, what string }{
		{"\x1b[?1049h", "re-enter the alt screen"},
		{"\x1b[?25l", "re-hide the cursor"},
		{"\x1b[?1002h", "re-enable cell-motion mouse"},
		{"\x1b[?1006h", "re-enable SGR mouse encoding"},
		{"\x1b[?2004h", "re-enable bracketed paste"},
	} {
		assert.Contains(t, out.String(), seq.esc,
			"re-assert must %s — bubbletea set this at startup and cannot "+
				"re-assert state its renderer believes is already active", seq.what)
	}

	// The returned cmd is tea.Sequence(tea.ClearScreen, repaint). sequenceMsg
	// is unexported, so unpack it reflectively: it is a slice of tea.Cmd.
	msg := cmd()
	seq := reflect.ValueOf(msg)
	require.Equal(t, reflect.Slice, seq.Kind(),
		"remote post-detach cmd must be a tea.Sequence, got %T", msg)
	require.Equal(t, 2, seq.Len(), "sequence must be exactly ClearScreen + repaint")

	first, ok := seq.Index(0).Interface().(tea.Cmd)
	require.True(t, ok)
	assert.Equal(t, tea.ClearScreen(), first(),
		"first sequenced cmd must be tea.ClearScreen so the renderer's stale "+
			"diff cache is invalidated before the repaint (#845)")

	second, ok := seq.Index(1).Interface().(tea.Cmd)
	require.True(t, ok)
	_, isRepaint := second().(repaintAfterDetachMsg)
	assert.True(t, isRepaint,
		"second sequenced cmd must emit repaintAfterDetachMsg — the remote "+
			"path converges on the same repaint flow (and #683 watchdog "+
			"semantics) as a local detach")

	require.False(t, h.attached.Load(), "attached flag must clear on the remote path too")
	endDetachWatchdog()
}

// TestAttachOverlayCallback_LocalAlsoReassertsTerminalModes pins the #1592
// Phase 2 PR7 behavior change: local full-screen attach is now a WS PTY byte
// proxy (apiclient.AttachStream), not a long-lived tmux render client, so it
// scribbles the terminal's alt-screen/mouse/scroll modes and hands it back
// neutral on detach — exactly like the remote path. The local detach therefore
// takes the SAME reassert + ClearScreen path (previously it skipped the reset);
// otherwise the TUI repaints into a stale scroll region (#845 reproduced
// locally). This test is intentionally the local-side sibling of the reassert
// test above — the callback no longer distinguishes local from remote.
func TestAttachOverlayCallback_LocalAlsoReassertsTerminalModes(t *testing.T) {
	resetDetachWatchdog(t)
	h := newTestHome(t)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := runAttachOverlayCallback(t, h)
	require.NotNil(t, cmd)

	require.Equal(t, remoteDetachTerminalReassert, out.String(),
		"local WS detach must now re-assert the TUI's terminal modes too (#1592 PR7)")
	// Post-detach cmd is the tea.Sequence(ClearScreen, repaint), same as remote.
	msg := cmd()
	seq := reflect.ValueOf(msg)
	require.Equal(t, reflect.Slice, seq.Kind(),
		"local post-detach cmd must now be a tea.Sequence, got %T", msg)
	require.Equal(t, 2, seq.Len(), "sequence must be exactly ClearScreen + repaint")
	endDetachWatchdog()
}

// TestAttachOverlayCallback_NoResetWhenAttachErrors: when the attach itself
// fails the terminal was never handed to the stream, so re-asserting modes
// (which re-enters and clears the alt screen) would pointlessly wipe the current
// frame.
func TestAttachOverlayCallback_NoResetWhenAttachErrors(t *testing.T) {
	h := newTestHome(t)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := h.attachOverlayCallback("t1", "test-attach", "", func() (chan struct{}, error) {
		return nil, assert.AnError
	})

	assert.Nil(t, cmd)
	assert.Zero(t, out.Len(),
		"no terminal reset may be written when the attach never started")
	assert.False(t, h.attached.Load())
}
