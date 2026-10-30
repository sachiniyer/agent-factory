package session

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Relaunch the agent in place (#5571 W7.1): the agent-server stops the agent and
// starts it again in its EXISTING workspace with the provider's resume. The
// branch, the tree and any uncommitted work are untouched — nothing is pushed,
// reaped, re-cloned or re-provisioned, and no post-worktree hook re-runs — as
// long as the worktree still exists. If it was deleted, the respawn rebuilds it
// (RebuildFromExistingBranch / RebuildFreshFromRecordedBase), which does restart
// the post-worktree hooks. A
// sandbox that is reachable and still has its workspace never needs more than
// this; re-provisioning is for a sandbox that is gone.

// relaunchFeature is the /v1/health token an in-sandbox agent-server advertises
// when it serves POST /v1/agent/relaunch. A driving daemon checks it before
// calling, so an older sandbox build degrades to today's path rather than
// answering an unknown-route 404.
const relaunchFeature = "relaunch"

// RelaunchFeature is exported for the agent-server's /v1/health advertisement.
const RelaunchFeature = relaunchFeature

// AgentRelaunchCallTimeout is the relaunch route's own budget: it waits for the
// old agent's process tree to exit and for the new one to start.
const AgentRelaunchCallTimeout = 2 * time.Minute

// ErrRelaunchUnsupported reports that this runtime cannot relaunch in place —
// an agent-server older than the relaunch route, or a runtime with no live
// server. Callers fall back to the re-provisioning path, which is the only
// behaviour such a runtime ever had.
var ErrRelaunchUnsupported = errors.New("the agent-server cannot relaunch the agent in place")

// AgentRelauncher is the optional capability of an AgentServer that can relaunch
// its agent in place. It is deliberately NOT a method on AgentServer: every
// existing implementation and test double stays valid, the same additive shape
// as PromptDeliveryReporter.
type AgentRelauncher interface {
	// Relaunch stops the agent (if it is running) and starts it again in the
	// existing workspace, resuming the provider's conversation.
	Relaunch() error
	// CanRelaunch reports whether Relaunch is available, without side effects. An
	// error means the answer could not be determined (an unreachable sandbox).
	CanRelaunch() (bool, error)
}

// CanRelaunchInPlace reports whether this instance's runtime can relaunch its
// agent in place right now. False on any doubt — the caller then keeps the
// re-provisioning behaviour it always had.
func (i *Instance) CanRelaunchInPlace() bool {
	r, ok := i.AgentServer().(AgentRelauncher)
	if !ok {
		return false
	}
	can, err := r.CanRelaunch()
	return err == nil && can
}

// RelaunchInPlace relaunches the agent in its existing workspace. It returns
// ErrRelaunchUnsupported when the runtime cannot; any other error means the
// relaunch was attempted or the sandbox could not be asked, and the workspace
// must be assumed to still hold work.
func (i *Instance) RelaunchInPlace() error {
	r, ok := i.AgentServer().(AgentRelauncher)
	if !ok {
		return ErrRelaunchUnsupported
	}
	can, err := r.CanRelaunch()
	if err != nil {
		return err
	}
	if !can {
		return ErrRelaunchUnsupported
	}
	return r.Relaunch()
}

// relaunchOrRecoverSandbox is what a sandbox's Recover and Respawn run: relaunch
// in place when the agent-server can, and re-provision only when it cannot — an
// older sandbox build, or a runtime with no provisioned sandbox at all. Any other
// relaunch failure is returned as is: the sandbox answered, its workspace may
// hold unpushed work, and replacing it would destroy that.
func relaunchOrRecoverSandbox(i *Instance) error {
	return decideRecovery(i, (*Instance).RelaunchInPlace, recoverSandbox)
}

// decideRecovery is relaunchOrRecoverSandbox with its two actions injected, so
// the policy — not the provisioning — can be tested.
func decideRecovery(i *Instance, relaunch, replace func(*Instance) error) error {
	// An explicit --force-reap is a request to REPLACE the sandbox: honouring it
	// by relaunching would retain exactly the work the operator asked to discard,
	// and an unreachable sandbox would fail the relaunch's health probe and never
	// reach the replacement the flag promises.
	if i.forceSandboxReplacement.Load() {
		return replace(i)
	}
	err := relaunch(i)
	if err == nil {
		// The agent process is a new one even though the workspace is the same
		// (#4222): stamp it before the live edge so the run-end classifier can
		// interrupt a task run still in flight.
		i.markAgentRuntimeReplaced(true)
		_ = i.Transition(ConfirmLive())
		return nil
	}
	if relaunchFallsBackToReprovision(err) {
		return replace(i)
	}
	return fmt.Errorf("relaunching the agent in place for session %q: %w", i.Title, err)
}

// SetForceSandboxReplacement records the operator's explicit intent to replace
// the sandbox (`sessions restore --force-reap`) for the recovery about to run.
// The caller clears it when the recovery returns.
func (i *Instance) SetForceSandboxReplacement(force bool) { i.forceSandboxReplacement.Store(force) }

// relaunchFallsBackToReprovision is the whole list of reasons a failed relaunch
// may be followed by re-provisioning: the runtime cannot relaunch at all, or
// there is no sandbox to relaunch in. Everything else — a transport failure, a
// refusal from a sandbox that answered — leaves the workspace presumed alive and
// is returned to the retry loop instead.
func relaunchFallsBackToReprovision(err error) bool {
	return errors.Is(err, ErrRelaunchUnsupported) || errors.Is(err, ErrRemoteSandboxNotProvisioned)
}

// Relaunch is the sandbox-side implementation: the in-sandbox agent-server's
// local runtime. Overlapping requests collapse onto one attempt (relaunchFlight).
func (s *localAgentServer) Relaunch() error { return s.relaunchWith(s.inst.relaunchLocalAgent) }

// relaunchWith runs one relaunch through the single-flight guard.
func (s *localAgentServer) relaunchWith(run func() error) error {
	return s.relaunch.do(func() error {
		s.agentOpMu.Lock()
		defer s.agentOpMu.Unlock()
		return run()
	})
}

// relaunchFlight makes a relaunch ONE serialized operation across its whole
// stop/start. The route discards request cancellation, so a daemon that timed
// out and retried while the first relaunch is still running would otherwise run
// a second one concurrently — stopping the replacement the first just started,
// or racing its startup. A request that arrives mid-flight JOINS the running
// attempt and returns its result, the same shape as the headless archive route.
// A request after it finished is a new relaunch.
type relaunchFlight struct {
	mu     sync.Mutex
	active *relaunchAttempt
}

type relaunchAttempt struct {
	done chan struct{}
	err  error
}

// relaunchJoinTimeout bounds how long a joiner waits for the running attempt;
// the leader is bounded by the same call budget the driving daemon applies.
var relaunchJoinTimeout = AgentRelaunchCallTimeout

func (f *relaunchFlight) do(run func() error) error {
	f.mu.Lock()
	if a := f.active; a != nil {
		f.mu.Unlock()
		select {
		case <-a.done:
			return a.err
		case <-time.After(relaunchJoinTimeout):
			return errors.New("relaunch: a relaunch of this agent is still running; not starting a second one beside it")
		}
	}
	a := &relaunchAttempt{done: make(chan struct{})}
	f.active = a
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.active = nil
		f.mu.Unlock()
		close(a.done)
	}()
	a.err = errors.New("relaunch: attempt aborted before it finished")
	a.err = run()
	return a.err
}

