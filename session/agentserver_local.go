package session

import (
	"context"
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

	mu sync.Mutex
	// brokers holds one lazy ptyBroker per tab index (#1592 Phase 2 PR6, tab-aware
	// streaming): the agent tab (0) and each shell/process tab (>0) have their own
	// clientless capture + ring buffer so a pane bound to any tab streams over WS.
	brokers map[int]*ptyBroker
	// closed latches once Kill has run. A Subscribe/Input/Resize that races the
	// kill must NOT lazily resurrect a broker (which would start a fresh clientless
	// capture goroutine on a session that is already being torn down and never gets
	// closed — the #1632 leak). ensureBroker refuses once closed.
	closed bool
}

// AgentServer returns the cached agent-server for this instance's runtime (#1592
// Phase 2). The daemon speaks to a session ONLY through this interface, so its
// observation/delivery paths never assume the session is local tmux. Cached so
// the data-plane ring buffer and subscribers persist across calls.
//
// This is the per-runtime factory (#1592 Phase 4 PR2): a session whose runtime
// exposes a remote agent-server (i.remoteClient, set at NewInstance from
// InstanceOptions.RemoteAgentServer) gets a remoteAgentServer HTTP/WS client;
// every other session gets the local in-process impl over tmux — the default,
// unchanged. The client was validated at construction, so this stays infallible.
func (i *Instance) AgentServer() AgentServer {
	// i.mu is the SOLE owner of BOTH the runtime-selection fields
	// (remoteClient/runtimeTeardown) AND the derived i.agentSrv cache (#1729).
	// Guarding a cache and the fields it is derived from under ONE mutex is what
	// keeps them consistent: restore/recover (bindProvisionResult/
	// resetRemoteRuntime) clears the cache and swaps the fields in a SINGLE i.mu
	// section, so AgentServer() can never rebuild the cache from a pre-restore
	// snapshot of the fields. The earlier two-mutex split (agentSrvMu for the cache,
	// i.mu for the fields) caused both the original data race AND a stale-cache
	// TOCTOU — a rebuild from an OLD snapshot that pinned a torn-down endpoint.
	//
	// Fast path: a warm cache is returned under a read lock. Slow path:
	// double-checked under the write lock, building + storing the cache from the
	// fields read in the SAME critical section. The server values are trivial struct
	// literals — they don't re-enter i.mu at construction — so building under i.mu is
	// safe. Callers must not already hold i.mu (the returned server's methods
	// re-acquire it), the same invariant tabTmuxSession relies on.
	i.mu.RLock()
	if as := i.agentSrv; as != nil {
		i.mu.RUnlock()
		return as
	}
	i.mu.RUnlock()

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agentSrv == nil {
		if i.remoteClient != nil {
			// teardown reaps the sandbox this session runs in (docker rm -f) after
			// the remote Kill tears the in-sandbox workspace down — nil for the PR2
			// out-of-process case (no sandbox to reap), set for a docker session.
			i.agentSrv = &remoteAgentServer{rc: i.remoteClient, teardown: i.runtimeTeardown}
		} else {
			i.agentSrv = &localAgentServer{inst: i}
		}
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

func (s *localAgentServer) Preview(tab int, full bool) (string, error) {
	// The agent tab (0) keeps the backend preview path — a backend may format its
	// agent output specially (e.g. the remote hook's sanitized stream). Shell/process
	// tabs (>0) capture their own tmux session, mirroring TabPane's former
	// updateAgent/updateShell split now that the daemon is the sole capturer.
	if tab == 0 {
		if full {
			return s.inst.backend.PreviewFullHistory(s.inst)
		}
		return s.inst.backend.Preview(s.inst)
	}
	if full {
		return s.inst.PreviewTabFullHistory(tab)
	}
	return s.inst.PreviewTab(tab)
}

// ctxPreviewBackend is the optional backend capability for a pane capture bound to
// a context (the local tmux runtime). When the backend supports it, PreviewContext
// threads ctx down to the capture so a cancelled readiness wait kills the in-flight
// capture subprocess.
type ctxPreviewBackend interface {
	PreviewContext(ctx context.Context, i *Instance) (string, error)
}

// PreviewContext is Preview for tab 0 (the agent tab) bound to ctx: cancelling ctx
// tears down the in-flight capture when the backend supports it (local tmux). Other
// tabs / full-history / non-ctx-aware backends fall back to the ctx-free capture —
// the caller's wait still returns promptly; only the subprocess-kill is skipped.
func (s *localAgentServer) PreviewContext(ctx context.Context, tab int, full bool) (string, error) {
	if tab == 0 && !full {
		if cb, ok := s.inst.backend.(ctxPreviewBackend); ok {
			return cb.PreviewContext(ctx, s.inst)
		}
	}
	return s.Preview(tab, full)
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

// ensureBroker lazily builds the ptyBroker for tab `tab`, bound to that tab's
// clientless tmux channel. It errors when the tab has no local PTY (not started,
// a remote runtime with no tmux session, or an out-of-range tab) rather than
// panicking.
func (s *localAgentServer) ensureBroker(tab int) (*ptyBroker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("session %q is being terminated", s.inst.Title)
	}
	if br := s.brokers[tab]; br != nil {
		return br, nil
	}
	ts := s.inst.tabTmuxSession(tab)
	if ts == nil {
		return nil, fmt.Errorf("session %q tab %d has no local PTY to stream", s.inst.Title, tab)
	}
	if s.brokers == nil {
		s.brokers = make(map[int]*ptyBroker)
	}
	br := newPTYBroker(newTmuxClientlessChannel(ts))
	s.brokers[tab] = br
	return br, nil
}

// resetBrokerCaptures stops every tab broker's stale clientless capture after a
// session recovery re-spawns tmux (#1682). The brokers, their ring buffers, and
// their current subscribers are all preserved — only the capture bound to the dead
// pane is torn down, so the next Subscribe rebuilds a FRESH pipe-pane against the
// restored pane. Without it a broker that was capturing when its tmux died keeps
// capturing=true over a parked readLoop, and post-recovery subscribers attach but
// never receive output. The brokers are snapshotted under s.mu and reset OUTSIDE
// the lock so a blocking capture teardown (which joins the readLoop) can't stall a
// concurrent Subscribe/Input/Resize. A no-op once Kill has latched closed.
func (s *localAgentServer) resetBrokerCaptures() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	brokers := make([]*ptyBroker, 0, len(s.brokers))
	for _, br := range s.brokers {
		brokers = append(brokers, br)
	}
	s.mu.Unlock()
	for _, br := range brokers {
		br.resetCapture()
	}
}

func (s *localAgentServer) Subscribe(tab int, since Seq) (PTYSubscription, error) {
	br, err := s.ensureBroker(tab)
	if err != nil {
		return nil, err
	}
	return br.subscribe(since)
}

func (s *localAgentServer) Input(tab int, b []byte) error {
	br, err := s.ensureBroker(tab)
	if err != nil {
		return err
	}
	return br.input(b)
}

func (s *localAgentServer) Resize(tab int, rows, cols uint16) error {
	br, err := s.ensureBroker(tab)
	if err != nil {
		return err
	}
	return br.resize(rows, cols)
}

// Archive commits any uncommitted work and pushes this session's branch to
// origin (#1592 Phase 4 PR6). Running INSIDE a sandbox as the in-process
// agent-server, this is where a docker/ssh session's push actually happens — it
// owns the git worktree, so it can snapshot the working tree and push the branch
// GitHub will hold as the durable workspace. Returns the pushed branch name.
func (s *localAgentServer) Archive() (string, error) {
	return s.inst.pushBranchForArchive()
}

func (s *localAgentServer) Kill() error {
	// Tear every tab's data plane down first so the clientless captures stop and
	// each subscriber's NextEvent returns io.EOF, then kill the underlying session.
	// Latch closed under the same lock that snapshots the brokers so a Subscribe
	// racing this teardown can't resurrect a broker after we've drained the map
	// (#1632): ensureBroker refuses once closed, so no post-kill capture goroutine
	// is ever started.
	s.mu.Lock()
	s.closed = true
	brokers := s.brokers
	s.brokers = nil
	s.mu.Unlock()
	for _, br := range brokers {
		br.close()
	}
	return s.inst.backend.Kill(s.inst)
}
