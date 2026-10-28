package session

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Relaunch the agent in place (#5571 W7.1): the agent-server stops the agent and
// starts it again in its EXISTING workspace with the provider's resume. The
// branch, the tree and any uncommitted work are untouched — nothing is pushed,
// reaped, re-cloned or re-provisioned, and no post-worktree hook re-runs. A
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
	err := i.RelaunchInPlace()
	if err == nil {
		// The agent process is a new one even though the workspace is the same
		// (#4222): stamp it before the live edge so the run-end classifier can
		// interrupt a task run still in flight.
		i.markAgentRuntimeReplaced(true)
		_ = i.Transition(ConfirmLive())
		return nil
	}
	if relaunchFallsBackToReprovision(err) {
		return recoverSandbox(i)
	}
	return fmt.Errorf("relaunching the agent in place for session %q: %w", i.Title, err)
}

// relaunchFallsBackToReprovision is the whole list of reasons a failed relaunch
// may be followed by re-provisioning: the runtime cannot relaunch at all, or
// there is no sandbox to relaunch in. Everything else — a transport failure, a
// refusal from a sandbox that answered — leaves the workspace presumed alive and
// is returned to the retry loop instead.
func relaunchFallsBackToReprovision(err error) bool {
	return errors.Is(err, ErrRelaunchUnsupported) || errors.Is(err, ErrRemoteSandboxNotProvisioned)
}

// Relaunch is the sandbox-side implementation: the in-sandbox agent-server's
// local runtime.
func (s *localAgentServer) Relaunch() error { return s.inst.relaunchLocalAgent() }

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
	if alive {
		state, blind, closeErr := ts.CloseAndWaitForPaneExitReportingBlindness()
		if state == tmux.PaneStateKnown && blind {
			return fmt.Errorf("relaunch: %q's agent session vanished without its pane ever being observed, so a detached child may still be writing the workspace; not starting a replacement", i.Title)
		}
		if state == tmux.PaneStateUnknown {
			return fmt.Errorf("relaunch: cannot confirm %q's agent stopped (%v); not starting a replacement, because two agents writing the workspace at once would corrupt the work in it", i.Title, closeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("relaunch: failed to stop the agent for %q: %w", i.Title, closeErr)
		}
	}
	return b.Respawn(i)
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
func (s *remoteAgentServer) Relaunch() error {
	return s.rc.callWithin("/v1/agent/relaunch", struct{}{}, nil, AgentRelaunchCallTimeout)
}

// A runtime with no live sandbox cannot relaunch in it: the typed
// not-provisioned error sends the caller to re-provisioning.
func (s *deadRemoteAgentServer) Relaunch() error { return s.err() }
func (s *deadRemoteAgentServer) CanRelaunch() (bool, error) {
	return false, s.err()
}