// CanRelaunch is always true for the local runtime: it owns the tmux session.
func (s *localAgentServer) CanRelaunch() (bool, error) { return true, nil }

var (
	_ AgentRelauncher = (*localAgentServer)(nil)
	_ AgentRelauncher = (*remoteAgentServer)(nil)
	_ AgentRelauncher = (*deadRemoteAgentServer)(nil)
)

// relaunchLocalAgent stops a running agent and starts it again through the same
// respawn a local session's Recover uses — the resolved-program flag injection
// and resume rewrite are not re-implemented here.
//
// A running agent is stopped first, and the stop must be CONFIRMED: two agents
// writing one tree is unrecoverable in a way a retry is not, so an unconfirmed
// stop aborts before anything starts (the SwapAgent rule). An agent that is
// already gone needs no stop.
func (i *Instance) relaunchLocalAgent() error {
	b, ok := i.currentBackend().(*LocalBackend)
	if !ok {
		return fmt.Errorf("relaunch: session %q has no local runtime to relaunch in", i.Title)
	}
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil {
		return fmt.Errorf("relaunch: session %q has no tmux binding", i.Title)
	}
	alive, err := i.AgentServer().Alive()
	if err != nil {
		return fmt.Errorf("relaunch: cannot tell whether %q's agent is running: %w", i.Title, err)
	}
	return stopThenRespawn(i.Title, ts, alive, func() error { return b.Respawn(i) })
}

