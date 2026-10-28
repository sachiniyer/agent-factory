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
// Not carried here: an account (W7.6 owns account handoff — a scoped session is
// refused), the conversation id of the new agent (W7.2), and the branch work
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
	// server resolves its program on its own machine.
	SwapAgentTo(target string) error
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
func (s *localAgentServer) SwapAgentTo(target string) error {
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
func (i *Instance) swapAgentInPlace(target string) error {
	target = strings.TrimSpace(target)
	if account, _ := i.AccountSelection(); account != "" {
		return ErrSwapNeedsAccountHandoff
	}
	plan, err := i.PrepareAgentSwap(target)
	if err != nil {
		return fmt.Errorf("cannot hand %q off to %s without stopping its current agent: %w", i.Title, target, err)
	}
	if err := i.Transition(BeginHandoff()); err != nil {
		return err
	}
	entry, err := i.RecordHandoffSwap(target, plan.EffectiveAgent(), HandoffReasonManual, "", false)
	if err != nil {
		_ = i.Transition(AbortHandoff())
		return err
	}
	if _, err := i.SwapAgent(plan); err != nil {
		if rbErr := i.RevertHandoff(entry); rbErr != nil {
			err = fmt.Errorf("%w (and the record could not be reverted: %v)", err, rbErr)
		}
		_ = i.Transition(AbortHandoff())
		return err
	}
	return i.Transition(CommitHandoff())
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

// SwapAgentTo asks the sandbox to swap its agent in place. Its own budget: the
// call waits for the old agent to exit and the new one to start.
func (s *remoteAgentServer) SwapAgentTo(target string) error {
	return s.rc.callWithin("/v1/agent/swap-agent", agentSwapReq{Target: target}, nil, AgentRelaunchCallTimeout)
}

func (s *deadRemoteAgentServer) SwapAgentTo(string) error { return s.err() }
func (s *deadRemoteAgentServer) CanSwapAgent() (bool, error) {
	return false, s.err()
}

// PrepareAgentSwap is the daemon-side half of the handoff's first phase. The
// command itself is resolved and preflighted ON THE HOST when the swap runs, so
// the plan carries only the target; ErrHandoffUnsupported when the sandbox's
// agent-server predates the route.
func (b *remoteAgentBackend) PrepareAgentSwap(i *Instance, target string) (AgentSwapPlan, error) {
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
	if err := sw.SwapAgentTo(plan.target); err != nil {
		if errors.Is(err, ErrRemoteSandboxNotProvisioned) {
			return fmt.Errorf("%w: %w", ErrHandoffUnsupported, err)
		}
		return err
	}
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
