package app

import (
	"bytes"
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// remoteFakeBackend is a FakeBackend that reports itself as the remote hook
// backend, so Instance.IsRemote() returns true without standing up a real
// HookBackend (and its terminal_cmd PTY). Everything else — IsAlive() true so
// the instance is attachable — is inherited.
type remoteFakeBackend struct {
	*session.FakeBackend
}

func (remoteFakeBackend) Type() string { return "remote" }

// swapAttachOverlayCallbackFn redirects the handleEnter -> attachOverlayCallback
// indirection for the duration of a test. The substitute forwards the real
// `remote` flag the call site computed but replaces the attach closure with one
// that detaches immediately (a pre-closed channel), so the real post-detach
// lifecycle runs synchronously without a tmux client or a remote terminal_cmd
// PTY.
func swapAttachOverlayCallbackFn(t *testing.T, fn func(*home, string, string, string, bool, func() (chan struct{}, error)) tea.Cmd) {
	t.Helper()
	prev := attachOverlayCallbackFn
	attachOverlayCallbackFn = fn
	t.Cleanup(func() { attachOverlayCallbackFn = prev })
}

// driveHandleEnterAttach presses Enter on a single instance (remote or local)
// with either the Terminal or the sidebar (Preview) tab active, and returns the
// post-detach cmd plus whatever was written to the remote-detach reset writer.
//
// The first-time attach help overlay is marked seen so showHelpScreen runs the
// onDismiss attach callback synchronously, and the real attachOverlayCallback is
// driven (via the swapped indirection) with a hermetic, immediately-detaching
// attach func. The only behaviour under observation is the `remote` argument the
// handleEnter call site chose: it decides whether the #845/#848 terminal
// reset+reassert is emitted.
func driveHandleEnterAttach(t *testing.T, terminalTab, remote bool) (tea.Cmd, string) {
	t.Helper()
	resetDetachWatchdog(t)

	h := newTestHome(t)
	// Skip the first-time attach help overlay so the deferred attach callback
	// runs synchronously inside handleEnter (see showHelpScreen, app/help.go).
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	inst := instanceWithFakeBackend(t, "inst")
	if remote {
		inst.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
		inst.SetStatus(session.Running)
	}
	require.Equal(t, remote, inst.IsRemote(),
		"precondition: instance remote-ness must match the case under test")
	require.True(t, inst.TmuxAlive(),
		"precondition: instance must be attachable so handleEnter reaches the attach path")

	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	if terminalTab {
		h.store.SetActiveTab(1)
		require.Equal(t, 1, h.store.ActiveTab(), "precondition: Terminal tab must be active")
	} else {
		require.Equal(t, 0, h.store.ActiveTab(), "precondition: sidebar (Preview) tab must be active")
	}

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)
	stubStatusPollPause(t)
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, rem bool, _ func() (chan struct{}, error)) tea.Cmd {
		return m.attachOverlayCallback(title, label, traceSuffix, rem, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch) // detach immediately, synchronously, no real PTY
			return ch, nil
		})
	})

	model, cmd := h.handleEnter()
	require.Nil(t, model.(*home).textOverlay,
		"the attach help overlay must be skipped so the attach callback ran")
	endDetachWatchdog()
	return cmd, out.String()
}

// TestHandleEnter_TerminalTabRemoteDetachEmitsReset is the regression guard for
// issue #889. Detaching from a remote session in the Terminal tab streams the
// terminal_cmd PTY (#843), which hands the terminal back via
// session.hookAttachTerminalRestore — on the MAIN screen with reporting modes
// off. The TUI runs in alt-screen, so the post-detach handling must re-assert
// bubbletea's modes (remoteDetachTerminalReassert) + ClearScreen, exactly as
// the sidebar remote attach already does.
//
// The bug was that the terminal-tab call site in handleEnter hardcoded
// remote=false, so the reassert never fired and the TUI kept rendering on the
// main screen (garbled UI). This drives the real handleEnter and pins that the
// terminal-tab path now keys the reset off the instance's real remote-ness, for
// every (tab, remote) combination — so a revert to the hardcoded false fails
// the remote/Terminal-tab case.
func TestHandleEnter_TerminalTabRemoteDetachEmitsReset(t *testing.T) {
	cases := []struct {
		name        string
		terminalTab bool
		remote      bool
	}{
		{"terminal-tab/remote emits reset (#889)", true, true},
		{"terminal-tab/local emits no reset", true, false},
		{"sidebar/remote emits reset", false, true},
		{"sidebar/local emits no reset", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, out := driveHandleEnterAttach(t, tc.terminalTab, tc.remote)
			require.NotNil(t, cmd, "attach callback must return a post-detach cmd")

			if tc.remote {
				require.Equal(t, remoteDetachTerminalReassert, out,
					"a remote detach must synchronously re-assert the TUI's terminal "+
						"modes — the terminal_cmd PTY left the terminal on the main screen")
				// Remote post-detach cmd is tea.Sequence(ClearScreen, repaint);
				// sequenceMsg is unexported, so unpack reflectively.
				seq := reflect.ValueOf(cmd())
				require.Equal(t, reflect.Slice, seq.Kind(),
					"remote post-detach cmd must be a tea.Sequence, got %T", cmd())
				require.Equal(t, 2, seq.Len(), "sequence must be ClearScreen + repaint")
				first, ok := seq.Index(0).Interface().(tea.Cmd)
				require.True(t, ok)
				assert.Equal(t, tea.ClearScreen(), first(),
					"first sequenced cmd must invalidate the renderer's stale diff cache")
			} else {
				assert.Zero(t, len(out),
					"a local detach must write no terminal reset — the tmux client "+
						"hands the terminal back untouched")
				_, isRepaint := cmd().(repaintAfterDetachMsg)
				assert.True(t, isRepaint,
					"local post-detach cmd must be the bare repaintAfterDetachMsg emitter")
			}
		})
	}
}
