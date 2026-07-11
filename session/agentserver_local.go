package session

import (
	"fmt"
	"sync"
)

// localAgentServer is the in-process AgentServer for the local runtime (#1592
// Phase 2 PR4): the agent runs as a tmux session on the daemon's own machine, so
// this "server" is a thin in-process facade that drives that tmux session
// directly. It is where the tmux-shaped operations the daemon used to call itself
// (HasUpdated/TapEnter/IsAlive/SendPromptCommand/Preview) now live — internal to
// the local runtime, reached only through the uniform AgentServer interface.
//
// It calls the Backend methods directly (i.backend.*) rather than the Instance
// wrappers, so the wrappers that existed only for the daemon's path (Instance
// HasUpdated/TapEnter) could be deleted with the daemon routed here — the seam is
// the AgentServer interface, not a second set of pass-through methods.
//
// As of PR5 it is a CACHED per-instance singleton (Instance.AgentServer memoizes
// it): its data plane owns the stateful pieces — the PTY output ring buffer and
// the fan-out subscriber set (ptyBroker) — which a per-call throwaway would drop.
// The broker is itself lazy: it is built (and its clientless tmux capture
// started) on the first Subscribe/Input/Resize and torn down on Kill.
type localAgentServer struct {
	inst *Instance

	mu     sync.Mutex
	broker *ptyBroker
}

// AgentServer returns the cached agent-server for this instance's runtime (#1592
// Phase 2). The daemon speaks to a session ONLY through this interface, so its
// observation/delivery paths never assume the session is local tmux. Cached so
// the data-plane ring buffer and subscribers persist across calls. Local today; a
// per-runtime factory selects container/ssh agent-servers in Phase 4.
func (i *Instance) AgentServer() AgentServer {
	i.agentSrvMu.Lock()
	defer i.agentSrvMu.Unlock()
	if i.agentSrv == nil {
		i.agentSrv = &localAgentServer{inst: i}
	}
	return i.agentSrv
}

var _ AgentServer = (*localAgentServer)(nil)

func (s *localAgentServer) Provision(firstTimeSetup bool) error {
	return s.inst.backend.Provision(s.inst, firstTimeSetup)
}

func (s *localAgentServer) Launch(firstTimeSetup bool) error {
	return s.inst.backend.Launch(s.inst, firstTimeSetup)
}

func (s *localAgentServer) Expose() (StreamEndpoint, error) {
	// The local runtime's data plane is in-process — no network hop, no URL. The
	// WS PTY broker (PR5) binds to this handle to fan tmux bytes to subscribers.
	return StreamEndpoint{Local: true}, nil
}

func (s *localAgentServer) Snapshot() (Observation, error) {
	// Dismiss a pending trust/permission prompt first, then read the pane — the
	// exact order the daemon poll ran (CheckAndHandleTrustPrompt then HasUpdated).
	// CheckAndHandleTrustPrompt's bool return is intentionally discarded: the poll
	// never consumed it.
	s.inst.backend.CheckAndHandleTrustPrompt(s.inst)
	updated, hasPrompt, content := s.inst.backend.HasUpdated(s.inst)
	return Observation{Updated: updated, HasPrompt: hasPrompt, Content: content}, nil
}

func (s *localAgentServer) Preview(full bool) (string, error) {
	if full {
		return s.inst.backend.PreviewFullHistory(s.inst)
	}
	return s.inst.backend.Preview(s.inst)
}

func (s *localAgentServer) Alive() bool {
	return s.inst.backend.IsAlive(s.inst)
}

func (s *localAgentServer) SendPrompt(prompt string) error {
	// The reliable command path (tmux send-keys), which is what automated/scheduled
	// deliveries need — it lands whether or not a PTY is currently attached.
	return s.inst.backend.SendPromptCommand(s.inst, prompt)
}

func (s *localAgentServer) TapEnter() {
	s.inst.backend.TapEnter(s.inst)
}

// --- data plane: WS PTY broker + clientless tmux fan-out (#1592 PR5) ---

// ensureBroker lazily builds the per-session ptyBroker bound to the instance's
// clientless tmux channel. It errors when the instance has no local PTY (not
// started, or a remote runtime with no agent tmux session) rather than panicking.
func (s *localAgentServer) ensureBroker() (*ptyBroker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broker != nil {
		return s.broker, nil
	}
	ts := s.inst.agentTmuxSession()
	if ts == nil {
		return nil, fmt.Errorf("session %q has no local PTY to stream", s.inst.Title)
	}
	s.broker = newPTYBroker(newTmuxClientlessChannel(ts))
	return s.broker, nil
}

func (s *localAgentServer) Subscribe(since Seq) (PTYSubscription, error) {
	br, err := s.ensureBroker()
	if err != nil {
		return nil, err
	}
	return br.subscribe(since)
}

func (s *localAgentServer) Input(b []byte) error {
	br, err := s.ensureBroker()
	if err != nil {
		return err
	}
	return br.input(b)
}

func (s *localAgentServer) Resize(rows, cols uint16) error {
	br, err := s.ensureBroker()
	if err != nil {
		return err
	}
	return br.resize(rows, cols)
}

func (s *localAgentServer) Kill() error {
	// Tear the data plane down first so the clientless capture stops and every
	// subscriber's NextEvent returns io.EOF, then kill the underlying session.
	s.mu.Lock()
	br := s.broker
	s.broker = nil
	s.mu.Unlock()
	if br != nil {
		br.close()
	}
	return s.inst.backend.Kill(s.inst)
}
