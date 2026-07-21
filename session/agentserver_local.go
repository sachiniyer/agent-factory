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
// (HasUpdated/IsAlive/SendPromptCommand/Preview) now live — internal to
// the local runtime, reached only through the uniform AgentServer interface.
//
// It calls the Backend methods directly (via i.currentBackend()) rather than the Instance
// wrappers, so the wrappers that existed only for the daemon's path (Instance
// HasUpdated) could be deleted with the daemon routed here — the seam is
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
	// brokers holds one lazy ptyBroker per tab, keyed by the tab's STABLE id
	// (#1738), not its ordinal index (#1592 Phase 2 PR6, tab-aware streaming): the
	// agent tab and each shell/process tab have their own clientless capture + ring
	// buffer so a pane bound to any tab streams over WS. Keying on the stable id
	// (resolved from the caller's index at ensureBroker time) means a broker follows
	// its tab across a reorder/close — after tab 1 of [agent,A,B] closes, B's broker
	// is still found under B's id even though B is now at index 1, so a new
	// subscriber for B never lands on A's stale (dead-tmux) broker the way an
	// index-keyed map would.
	brokers map[string]*ptyBroker
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
		switch {
		case i.remoteClient != nil:
			// teardown reaps the sandbox this session runs in (docker rm -f) after
			// the remote Kill tears the in-sandbox workspace down — nil for the PR2
			// out-of-process case (no sandbox to reap), set for a docker session.
			i.agentSrv = &remoteAgentServer{rc: i.remoteClient, inst: i, teardown: i.runtimeTeardown}
		case backendIsRemoteWorkspace(i.backend):
			// A remote-workspace backend (docker/ssh/hook) with NO live client:
			// remoteClient is nil because the runtime wiring was torn down — a
			// sandbox session loaded inert from disk, or one left Lost by a FAILED
			// lost-recovery (reprovisionRemote succeeded, Start failed, then
			// teardownAfterStartFailure cleared remoteClient while the session stays
			// started+Lost so the #1128 restore loop keeps retrying). A localAgentServer
			// must NEVER stand in here: a remote backend's data-plane methods
			// (HasUpdated/IsAlive/Preview/…) delegate straight back to i.AgentServer(),
			// so a localAgentServer wrapping one mutually recurses until the daemon
			// poll overflows its stack (#2005). Report the sandbox as gone instead —
			// the poll's remote-probe-failure path (#1794) and the restore loop's
			// Alive gate already handle that, and the instance keeps its started+Lost
			// state so auto-recovery is never stranded.
			i.agentSrv = &deadRemoteAgentServer{title: i.Title, teardown: i.runtimeTeardown}
		default:
			i.agentSrv = &localAgentServer{inst: i}
		}
	}
	return i.agentSrv
}

// backendIsRemoteWorkspace reports whether b drives an off-box workspace
// (docker/ssh/hook) — the backends whose data-plane methods delegate back to
// i.AgentServer() (they embed remoteAgentBackend), and which therefore must never
// be wrapped in a localAgentServer (#2005). A nil backend is treated as local
// (the not-yet-initialised default, matching Instance.Capabilities).
func backendIsRemoteWorkspace(b Backend) bool {
	return b != nil && b.Capabilities().Workspace == WorkspaceRemote
}

var _ AgentServer = (*localAgentServer)(nil)

func (s *localAgentServer) Provision(firstTimeSetup bool) error {
	return s.inst.currentBackend().Provision(s.inst, firstTimeSetup)
}

func (s *localAgentServer) Launch(firstTimeSetup bool) error {
	return s.inst.currentBackend().Launch(s.inst, firstTimeSetup)
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
	// One backend snapshot for both calls, so the pair can never straddle a
	// restore's rebind and dismiss a prompt on one backend while reading the pane
	// of another (#2096).
	b := s.inst.currentBackend()
	b.CheckAndHandleTrustPrompt(s.inst)
	updated, hasPrompt, content := b.HasUpdated(s.inst)
	return Observation{
		Updated:     updated,
		HasPrompt:   hasPrompt,
		Content:     content,
		ModelChange: b.AgentModelChange(s.inst),
	}, nil
}

