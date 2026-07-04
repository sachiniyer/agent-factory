package app

import (
	"bytes"
	"io"
	"reflect"
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
func runAttachOverlayCallback(t *testing.T, h *home, remote bool) tea.Cmd {
	t.Helper()
	stubStatusPollPause(t)
	ch := make(chan struct{})
	done := make(chan tea.Cmd, 1)
	go func() {
		done <- h.attachOverlayCallback("t1", "test-attach", "", remote, func() (chan struct{}, error) {
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

// TestAttachOverlayCallback_RemoteReassertsTerminalModes is the app half of
// the #845 fix. After a remote attach returns, the hook backend has handed
// the terminal back in a neutral state (main screen, cursor visible, all
// reporting modes off — see session.hookAttachTerminalRestore), which is NOT
// the state this TUI's renderer assumes. The callback must re-assert
// bubbletea's startup modes synchronously — while the Update goroutine is
// still blocked here, before the renderer can emit a frame — and then route
// through tea.ClearScreen + the usual repaintAfterDetachMsg flow so the stale
// diff cache is invalidated and the screen fully repainted.
func TestAttachOverlayCallback_RemoteReassertsTerminalModes(t *testing.T) {
	resetDetachWatchdog(t)
	h := newTestHome(t)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := runAttachOverlayCallback(t, h, true)
	require.NotNil(t, cmd)

	// The re-assert was written before the callback returned (i.e. before the
	// bubbletea event loop could resume), in full.
	require.Equal(t, remoteDetachTerminalReassert, out.String(),
		"remote detach must synchronously re-assert the TUI's terminal modes")
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

// TestAttachOverlayCallback_LocalDetachWritesNoReset pins the local flow
// unchanged: a local tmux detach leaves the terminal exactly as the TUI
// expects, so no mode bytes are written and the post-detach cmd is the plain
// repaintAfterDetachMsg emitter (#579/#683 flow) — no ClearScreen flicker.
func TestAttachOverlayCallback_LocalDetachWritesNoReset(t *testing.T) {
	resetDetachWatchdog(t)
	h := newTestHome(t)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := runAttachOverlayCallback(t, h, false)
	require.NotNil(t, cmd)

	assert.Zero(t, out.Len(),
		"local detach must not write any terminal reset — the tmux client "+
			"hands the terminal back untouched")
	_, isRepaint := cmd().(repaintAfterDetachMsg)
	assert.True(t, isRepaint,
		"local post-detach cmd must remain the bare repaintAfterDetachMsg emitter")
	endDetachWatchdog()
}

// TestAttachOverlayCallback_NoResetWhenRemoteAttachErrors: when the attach
// itself fails the terminal was never handed to the remote stream, so
// re-asserting modes (which re-enters and clears the alt screen) would
// pointlessly wipe the current frame.
func TestAttachOverlayCallback_NoResetWhenRemoteAttachErrors(t *testing.T) {
	h := newTestHome(t)

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)

	cmd := h.attachOverlayCallback("t1", "test-attach", "", true, func() (chan struct{}, error) {
		return nil, assert.AnError
	})

	assert.Nil(t, cmd)
	assert.Zero(t, out.Len(),
		"no terminal reset may be written when the attach never started")
	assert.False(t, h.attached.Load())
}
