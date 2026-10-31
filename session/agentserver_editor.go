package session

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// EditorStatus is the state of a workspace's editor on its host (W5.4, #5571).
type EditorStatus struct {
	// State is agentproto.EditorStateReady, EditorStateStarting or EditorStateMissing.
	State string
	// Message explains a missing editor (what to install on the host).
	Message string
}

// EditorRelayer is implemented by the agent-servers that run the workspace's
// editor on their host and relay it. Like LoopbackRelayer it is separate from
// AgentServer because only an off-box workspace has an editor the daemon cannot
// start itself.
type EditorRelayer interface {
	// EnsureEditor starts the editor if needed and reports whether it serves yet.
	EnsureEditor(ctx context.Context) (EditorStatus, error)
	// DialEditor opens a byte stream to the editor's socket on the host. The
	// target is server-owned: the caller names no path or port.
	DialEditor(ctx context.Context) (net.Conn, error)
	// StopEditor ends this workspace's editor on the host (and no other editor).
	StopEditor(ctx context.Context) error
}

var _ EditorRelayer = (*remoteAgentServer)(nil)

// editorCallTimeout bounds the start/status call; the editor's own cold start is
// reported as "starting", not waited out.
const editorCallTimeout = 30 * time.Second

// EnsureEditor implements EditorRelayer. A host whose af predates the editor
// route is told so, rather than failing with an unknown-route error.
func (s *remoteAgentServer) EnsureEditor(_ context.Context) (EditorStatus, error) {
	h, err := s.rc.health()
	if err != nil {
		return EditorStatus{}, err
	}
	if !slices.Contains(h.Features, agentproto.EditorFeature) {
		return EditorStatus{}, fmt.Errorf("the af on this session's host predates the workspace editor; update af there to open a VS Code tab")
	}
	var resp struct {
		State   string `json:"state"`
		Message string `json:"message"`
	}
	if err := s.rc.callWithin(agentproto.EditorPath, struct{}{}, &resp, editorCallTimeout); err != nil {
		return EditorStatus{}, err
	}
	return EditorStatus{State: resp.State, Message: resp.Message}, nil
}

// DialEditor implements EditorRelayer.
func (s *remoteAgentServer) DialEditor(ctx context.Context) (net.Conn, error) {
	return s.trackedRelay(func() (net.Conn, error) {
		return s.rc.dialRelayQuery(ctx, agentproto.RelayTargetQueryParam, agentproto.RelayTargetEditor,
			"the workspace editor is not serving on the host")
	})
}

// StopEditor implements EditorRelayer. A host without the route has no editor of
// ours to stop, so that is success rather than an error.
func (s *remoteAgentServer) StopEditor(_ context.Context) error {
	h, err := s.rc.health()
	if err != nil {
		return err
	}
	if !slices.Contains(h.Features, agentproto.EditorFeature) {
		return nil
	}
	return s.rc.callWithin(agentproto.EditorStopPath, struct{}{}, nil, editorCallTimeout)
}
