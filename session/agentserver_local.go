package session

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
// It is stateless (holds only the instance) and constructed on demand by
// Instance.AgentServer(); the stateful pieces — the PTY ring buffer and the
// fan-out subscriber set — arrive with the WS broker in PR5, at which point the
// agent-server becomes a cached per-instance singleton.
type localAgentServer struct {
	inst *Instance
}

// AgentServer returns the agent-server for this instance's runtime (#1592 Phase
// 2). The daemon speaks to a session ONLY through this interface, so its
// observation/delivery paths never assume the session is local tmux. Local today;
// a per-runtime factory selects container/ssh agent-servers in Phase 4.
func (i *Instance) AgentServer() AgentServer {
	return &localAgentServer{inst: i}
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

// --- data plane: wired in PR5 (WS PTY broker + clientless tmux fan-out) ---

func (s *localAgentServer) Subscribe(Seq) (PTYSubscription, error) {
	return nil, ErrDataPlaneUnwired
}

func (s *localAgentServer) Input([]byte) error {
	return ErrDataPlaneUnwired
}

func (s *localAgentServer) Resize(uint16, uint16) error {
	return ErrDataPlaneUnwired
}

func (s *localAgentServer) Kill() error {
	return s.inst.backend.Kill(s.inst)
}