func (s *localAgentServer) Preview(tab int, full bool) (PreviewSnapshot, error) {
	// The agent tab (0) keeps the backend preview path — a backend may format its
	// agent output specially (e.g. the remote hook's sanitized stream). Shell/process
	// tabs (>0) capture their own tmux session, mirroring TabPane's former
	// updateAgent/updateShell split now that the daemon is the sole capturer.
	if tab != 0 {
		return s.inst.PreviewTabSnapshot(tab, full)
	}
	b := s.inst.currentBackend()
	var (
		content string
		err     error
	)
	if full {
		content, err = b.PreviewFullHistory(s.inst)
	} else {
		content, err = b.Preview(s.inst)
	}
	if err != nil {
		return PreviewSnapshot{}, err
	}
	s.inst.mu.RLock()
	ts := s.inst.tmuxLocked()
	s.inst.mu.RUnlock()
	return previewSnapshotWithModes(content, ts), nil
}

// PreviewByID binds directly to the tmux/backend target selected under the
// instance lock. No ordinal crosses that lock boundary (#2200).
func (s *localAgentServer) PreviewByID(tabID string, full bool) (PreviewSnapshot, error) {
	return s.inst.PreviewTabSnapshotByID(tabID, full)
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
		if cb, ok := s.inst.currentBackend().(ctxPreviewBackend); ok {
			return cb.PreviewContext(ctx, s.inst)
		}
	}
	snapshot, err := s.Preview(tab, full)
	return snapshot.Content, err
}

// Alive probes the local backend in-process, so the answer is always definitive
// and the error is always nil — there is no transport between here and the tmux
// server that could leave liveness UNKNOWN. The error exists for the remote
// runtime; see the AgentServer interface.
func (s *localAgentServer) Alive() (bool, error) {
	// Forward the backend's error rather than hardcoding nil (#1917 round 8): nil
	// here meant "the probe answered", which was never checked and often false.
	// probeLiveness maps a non-nil error to probeUnknown, so this is the hop that
	// lets an unanswerable tmux stop being counted as alive.
	return s.inst.currentBackend().IsAlive(s.inst)
}

func (s *localAgentServer) SendPrompt(prompt string) error {
	// The reliable command path (tmux send-keys), which is what automated/scheduled
	// deliveries need — it lands whether or not a PTY is currently attached.
	return s.inst.currentBackend().SendPromptCommand(s.inst, prompt)
}

// --- data plane: WS PTY broker + clientless tmux fan-out (#1592 PR5) ---

// ensureBroker lazily builds the ptyBroker for tab `tab`, bound to that tab's
// clientless tmux channel. It errors when the tab has no local PTY (not started,
// a remote runtime with no tmux session, or an out-of-range tab) rather than
// panicking.
func (s *localAgentServer) ensureBroker(tab int) (*ptyBroker, error) {
	// Resolve the caller's ordinal to the tab's STABLE id (#1738) and the tmux it
	// currently backs in ONE instance-lock acquisition, BEFORE taking s.mu. Two
	// lookups could pair b's ID with c's tmux if a close shifted the roster between
	// them — then cache that wrong binding under b forever (#2200).
	id, ts, ok := s.inst.tabTmuxTargetAt(tab)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("session %q is being terminated", s.inst.Title)
	}
	if !ok || ts == nil {
		return nil, fmt.Errorf("session %q tab %d has no local PTY to stream", s.inst.Title, tab)
	}
	if br := s.brokers[id]; br != nil {
		return br, nil
	}
	if s.brokers == nil {
		s.brokers = make(map[string]*ptyBroker)
	}
	br := newPTYBroker(newTmuxClientlessChannel(ts))
	s.brokers[id] = br
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

