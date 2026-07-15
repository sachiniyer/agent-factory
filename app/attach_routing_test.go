package app

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// backendAttachSpy mirrors the REAL docker/ssh/hook backends: since #1592 Phase
// 4 PR7 provisioned remote runtimes and exposed them as agent-servers, every
// remote backend's Attach/AttachTerminal returns a routing-invariant error
// rather than a PTY. The spy records the calls so a test can pin that the client
// attach dispatch never reaches them.
//
// The plain FakeBackend cannot stand in here: its Attach SUCCEEDS (a pre-closed
// channel), so a test using it would pass while the dispatch routed to a backend
// that errors in production — exactly the gap that let #1837 ship.
type backendAttachSpy struct {
	*session.FakeBackend
	attachCalls         atomic.Int32
	attachTerminalCalls atomic.Int32
}

func (b *backendAttachSpy) Attach(*session.Instance) (chan struct{}, error) {
	b.attachCalls.Add(1)
	return nil, fmt.Errorf("sessions attach client-side over the WS PTY stream, not through the backend")
}

func (b *backendAttachSpy) AttachTerminal(*session.Instance, int) (chan struct{}, error) {
	b.attachTerminalCalls.Add(1)
	return nil, fmt.Errorf("terminal tabs attach client-side over the WS PTY stream, not through the backend")
}

func (b *backendAttachSpy) backendCalls() int32 {
	return b.attachCalls.Load() + b.attachTerminalCalls.Load()
}

// remoteAttachSpy reports WorkspaceRemote (a bare remote hook backend's
// capabilities) on top of the erroring attach surface.
type remoteAttachSpy struct{ *backendAttachSpy }

func (remoteAttachSpy) Type() string { return "remote" }

func (remoteAttachSpy) Capabilities() session.Capabilities {
	return (&session.HookBackend{}).Capabilities()
}

// TestAttachInstanceTab_RoutesEverySessionToStream is the regression guard for
// #1837. The full-screen attach byte source is the daemon's WS PTY stream for
// EVERY session — local or remote, agent tab or terminal tab — because the
// daemon resolves locality itself via instance.AgentServer() (a local broker, or
// a remoteAgentServer proxy for docker/ssh/hook).
//
// The bug: attachInstanceTab branched on Capabilities().Workspace and sent a
// remote session to m.store.AttachInstance (-> backend.Attach) for the agent tab
// and ui.AttachTerminalTab (-> backend.AttachTerminal) for a terminal tab. Every
// remote backend's Attach/AttachTerminal returns an error, so remote attach
// failed outright — and silently, since attachOverlayCallback only logs the
// error and returns a nil cmd.
//
// Pre-fix, the two remote cases route to the backend: backendCalls() is 1 and
// the stream is never dialed, so this fails. Post-fix all four cases dial the
// stream and never touch the backend.
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
			spy := &backendAttachSpy{FakeBackend: session.NewFakeBackend()}
			if tc.remote {
				inst.SetBackend(remoteAttachSpy{spy})
			} else {
				inst.SetBackend(spy)
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

			require.Zero(t, spy.backendCalls(),
				"attach must NEVER reach backend.Attach/AttachTerminal — every remote "+
					"backend's implementation returns a routing-invariant error, so a "+
					"backend-routed attach fails outright (#1837)")
			require.Equal(t, int32(1), streamCalls.Load(),
				"attach must dial the daemon's WS PTY stream exactly once — the sole "+
					"byte source for local and remote sessions alike (#1837)")
			require.Equal(t, inst.Title, gotTitle, "the stream must target the captured instance (#716)")
			require.Equal(t, tc.wantTabIdx, gotTabIdx, "the stream must target the captured tab (#716)")
		})
	}
}
