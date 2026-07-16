package app

import (
	"bytes"
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
)

// remoteFakeBackend is a FakeBackend that reports itself as the remote hook
// backend, so its Capabilities().Workspace is WorkspaceRemote without standing
// up a real HookBackend (and its terminal_cmd PTY). Everything else — IsAlive()
// true so the instance is attachable — is inherited.
type remoteFakeBackend struct {
	*session.FakeBackend
}

func (remoteFakeBackend) Type() string { return "remote" }

// Capabilities mirrors a bare remote hook backend (WorkspaceRemote, attachable,
// no terminal_cmd) so the delocalized client gates that read Capabilities()
// see this double as remote (#1592 Phase 1 PR2).
func (remoteFakeBackend) Capabilities() session.Capabilities {
	return (&session.HookBackend{}).Capabilities()
}

// swapAttachOverlayCallbackFn redirects the handleEnter -> attachOverlayCallback
// indirection for the duration of a test. The substitute replaces the attach
// closure with one that detaches immediately (a pre-closed channel), so the real
// post-detach lifecycle runs synchronously without a tmux client or a remote
// terminal_cmd PTY.
func swapAttachOverlayCallbackFn(t *testing.T, fn func(*home, string, string, string, func() (chan struct{}, error)) tea.Cmd) {
	t.Helper()
	prev := attachOverlayCallbackFn
	attachOverlayCallbackFn = fn
	t.Cleanup(func() { attachOverlayCallbackFn = prev })
}

// driveHandleEnterAttach presses Enter on a single instance (remote or local)
// with either the Terminal or the sidebar Agent tab active, and returns the
// post-detach cmd plus whatever was written to the remote-detach reset writer.
//
// The first-time attach help overlay is marked seen so showHelpScreen runs the
// onDismiss attach callback synchronously, and the real attachOverlayCallback is
// driven (via the swapped indirection) with a hermetic, immediately-detaching
// attach func. Since #1592 Phase 2 PR7 the post-detach #845 terminal
// reset+reassert is uniform across local and remote, so every (tab, locality)
// case exercises the same call site and emits it.
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
		inst.SetStatusForTest(session.Running)
	}
	require.Equal(t, remote, inst.Capabilities().Workspace == session.WorkspaceRemote,
		"precondition: instance remote-ness must match the case under test")
	require.True(t, inst.TmuxAlive(),
		"precondition: instance must be attachable so handleEnter reaches the attach path")

	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	if terminalTab {
		h.store.SetActiveTab(1)
		require.Equal(t, 1, h.store.ActiveTab(), "precondition: Terminal tab must be active")
	} else {
		require.Equal(t, 0, h.store.ActiveTab(), "precondition: sidebar Agent tab must be active")
	}

	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
		return m.attachOverlayCallback(title, label, traceSuffix, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch) // detach immediately, synchronously, no real PTY
			return ch, nil
		})
	})

	model, cmd := h.handleEnter()
	require.Nil(t, model.(*home).textOverlay,
		"the attach help overlay must be skipped so the attach transition is scheduled")
	cmd = runAttachTransitionCmd(t, h, cmd)
	endDetachWatchdog()
	return cmd, out.String()
}

// TestHandleEnter_TerminalTabDetachEmitsReset is the regression guard for issue
// #889, updated for the #1592 Phase 2 PR7 uniform reassert. Every full-screen
// attach — local WS PTY proxy or remote hook terminal_cmd PTY — scribbles the
// pane program's alt-screen/mouse/scroll modes onto the real terminal and hands
// it back neutral (MAIN screen, reporting off) on detach. The TUI runs in
// alt-screen, so the post-detach handling must ALWAYS re-assert bubbletea's
// modes (remoteDetachTerminalReassert) + ClearScreen.
//
// The original #889 bug was that the terminal-tab call site in handleEnter
// hardcoded remote=false so the reassert never fired for a remote terminal-tab
// detach; the reassert is now unconditional, so this drives the real handleEnter
// and pins that EVERY (tab, locality) combination emits it.
func TestHandleEnter_TerminalTabDetachEmitsReset(t *testing.T) {
	cases := []struct {
		name        string
		terminalTab bool
		remote      bool
	}{
		{"terminal-tab/remote emits reset (#889)", true, true},
		{"terminal-tab/local emits reset", true, false},
		{"sidebar/remote emits reset", false, true},
		{"sidebar/local emits reset", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, out := driveHandleEnterAttach(t, tc.terminalTab, tc.remote)
			require.NotNil(t, cmd, "attach callback must return a post-detach cmd")

			require.Equal(t, remoteDetachTerminalReassert, out,
				"every detach must synchronously re-assert the TUI's terminal modes "+
					"— the attach driver (local WS proxy or remote terminal_cmd PTY) "+
					"left the terminal on the main screen")
			// Post-detach cmd is tea.Sequence(ClearScreen, repaint); sequenceMsg is
			// unexported, so unpack reflectively.
			seq := reflect.ValueOf(cmd())
			require.Equal(t, reflect.Slice, seq.Kind(),
				"post-detach cmd must be a tea.Sequence, got %T", cmd())
			require.Equal(t, 2, seq.Len(), "sequence must be ClearScreen + repaint")
			first, ok := seq.Index(0).Interface().(tea.Cmd)
			require.True(t, ok)
			assert.Equal(t, tea.ClearScreen(), first(),
				"first sequenced cmd must invalidate the renderer's stale diff cache")
		})
	}
}

