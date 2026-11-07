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

// RelaunchConversationReport is the in-sandbox agent-server's account of what
// the relaunch it just performed did to its session's conversation (#6868):
// carried only when that relaunch's respawn resumed this session's OWN
// conversation — the same verdict a local respawn's conversation outcome
// computes, evaluated on the sandbox's own provider store and command. The
// wire field is additive: an agent-server build that predates it answers the
// relaunch without one, and the driving daemon then reports exactly what it
// does today.
type RelaunchConversationReport struct {
	// Session names the session the report describes; the driving daemon
	// drops a report that does not bind to its own title.
	Session string `json:"session,omitempty"`
	// Carried is true only when the relaunch's respawn resumed this session's
	// own conversation.
	Carried bool `json:"carried"`
	// CarryFailure is why it did not — the same texts a local respawn's
	// verdict produces for a fresh start.
	CarryFailure string `json:"carry_failure,omitempty"`
	// Conversation names the provider conversation the relaunch resumed when
	// it resumed one by id — the recorded conversation an exact resume names.
	// A worktree-scoped resume cannot name the conversation it will land on
	// before the launch picks it, and a fresh start has none, so both leave
	// it empty.
	Conversation AgentConversationData `json:"conversation,omitempty"`
}

// AgentRelaunchReporter is the optional reporting half of an in-place relaunch
// (#6868): the relaunch returns the conversation outcome of the respawn it
// ACTUALLY performed, which is the only carried/not-carried answer a driving
// daemon can truthfully report for a sandbox session. The decision between
// relaunching and re-provisioning is made at the call site on the driving
// daemon, so nothing computed BEFORE the relaunch can answer for the branch
// that ran — only the relaunch itself can.
type AgentRelaunchReporter interface {
	// RelaunchReporting relaunches the agent in place and reports what its
	// respawn did to the session's conversation.
	RelaunchReporting() (*RelaunchConversationReport, error)
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
//
// This is also where the retry-limit conversation answer is settled (#6868):
// the relaunch it calls reports its own resume verdict back over the wire, and
// the re-provision branch records its own not-carried, so whatever this call
// actually ran is what a later ResumeConversationOutcome reports.
func relaunchOrRecoverSandbox(i *Instance) error {
	return decideRecovery(i, (*Instance).RelaunchInPlace, recoverSandbox)
}

// decideRecovery is relaunchOrRecoverSandbox with its two actions injected, so
// the policy — not the provisioning — can be tested.
func decideRecovery(i *Instance, relaunch, replace func(*Instance) error) error {
	// A report an earlier respawn left must not describe the branch this one
	// is about to pick (#6868).
	i.clearRespawnConversationReport()
	// An explicit --force-reap is a request to REPLACE the sandbox: honouring it
	// by relaunching would retain exactly the work the operator asked to discard,
	// and an unreachable sandbox would fail the relaunch's health probe and never
	// reach the replacement the flag promises.
	if i.forceSandboxReplacement.Load() {
		i.noteRespawnConversationReport(sandboxReprovisionedReport)
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
		i.noteRespawnConversationReport(sandboxReprovisionedReport)
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

// relaunchWith runs one relaunch through the single-flight guard, dropping the
// attempt's report.
func (s *localAgentServer) relaunchWith(run func() error) error {
	_, err := s.relaunchReporting(func() (*RelaunchConversationReport, error) {
		return nil, run()
	})
	return err
}

// relaunchReporting is relaunchWith for the body that can answer for the
// respawn it ran: the attempt's report joins the attempt's result, so every
// request this attempt served gets the same verdict.
func (s *localAgentServer) relaunchReporting(run func() (*RelaunchConversationReport, error)) (*RelaunchConversationReport, error) {
	return s.relaunch.do(func() (*RelaunchConversationReport, error) {
		s.agentOpMu.Lock()
		defer s.agentOpMu.Unlock()
		return run()
	})
}

// RelaunchReporting is the relaunch's own conversation verdict (#6868): what
// the respawn it performed did to the session's conversation, evaluated on
// this sandbox's own store inside the attempt that ran it.
func (s *localAgentServer) RelaunchReporting() (*RelaunchConversationReport, error) {
	return s.relaunchReporting(s.inst.relaunchLocalAgentReporting)
}

// relaunchFlight makes a relaunch ONE serialized operation across its whole
// stop/start. The route discards request cancellation, so a daemon that timed
// out and retried while the first relaunch is still running would otherwise run
// a second one concurrently — stopping the replacement the first just started,
// or racing its startup. A request that arrives mid-flight JOINS the running
// attempt and returns its result — report included (#6868) — the same shape as
// the headless archive route. A request after it finished is a new relaunch.
type relaunchFlight struct {
	mu     sync.Mutex
	active *relaunchAttempt
}

type relaunchAttempt struct {
	done   chan struct{}
	report *RelaunchConversationReport
	err    error
}

// relaunchJoinTimeout bounds how long a joiner waits for the running attempt;
// the leader is bounded by the same call budget the driving daemon applies.
var relaunchJoinTimeout = AgentRelaunchCallTimeout

func (f *relaunchFlight) do(run func() (*RelaunchConversationReport, error)) (*RelaunchConversationReport, error) {
	f.mu.Lock()
	if a := f.active; a != nil {
		f.mu.Unlock()
		select {
		case <-a.done:
			return a.report, a.err
		case <-time.After(relaunchJoinTimeout):
			return nil, errors.New("relaunch: a relaunch of this agent is still running; not starting a second one beside it")
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
	a.report, a.err = run()
	return a.report, a.err
}

// CanRelaunch is always true for the local runtime: it owns the tmux session.
func (s *localAgentServer) CanRelaunch() (bool, error) { return true, nil }

var (
	_ AgentRelauncher       = (*localAgentServer)(nil)
	_ AgentRelauncher       = (*remoteAgentServer)(nil)
	_ AgentRelauncher       = (*deadRemoteAgentServer)(nil)
	_ AgentRelaunchReporter = (*localAgentServer)(nil)
	_ AgentRelaunchReporter = (*remoteAgentServer)(nil)
	_ AgentRelaunchReporter = (*deadRemoteAgentServer)(nil)
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
	_, err := i.relaunchLocalAgentReporting()
	return err
}

// relaunchLocalAgentReporting is relaunchLocalAgent plus the relaunch's own
// conversation verdict (#6868): the plan the respawn about to run will apply —
// whether it resumes this session's own conversation, and which recorded
// conversation it names when it resumes one by id. The verdict is computed
// where the respawn is, on this runtime's own store and command, so it answers
// for the relaunch that actually ran rather than a check made before it.
func (i *Instance) relaunchLocalAgentReporting() (*RelaunchConversationReport, error) {
	b, ok := i.currentBackend().(*LocalBackend)
	if !ok {
		return nil, fmt.Errorf("relaunch: session %q has no local runtime to relaunch in", i.Title)
	}
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil {
		return nil, fmt.Errorf("relaunch: session %q has no tmux binding", i.Title)
	}
	alive, err := i.AgentServer().Alive()
	if err != nil {
		return nil, fmt.Errorf("relaunch: cannot tell whether %q's agent is running: %w", i.Title, err)
	}
	var report *RelaunchConversationReport
	err = stopThenRespawn(i.Title, ts, alive, func() error {
		outcome, resumed := i.respawnConversationPlan()
		report = &RelaunchConversationReport{
			Session:      i.Title,
			Carried:      outcome.Carried,
			CarryFailure: outcome.CarryFailure,
			Conversation: resumed,
		}
		return b.Respawn(i)
	})
	if err != nil {
		return nil, err
	}
	return report, nil
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
	report, err := s.RelaunchReporting()
	if err != nil {
		return err
	}
	if s.inst != nil {
		s.inst.noteRelaunchConversationReport(s.rc, report)
	}
	return nil
}

// agentRelaunchResp is the relaunch route's response body: the relaunch's own
// resume verdict (#6868). An agent-server that predates the field answers
// without it — Conversation stays nil and the driving daemon reports exactly
// what it does today.
type agentRelaunchResp struct {
	OK           bool                        `json:"ok"`
	Conversation *RelaunchConversationReport `json:"conversation,omitempty"`
}

// RelaunchReporting is Relaunch with the sandbox's own resume verdict returned
// (#6868): nil when the answering agent-server is a build that predates the
// report.
func (s *remoteAgentServer) RelaunchReporting() (*RelaunchConversationReport, error) {
	var resp agentRelaunchResp
	if err := s.rc.callWithin("/v1/agent/relaunch", struct{}{}, &resp, AgentRelaunchCallTimeout); err != nil {
		return nil, err
	}
	s.resetBrokerCaptures()
	return resp.Conversation, nil
}

// A runtime with no live sandbox cannot relaunch in it: the typed
// not-provisioned error sends the caller to re-provisioning.
func (s *deadRemoteAgentServer) Relaunch() error { return s.err() }
func (s *deadRemoteAgentServer) RelaunchReporting() (*RelaunchConversationReport, error) {
	return nil, s.err()
}
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