// ensureBrokerByID is ensureBroker addressed by a tab's STABLE id (#1738) — the
// id-native binding path (TabAddressableServer). It resolves the id to the tmux it
// currently backs ATOMICALLY (TabTmuxByID, one lock), so unlike the ordinal path
// there is no window in which a concurrent close/reorder can shift a different tab
// under the caller's address (#1779). The broker map is already id-keyed, so this
// is the direct route: no ordinal is involved at any point. A stale/unknown id is
// ErrTabGone — a refusal, never a fall back to a positional tab.
func (s *localAgentServer) ensureBrokerByID(tabID string) (*ptyBroker, error) {
	// Resolve under i.mu BEFORE taking s.mu (never nest s.mu → i.mu).
	ts, exists := s.inst.TabTmuxByID(tabID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("session %q is being terminated", s.inst.Title)
	}
	// GONE and NOT-YET-STREAMABLE are different answers. Only an id that names no
	// tab is ErrTabGone (the client should stop addressing it); a real tab with no
	// local PTY — not started, or a remote runtime — keeps the ordinal path's
	// message, so a client isn't told a tab that may still come up is gone.
	if !exists {
		return nil, fmt.Errorf("session %q tab id %q: %w", s.inst.Title, tabID, ErrTabGone)
	}
	if ts == nil {
		return nil, fmt.Errorf("session %q tab id %q has no local PTY to stream", s.inst.Title, tabID)
	}
	if br := s.brokers[tabID]; br != nil {
		return br, nil
	}
	if s.brokers == nil {
		s.brokers = make(map[string]*ptyBroker)
	}
	br := newPTYBroker(newTmuxClientlessChannel(ts))
	s.brokers[tabID] = br
	return br, nil
}

// tabStreamCloser is the optional agent-server capability of ending ONE tab's
// PTY stream — the data-plane half of closing a tab. It is deliberately narrow
// and unexported: CloseTab is a session-package operation, and a runtime that
// has no in-process broker to close (the remote agent-server, whose brokers live
// on the far side of the wire) simply does not implement it.
type tabStreamCloser interface {
	closeTabStream(tabID string)
}

var _ tabStreamCloser = (*localAgentServer)(nil)

// closeTabStream tears down the data plane of the ONE tab named by tabID (#2136):
// its subscribers' NextEvent returns ErrTabClosed and its clientless capture
// stops, so a PTY-only client learns the tab is gone at once instead of blocking
// on a stream that will never produce another byte. Every OTHER tab's broker is
// untouched — the map is keyed by the tab's STABLE id (#1738), so this can only
// ever reach the tab that was actually closed, even after a reorder.
//
// The broker is also dropped from the map: the id is never reused, so keeping a
// closed broker under it would only retain the ring buffer. Safe to call for a
// tab that has no broker (nobody ever streamed it), was already closed, or on an
// agent-server already torn down by Kill — each is a no-op.
func (s *localAgentServer) closeTabStream(tabID string) {
	if tabID == "" {
		return
	}
	s.mu.Lock()
	br := s.brokers[tabID]
	delete(s.brokers, tabID)
	s.mu.Unlock()
	if br != nil {
		br.closeTab()
	}
}

func (s *localAgentServer) SubscribeTab(tabID string, since Seq) (PTYSubscription, error) {
	br, err := s.ensureBrokerByID(tabID)
	if err != nil {
		return nil, err
	}
	return br.subscribe(since)
}

func (s *localAgentServer) InputTab(tabID string, b []byte) error {
	br, err := s.ensureBrokerByID(tabID)
	if err != nil {
		return err
	}
	return br.input(b)
}

func (s *localAgentServer) ResizeTab(tabID string, rows, cols uint16) error {
	br, err := s.ensureBrokerByID(tabID)
	if err != nil {
		return err
	}
	return br.resize(rows, cols)
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
	return s.inst.currentBackend().Kill(s.inst)
}