// TestHandleEnter_RemotePanePreviewTriggersFullScreenAttach is the regression
// guard for issue #1601. Enter that commits a sidebar tab-row PREVIEW whose
// target is a remote (non-embeddable) session must, on the FIRST press, both
// commit the preview AND fire the full-screen attach — mirroring the
// focused-pane and tree Enter paths.
//
// The bug: handleEnter's panePreviewTxn branch short-circuited with `||
// liveSessionName(...) == ""`, returning after only committing the preview for
// a remote target. enterPane already routes non-embeddable panes to
// handleEnterPane's full-screen attach, so the short-circuit skipped it and
// forced the user to press Enter (or `o`) a second time. Dropping the clause
// lets enterPane run: an embeddable tab enters interactive mode in place, a
// remote one attaches full-screen on the first Enter.
//
// This drives the real key dispatch and observes the strongest side effect of a
// remote full-screen attach+detach: the #845 terminal mode re-assert. On the
// pre-fix code the branch returns before enterPane, no attach starts, the reset
// writer stays empty, and runAttachTransitionCmd returns nil — so this test
// fails without the fix.
func TestHandleEnter_RemotePanePreviewTriggersFullScreenAttach(t *testing.T) {
	resetDetachWatchdog(t)

	h := newTestHome(t)
	// Skip the first-time attach help overlay so the deferred attach callback
	// runs synchronously inside handleEnter (see showHelpScreen, app/help.go).
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	// Owner pane: a plain local instance whose pane hosts the preview.
	owner := instanceWithFakeBackend(t, "owner")
	owner.AddTabForTest("agent", session.TabKindAgent)
	h.store.AddInstance(owner)

	// Preview target: a REMOTE (non-embeddable) instance — liveSessionName == ""
	// for its active tab, so committing the preview must fall through to the
	// full-screen attach rather than an in-place interactive embed.
	remote := instanceWithFakeBackend(t, "remote-inst")
	remote.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	remote.SetStatusForTest(session.Running)
	remote.AddTabForTest("agent", session.TabKindAgent)
	h.store.AddInstance(remote)

	resizeHome(h, 200, 40)

	// Open (and focus) the owner instance's pane, then move the sidebar cursor
	// onto the remote instance so its row becomes a live preview in the owner
	// pane — the pane preview transaction Enter will commit.
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes(), "precondition: the owner pane must be open")

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: selecting the remote instance must open a preview")
	require.Same(t, remote, h.panePreviewTxn.target.instance, "precondition: the preview must target the remote instance")
	require.Equal(t, "", liveSessionName(remote, h.panePreviewTxn.target.tab),
		"precondition: the remote target must be non-embeddable (liveSessionName == \"\")")

	// Drive the attach lifecycle hermetically: the swapped callback detaches
	// immediately (a pre-closed channel), and the mode re-assert lands in a buffer
	// instead of the test's terminal.
	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)
	swapAttachOverlayCallbackFn(t, func(m *home, title, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
		return m.attachOverlayCallback(title, label, traceSuffix, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch) // detach immediately, synchronously, no real PTY
			return ch, nil
		})
	})

	// FIRST Enter, via the real key dispatch.
	model, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)
	require.Nil(t, model.(*home).panePreviewTxn,
		"Enter must commit (clear) the pane preview transaction")

	cmd = runAttachTransitionCmd(t, h, cmd)
	endDetachWatchdog()

	require.NotNil(t, cmd,
		"the FIRST Enter must fire the full-screen attach — not require a second Enter/`o` (#1601)")
	require.Equal(t, remoteDetachTerminalReassert, out.String(),
		"the first Enter must run the remote full-screen attach+detach lifecycle, "+
			"which re-asserts the TUI's terminal modes (#845); an empty buffer means "+
			"the attach never fired (the pre-#1601 early return)")
}
