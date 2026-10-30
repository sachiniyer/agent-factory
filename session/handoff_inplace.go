package session

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Agent handoff in place for a sandbox session (#5571 W7.5).
//
// `handoff --to <agent>` swaps the agent in the SAME workspace: the branch, the
// tree and any uncommitted work stay where they are, and only the agent process
// changes. The daemon runs its ordinary handoff transaction against the session's
// remote backend; the backend asks the sandbox's agent-server to do the swap, and
// the agent-server runs the same prepare / record / swap its local sessions use,
// resolving the NEW agent's program on the host (W2.1) — the daemon's own absolute
// path for an agent would be the wrong machine's.
//
// Not carried here: an account across agents (an account change on the same agent is
// W7.6, account_swap_offbox.go; a scoped session is refused here), the conversation id of the new agent (W7.2), and the branch work
// summary in the mission brief (the daemon holds no worktree for a sandbox
// session, so the brief names the goal but not the commits).

// swapAgentFeature is the /v1/health token for POST /v1/agent/swap-agent.
const swapAgentFeature = "agent-swap"

// SwapAgentFeature is exported for the agent-server's /v1/health advertisement.
const SwapAgentFeature = swapAgentFeature

// AgentSwapper is the optional capability of an AgentServer that can replace its
// agent with another in place. Like AgentRelauncher it is NOT a method on
// AgentServer, so every existing implementation stays valid.
type AgentSwapper interface {
	// SwapAgentTo stops the current agent and starts target in the same
	// workspace. target is an agent name (one of tmux.SupportedPrograms); the
	// server resolves its program on its own machine and returns the command it
	// resolved — the evidence the driving daemon records for the new runtime.
	SwapAgentTo(target string) (program string, err error)
	// CanSwapAgent reports whether SwapAgentTo is available, without side
	// effects. An error means the answer could not be determined.
	CanSwapAgent() (bool, error)
}

var (
	_ AgentSwapper = (*localAgentServer)(nil)
	_ AgentSwapper = (*remoteAgentServer)(nil)
	_ AgentSwapper = (*deadRemoteAgentServer)(nil)
)

// ErrSwapNeedsAccountHandoff refuses an in-place swap of a session that carries
// an account: moving an account across agents is W7.6.
var ErrSwapNeedsAccountHandoff = errors.New("an account-scoped sandbox session cannot be handed off in place yet; account handoff is not supported off-box")

// --- sandbox side ---

// SwapAgentTo runs the swap in the in-sandbox instance, serialized with any
// relaunch (agentOpMu): two operations replacing one agent must not interleave.
func (s *localAgentServer) SwapAgentTo(target string) (string, error) {
	s.agentOpMu.Lock()
	defer s.agentOpMu.Unlock()
	return s.inst.swapAgentInPlace(target)
}

// CanSwapAgent is always true for the local runtime: it owns the tmux session.
func (s *localAgentServer) CanSwapAgent() (bool, error) { return true, nil }

// swapAgentInPlace is the daemon's handoff transaction, run locally by the
// agent-server for its own instance: freeze the launch, fence the window, record,
// swap, commit. The mission is delivered by the DRIVING daemon over the ordinary
// prompt path, so no delivery happens here.
func (i *Instance) swapAgentInPlace(target string) (string, error) {
	return i.swapAgentInPlaceWith(target, nil)
}

// swapAgentInPlaceWith is swapAgentInPlace with a step that runs between the
// confirmed stop of the old agent and the start of the new one (the account
// handoff's scope rotation). A failing step aborts the swap before anything starts.
func (i *Instance) swapAgentInPlaceWith(target string, afterStop func() error) (string, error) {
	return i.swapAgentInPlaceFrom(target, "", afterStop)
}

// swapAgentInPlaceFrom is swapAgentInPlaceWith with the launch command fixed: a
// non-empty restart re-executes exactly that command (the account swap's), where
// an empty one resolves the target agent as a handoff does.
func (i *Instance) swapAgentInPlaceFrom(target, restart string, afterStop func() error) (string, error) {
	target = strings.TrimSpace(target)
	if account, _ := i.AccountSelection(); account != "" {
		return "", i.accountSwapRefusal(account)
	}
	var plan AgentSwapPlan
	var err error
	if restart != "" {
		plan, err = i.prepareRestartSwap(target, restart)
	} else {
		plan, err = i.PrepareAgentSwap(target)
	}
	if err != nil {
		return "", fmt.Errorf("cannot hand %q off to %s without stopping its current agent: %w", i.Title, target, err)
	}
	if afterStop != nil {
		prev := plan.afterStop
		plan.afterStop = func() error {
			if prev != nil {
				if err := prev(); err != nil {
					return err
				}
			}
			return afterStop()
		}
	}
	if err := i.Transition(BeginHandoff()); err != nil {
		return "", err
	}
	entry, err := i.RecordHandoffSwap(target, plan.EffectiveAgent(), HandoffReasonManual, "", false)
	if err != nil {
		_ = i.Transition(AbortHandoff())
		return "", err
	}
	if _, err := i.SwapAgent(plan); err != nil {
		if rbErr := i.RevertHandoff(entry); rbErr != nil {
			err = fmt.Errorf("%w (and the record could not be reverted: %v)", err, rbErr)
		}
		_ = i.Transition(AbortHandoff())
		return "", err
	}
	// The command the host resolved and launched: what the driving daemon records
	// as the new runtime's evidence (plan.baseProgram is override-resolved).
	return plan.baseProgram, i.Transition(CommitHandoff())
}

// accountSwapRefusal is the actionable refusal for an account-scoped sandbox
// session. It wraps ErrSwapNeedsAccountHandoff so callers can match it.
func (i *Instance) accountSwapRefusal(account string) error {
	return fmt.Errorf("session %q is scoped to account %q, and handing an account-scoped sandbox session to another agent is not supported yet; "+
		"start the session without --account, or use a local session, to hand it off: %w", i.Title, account, ErrSwapNeedsAccountHandoff)
}

// RefuseOffBoxAccountHandoff refuses `handoff --account` on a session whose
// workspace is off-box and that is not the W7.6 rotation (OffBoxAccountHandoffApplies):
// a sandbox session that carries no account has none to move. Refused up front,
// before anything is stopped.
func (i *Instance) RefuseOffBoxAccountHandoff() error {
	i.mu.RLock()
	remote := i.capabilitiesLocked().Workspace == WorkspaceRemote
	i.mu.RUnlock()
	if !remote {
		return nil
	}
	return fmt.Errorf("session %q runs in a sandbox, and account handoff is not supported off-box yet; hand it off to another agent without --account, or use a local session: %w",
		i.Title, ErrSwapNeedsAccountHandoff)
}

// --- daemon side ---

// CanSwapAgent asks the sandbox's /v1/health whether it serves the swap route.
func (s *remoteAgentServer) CanSwapAgent() (bool, error) {
	h, err := s.rc.health()
	if err != nil {
		return false, err
	}
	return slices.Contains(h.Features, swapAgentFeature), nil
}

type agentSwapReq struct {
	Target string `json:"target"`
}

type agentSwapResp struct {
	OK      bool   `json:"ok"`
	Program string `json:"program"`
}

// SwapAgentTo asks the sandbox to swap its agent in place. Its own budget: the
// call waits for the old agent to exit and the new one to start.
func (s *remoteAgentServer) SwapAgentTo(target string) (string, error) {
	var resp agentSwapResp
	if err := s.rc.callWithin("/v1/agent/swap-agent", agentSwapReq{Target: target}, &resp, AgentRelaunchCallTimeout); err != nil {
		return "", err
	}
	return resp.Program, nil
}

func (s *deadRemoteAgentServer) SwapAgentTo(string) (string, error) { return "", s.err() }
func (s *deadRemoteAgentServer) CanSwapAgent() (bool, error) {
	return false, s.err()
}

// PrepareAgentSwap is the daemon-side half of the handoff's first phase. The
// command itself is resolved and preflighted ON THE HOST when the swap runs, so
// the plan carries only the target; ErrHandoffUnsupported when the sandbox's
// agent-server predates the route.
func (b *remoteAgentBackend) PrepareAgentSwap(i *Instance, target string) (AgentSwapPlan, error) {
	// BEFORE anything else, and before the daemon's record transaction can drop
	// the account: an account-scoped sandbox session is refused up front, with
	// its outgoing credentials still in place. The check is on the daemon's own
	// row because the in-sandbox instance carries no account selection.
	if account, _ := i.AccountSelection(); account != "" {
		return AgentSwapPlan{}, i.accountSwapRefusal(account)
	}
	sw, ok := i.AgentServer().(AgentSwapper)
	if !ok {
		return AgentSwapPlan{}, ErrHandoffUnsupported
	}
	can, err := sw.CanSwapAgent()
	if errors.Is(err, ErrRemoteSandboxNotProvisioned) {
		return AgentSwapPlan{}, fmt.Errorf("%w: %w", ErrHandoffUnsupported, err)
	}
	if err != nil {
		return AgentSwapPlan{}, err
	}
	if !can {
		return AgentSwapPlan{}, ErrHandoffUnsupported
	}
	if !tmux.IsSupportedProgram(target) {
		return AgentSwapPlan{}, fmt.Errorf("unknown agent %q: handoff target must be one of %s", target, tmux.SupportedProgramsString())
	}
	return AgentSwapPlan{target: target, baseProgram: target, program: target}, nil
}

// SwapAgent replaces the sandbox's agent through its agent-server.
func (b *remoteAgentBackend) SwapAgent(i *Instance, plan AgentSwapPlan) error {
	sw, ok := i.AgentServer().(AgentSwapper)
	if !ok {
		return ErrHandoffUnsupported
	}
	// The mission brief is assembled BEFORE the swap, the point at which a local
	// handoff captures it after stopping the old agent: a failure here aborts with
	// the outgoing agent untouched. It carries the goal (the stored prompt or
	// --brief); the branch summary needs a worktree the daemon does not hold.
	if err := plan.CaptureAfterStop(); err != nil {
		return err
	}
	program, err := sw.SwapAgentTo(plan.target)
	if err != nil {
		if errors.Is(err, ErrRemoteSandboxNotProvisioned) {
			return fmt.Errorf("%w: %w", ErrHandoffUnsupported, err)
		}
		return err
	}
	// Replace the outgoing runtime's command evidence with the host-resolved
	// command of the new one. CurrentAgentName and ResolvedAgent prefer this
	// evidence over the recorded Program, so leaving the old command would report
	// the outgoing agent and apply its readiness heuristics to the new one. With
	// no answer the claim is retired rather than left naming an agent that is gone.
	i.replaceRuntimeProgram(program)
	// The stored launch script names the agent that was just replaced.
	i.retireLaunchScript()
	// A new agent process, same workspace (#4222).
	i.markAgentRuntimeReplaced(true)
	return nil
}

// offBoxSwapAPILocked reports whether this instance's sandbox is known to serve
// the swap route — a cache read, no network under i.mu.
func (i *Instance) offBoxSwapAPILocked() bool {
	rs := i.remoteTabServerCachedLocked()
	return rs != nil && rs.hasFeatureCached(swapAgentFeature)
}

// replaceRuntimeProgram records program as the runtime's launch evidence, or
// retires the claim when it is empty.
func (i *Instance) replaceRuntimeProgram(program string) {
	program = strings.TrimSpace(program)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.setRuntimeEvidenceLocked(program, 0, 0)
}

// handoffInPlaceBackend says which off-box backends hand off in place: the ones
// whose agent-server this slice drives and whose provisioner is shared (ssh and
// sandbox). docker embeds the same remote backend but is not a parity goal here.
func handoffInPlaceBackend(backendType string) bool {
	return backendType == "ssh" || backendType == "sandbox"
}