// stopThenRespawn is the order that makes a relaunch safe: the stop runs FIRST
// and EVERY time — even when the agent session is already gone, because tmux can
// no longer enumerate a vanished session's children but the AF_SESSION marker
// outlives it, and the stop's survivor sweep reads that. A child still running
// after its session died makes the stop refuse; skipping the stop for a dead
// session would start a replacement beside that child. The respawn runs only
// when the stop is confirmed.
func stopThenRespawn(title string, ts paneStopper, wasAlive bool, respawn func() error) error {
	if err := confirmAgentStopped(title, ts, wasAlive); err != nil {
		return err
	}
	return respawn()
}

// --- remote client ---

// CanRelaunch asks the sandbox's /v1/health whether it serves the relaunch
// route. Not cached: a relaunch is rare and a stale "no" would pin a modern
// sandbox to the destructive path.
func (s *remoteAgentServer) CanRelaunch() (bool, error) {
	h, err := s.rc.health()
	if err != nil {
		return false, err
	}
	return slices.Contains(h.Features, relaunchFeature), nil
}

// Relaunch asks the sandbox to relaunch its agent in place. Its own budget: the
// call waits for the old agent to exit.
//
// The sandbox resets ITS broker and repaints its own subscribers, but this
// daemon's broker is another subscriber of the old pane: its upstream socket
// survives an in-place respawn, and remoteClientlessChannel deliberately drops
// the sandbox's repaint (the daemon paints its own). So a client still attached
// here would keep the pre-relaunch screen and miss the replacement's output
// until something unrelated reconnected it (#5871). On success every broker
// takes the same recovery a respawned local pane gets: stale capture released,
// ring discarded, fresh dial, every attached client re-seeded with a repaint of
// the replacement's screen.
func (s *remoteAgentServer) Relaunch() error {
	if err := s.rc.callWithin("/v1/agent/relaunch", struct{}{}, nil, AgentRelaunchCallTimeout); err != nil {
		return err
	}
	s.resetBrokerCaptures()
	return nil
}

// A runtime with no live sandbox cannot relaunch in it: the typed
// not-provisioned error sends the caller to re-provisioning.
func (s *deadRemoteAgentServer) Relaunch() error { return s.err() }
func (s *deadRemoteAgentServer) CanRelaunch() (bool, error) {
	return false, s.err()
}

// paneStopper is the tmux session's confirmed-stop, narrowed so the decision
// below can be tested without tmux.
type paneStopper interface {
	CloseAndWaitForPaneExitReportingBlindness() (tmux.PaneState, bool, error)
}

// confirmAgentStopped stops the agent's session and returns nil only when it
// has positive evidence that nothing from it is still running. wasAlive says
// whether the session was running before the stop.
//
//   - an unconfirmed stop, or survivors found by the sweep, refuse: two agents
//     writing one tree is unrecoverable in a way a retry is not;
//   - a session that VANISHED with no pane observed is the normal shape of a
//     dead agent, and its survivor sweep already ran inside the stop, so it
//     proceeds — but the same shape for an agent that was just running means a
//     detached child may still be writing, and refuses.
func confirmAgentStopped(title string, ts paneStopper, wasAlive bool) error {
	state, blind, closeErr := ts.CloseAndWaitForPaneExitReportingBlindness()
	if state == tmux.PaneStateUnknown {
		return fmt.Errorf("relaunch: cannot confirm %q's agent stopped (%v); not starting a replacement, because two agents writing the workspace at once would corrupt the work in it", title, closeErr)
	}
	if state == tmux.PaneStateKnown && blind && wasAlive {
		return fmt.Errorf("relaunch: %q's agent session vanished without its pane ever being observed, so a detached child may still be writing the workspace; not starting a replacement", title)
	}
	if closeErr != nil {
		return fmt.Errorf("relaunch: failed to stop the agent for %q: %w", title, closeErr)
	}
	return nil
}
