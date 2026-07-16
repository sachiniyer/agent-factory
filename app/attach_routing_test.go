package app

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestAttachInstanceTab_RoutesEverySessionToStream is the regression guard for
// #1837. The full-screen attach byte source is the daemon's WS PTY stream for
// EVERY session — local or remote, agent tab or terminal tab — because the
// daemon resolves locality itself via instance.AgentServer() (a local broker, or
// a remoteAgentServer proxy for docker/ssh/hook).
//
// The bug: attachInstanceTab branched on Capabilities().Workspace and sent a
// remote session to m.store.AttachInstance (-> backend.Attach) for the agent tab
// and ui.AttachTerminalTab (-> backend.AttachTerminal) for a terminal tab. Every
// remote backend's Attach/AttachTerminal returned an error, so remote attach
// failed outright — and silently, since attachOverlayCallback only logs the
// error and returns a nil cmd.
//
// This test used to also assert the attach never reached the backend, via a spy
// that errored like the real remote backends. #1852 deleted that whole backend
// attach surface, so the invariant is now enforced by the type system — there is
// no backend.Attach left to mis-route to — and what remains to check is the
// positive half: every (remote|local) x (agent|terminal) case dials the stream
// exactly once, at the captured instance and tab.
func TestAttachInstanceTab_RoutesEverySessionToStream(t *testing.T) {
	cases := []struct {
		name       string
		remote     bool
		tabIdx     int
		wantTabIdx int
	}{
		{"remote/agent tab (#1837)", true, 0, 0},
		{"remote/terminal tab (#1837)", true, 1, 1},
		{"local/agent tab", false, 0, 0},
		{"local/terminal tab", false, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetDetachWatchdog(t)

			h := newTestHome(t)
			// Skip the first-time attach help overlay so the deferred attach callback
			// runs synchronously inside attachInstanceTab (see showHelpScreen).
			require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

			inst := instanceWithFakeBackend(t, "inst")
			if tc.remote {
				inst.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
			}
			inst.SetStatusForTest(session.Running)
			require.Equal(t, tc.remote, inst.Capabilities().Workspace == session.WorkspaceRemote,
				"precondition: instance remote-ness must match the case under test")

			h.store.AddInstance(inst)
			h.sidebar.SetSelectedInstance(0)

			// Observe the stream dial instead of standing up a daemon; detach
			// immediately so the post-detach lifecycle runs synchronously.
			var streamCalls atomic.Int32
			var gotTitle string
			var gotTabIdx int
			t.Cleanup(SetAttachStreamFnForTest(func(_ context.Context, title, _, _ string, tabIdx int) (chan struct{}, error) {
				streamCalls.Add(1)
				gotTitle, gotTabIdx = title, tabIdx
				ch := make(chan struct{})
				close(ch)
				return ch, nil
			}))

			_, cmd := h.attachInstanceTab(inst, tc.tabIdx, "agent", "terminal")
			_ = runAttachTransitionCmd(t, h, cmd)
			endDetachWatchdog()

			require.Equal(t, int32(1), streamCalls.Load(),
				"attach must dial the daemon's WS PTY stream exactly once — the sole "+
					"byte source for local and remote sessions alike (#1837)")
			require.Equal(t, inst.Title, gotTitle, "the stream must target the captured instance (#716)")
			require.Equal(t, tc.wantTabIdx, gotTabIdx, "the stream must target the captured tab (#716)")
		})
	}
}
