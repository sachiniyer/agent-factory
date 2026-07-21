package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Handoff reasons. Only the usage-limit reason exists today; the constant is
// named rather than inlined because the automatic trigger (deferred, see
// docs/design/agent-handoff.md §2.2) will add its own and the ledger has to
// distinguish "a human chose this" from "af chose this" after the fact.
const (
	HandoffReasonUsageLimit = "usage limit"
	HandoffReasonManual     = "manual"
)

// AgentHandoff is one entry in a tab's append-only handoff ledger (#2013): the
// record that this session's agent was swapped for another one, mid-work, on
// the same worktree and branch.
//
// It carries the outgoing agent's conversation identity because the swap
// destroys it on the live tab — Tab.Conversation holds exactly one provider id
// and the incoming agent's capture overwrites it. Preserving it here is what
// keeps a handoff reversible: hand back later and the original conversation is
// still addressable, instead of degrading to that provider's "resume whatever
// was most recent in this directory" behavior.
//
// HeadSHA is the attribution boundary. af authors none of the agent's commits
// and writes no commit trailers, so it cannot mark the work itself; what it can
// do is pin the branch tip at the instant of the swap, which turns "who wrote
// which half" into a git range a reviewer can verify rather than a label they
// have to trust.
type AgentHandoff struct {
	// From is the outgoing agent's conversation identity, as far as it was
	// known. Its Agent field is the outgoing agent name even when no
	// conversation id was ever captured.
	From AgentConversationData `json:"from,omitempty"`
	// To is the incoming agent (a tmux.SupportedPrograms name).
	To string `json:"to"`
	// At is when the swap was recorded.
	At time.Time `json:"at"`
	// HeadSHA is the branch tip at swap time — everything at or before it is the
	// outgoing agent's work. Empty when the branch had no commits yet.
	HeadSHA string `json:"head_sha,omitempty"`
	// Reason is why the swap happened (HandoffReason*).
	Reason string `json:"reason,omitempty"`
	// Automatic is false for a user-confirmed handoff. Always false today: only
	// the prompted path is built (design D1). It is recorded anyway so a reviewer
	// reading a ledger written by a future af still learns whether a human was in
	// the loop, rather than inferring it from the af version.
	Automatic bool `json:"automatic,omitempty"`
}

// From/To agent names for display, e.g. "codex → claude".
func (h AgentHandoff) String() string {
	from := strings.TrimSpace(h.From.Agent)
	if from == "" {
		from = "unknown"
	}
	return from + " → " + h.To
}

// AgentProgram returns the instance's configured agent program enum under the
// instance lock.
//
// Program became mutable with #2013 (a handoff rewrites it in place), so the
// reads that can run concurrently with a swap must go through here. The write
// side holds i.mu; an unguarded read of the bare field races it. Construction
// and restore paths touch i.Program directly and are safe: the instance is not
// yet shared at that point.
func (i *Instance) AgentProgram() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Program
}

// Handoffs returns a copy of the agent tab's handoff ledger, oldest first.
func (i *Instance) Handoffs() []AgentHandoff {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if len(i.Tabs) == 0 || len(i.Tabs[0].Handoffs) == 0 {
		return nil
	}
	out := make([]AgentHandoff, len(i.Tabs[0].Handoffs))
	copy(out, i.Tabs[0].Handoffs)
	return out
}

// LastHandoff returns the most recent ledger entry, if any.
func (i *Instance) LastHandoff() (AgentHandoff, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if len(i.Tabs) == 0 || len(i.Tabs[0].Handoffs) == 0 {
		return AgentHandoff{}, false
	}
	return i.Tabs[0].Handoffs[len(i.Tabs[0].Handoffs)-1], true
}

// ValidateHandoffTarget checks that target is a usable handoff destination for
// this instance, without mutating anything. It is the shared precondition for
// the CLI, the RPC, and the TUI so all three refuse the same inputs with the
// same words.
//
// The target is compared against CurrentAgentName, not ResolvedAgent. See that
// function for why: ResolvedAgent answers "which binary is running" and is
// documented to return "" for a wrapper script, which silently disables this
// guard exactly when a user has customized their setup.
func (i *Instance) ValidateHandoffTarget(target string) error {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.validateHandoffTargetLocked(target)
}

// validateHandoffTargetLocked is ValidateHandoffTarget's already-locked half.
// Keeping target identity and runtime eligibility checks in the same instance
// critical section lets SwapAgentProgram validate and mutate one state snapshot.
func (i *Instance) validateHandoffTargetLocked(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("handoff target agent is required (one of %s)", tmux.SupportedProgramsString())
	}
	if !tmux.IsSupportedProgram(target) {
		return fmt.Errorf("unknown agent %q: handoff target must be one of %s", target, tmux.SupportedProgramsString())
	}
	if current := i.currentAgentNameLocked(); current == target {
		return fmt.Errorf("session is already running %s", target)
	}
	return nil
}

// CurrentAgentName reports which agent enum this session should be treated AS.
// It returns "" only when that is genuinely unknowable, and every handoff
// surface — the picker's filter, the same-agent guard, the confirmation copy,
// the ledger's outgoing entry — resolves it through here so they cannot
// disagree about who is being replaced.
//
// It is deliberately NOT ResolvedAgent, and the difference is load-bearing.
// ResolvedAgent answers a different question — "which binary is this pane
// actually running" — for decisions like claude-only flag injection and
// readiness detection (#1116). For those, a wrapper script that af cannot
// identify SHOULD come back empty: injecting claude's flags into an unknown
// command would break it. configuration.md documents that contract ("if you
// wrap an agent in a script, name the script after the agent").
//
// Identity is not that question. A session created as claude is claude even
// when it launches through ~/bin/my-claude-wrapper, and answering "" there is
// not conservative — it is what let the picker offer claude as a handoff target
// for a session already running claude, and let the same-agent guard pass it.
// A self-handoff kills a working agent and restarts it with no conversation, so
// the empty answer authorized the destructive path rather than blocking it.
//
// Precedence runs from most to least direct evidence:
//  1. the running command, when af can identify it — it beats any record,
//     because an override pointing "claude" at codex really is running codex;
//  2. the conversation the agent actually opened, captured at runtime;
//  3. the configured enum, which is what the user asked for and what a handoff
//     rewrites — this is the one that rescues the wrapper-script case.
func (i *Instance) CurrentAgentName() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.currentAgentNameLocked()
}

// currentAgentNameLocked is CurrentAgentName's already-locked half, for callers
// holding i.mu (SwapAgentProgram builds the ledger entry inside its write lock,
// and sync.RWMutex is not reentrant). TmuxSession.Program takes only the tmux
// session's own programMu and never calls back into Instance, so reading it
// under i.mu introduces no lock cycle.
func (i *Instance) currentAgentNameLocked() string {
	if ts := i.tmuxLocked(); ts != nil {
		if agent := tmux.DetectAgentFromCommand(ts.Program()); agent != "" {
			return agent
		}
	}
	if len(i.Tabs) > 0 {
		if recorded := strings.TrimSpace(i.Tabs[0].Conversation.Agent); tmux.IsSupportedProgram(recorded) {
			return recorded
		}
	}
	return tmux.DetectAgentFromCommand(i.Program)
}

// SwapAgentProgram rewrites the instance's agent program in place and appends
// the handoff to the tab's ledger. It mutates state only — the caller re-spawns
// the pane and delivers the mission — so it is safe to call before any
// irreversible teardown and cheap to test on its own.
//
// Clearing Tab.Conversation is load-bearing, not tidiness. respawn feeds the
// program through prepareResumeConversation, which would otherwise hand the
// INCOMING agent the outgoing agent's recorded conversation id. That specific
// call is already guarded (ResumeProgramWithConversationID refuses a
// provider mismatch), but leaving a stale codex id on a tab now running claude
// is a lie in the record that the next reader has to re-derive the guard for.
// The ledger keeps the id; the live slot describes the live agent.
//
// The caller must hold whatever serialization the daemon requires; this method
// takes only the instance lock.
func (i *Instance) SwapAgentProgram(target, reason, headSHA string, automatic bool) (AgentHandoff, error) {
	target = strings.TrimSpace(target)

	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.validateHandoffTargetLocked(target); err != nil {
		return AgentHandoff{}, err
	}
	if err := i.lifecycleViewLocked().ValidateRuntimeAction(RuntimeActionHandoff); err != nil {
		return AgentHandoff{}, err
	}
	return i.recordHandoffSwapLocked(target, reason, headSHA, automatic)
}

// RecordHandoffSwap is the transaction-owned mutation used by the daemon after
// BeginHandoff has raised OpReplacing. Keeping it separate from
// SwapAgentProgram makes both legal orderings explicit: ordinary state-only
// tests require a settled live row, while production replacement requires the
// fence and cannot accidentally validate itself as "busy".
func (i *Instance) RecordHandoffSwap(target, reason, headSHA string, automatic bool) (AgentHandoff, error) {
	target = strings.TrimSpace(target)

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpReplacing {
		return AgentHandoff{}, fmt.Errorf("session %q has no agent replacement in flight", i.Title)
	}
	if err := i.validateHandoffTargetLocked(target); err != nil {
		return AgentHandoff{}, err
	}
	return i.recordHandoffSwapLocked(target, reason, headSHA, automatic)
}

// handoffStorageCheckpoint projects a runtime swap that has completed while its
// in-memory delivery fence is still raised. Disk cannot retain process-local
// operations, but it must not keep claiming the outgoing agent either: a daemon
// crash during readiness would then restore the wrong Program over a pane that
// already runs the target. The persisted recovery posture is the same one a
// generic post-swap delivery failure takes — incoming agent, LiveRunning, no
// outgoing-provider limit metadata — while memory remains OpReplacing until the
// mission is delivered or explicitly parked.
func (i *Instance) handoffStorageCheckpoint() InstanceData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	data := i.toInstanceDataLocked()
	data.Status = Running
	data.Liveness = LiveRunning
	data.InFlightOp = OpNone
	data.LimitResetAt = time.Time{}
	return data
}

func (i *Instance) recordHandoffSwapLocked(target, reason, headSHA string, automatic bool) (AgentHandoff, error) {

	if len(i.Tabs) == 0 {
		return AgentHandoff{}, fmt.Errorf("session %q has no agent tab to hand off", i.Title)
	}

	// Record the outgoing agent through the shared identity resolver, so the
	// ledger names the same agent the guard compared and the confirmation
	// showed. The conversation id alongside it is what makes a hand-back able to
	// re-enter the outgoing agent's own thread, so it is kept as captured.
	outgoing := i.Tabs[0].Conversation
	if strings.TrimSpace(outgoing.Agent) == "" {
		outgoing.Agent = i.currentAgentNameLocked()
	}

	entry := AgentHandoff{
		From:      outgoing,
		To:        target,
		At:        time.Now(),
		HeadSHA:   strings.TrimSpace(headSHA),
		Reason:    strings.TrimSpace(reason),
		Automatic: automatic,
	}

	i.Tabs[0].Handoffs = append(i.Tabs[0].Handoffs, entry)
	i.Tabs[0].Conversation = AgentConversationData{}
	i.Program = target
	// Invalidate outgoing-runtime capture BEFORE its pane is torn down. A capture
	// already waiting on a rollout must not refill the live slot after this record
	// has been rewritten for the incoming agent.
	i.agentRuntimeGeneration++

	return entry, nil
}

// RevertHandoff undoes a SwapAgentProgram whose runtime swap then failed,
// restoring the outgoing agent as the recorded program and putting its
// conversation id back.
//
// This exists because a failed replacement did not establish the incoming
// runtime. Leaving Program set to that unconfirmed agent would make every later
// decision — respawn flag injection, readiness heuristics, the next handoff's
// same-agent check — act as though the swap committed. A stale ledger entry for
// a swap that never completed is the same class of lie, so the entry comes off
// too.
//
// It removes only the trailing entry, and only when it is the one passed in: if
// anything else has appended since, this is no longer an unwind and refusing is
// safer than truncating someone else's record.
func (i *Instance) RevertHandoff(entry AgentHandoff) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if len(i.Tabs) == 0 {
		return fmt.Errorf("session %q has no agent tab", i.Title)
	}
	n := len(i.Tabs[0].Handoffs)
	if n == 0 {
		return fmt.Errorf("session %q has no handoff to revert", i.Title)
	}
	if last := i.Tabs[0].Handoffs[n-1]; last != entry {
		return fmt.Errorf("session %q: the last handoff is not the one being reverted", i.Title)
	}

	i.Tabs[0].Handoffs = i.Tabs[0].Handoffs[:n-1]
	i.Tabs[0].Conversation = entry.From
	if from := strings.TrimSpace(entry.From.Agent); from != "" {
		i.Program = from
	}
	// Generations are monotonic even on rollback. Reusing the old number would
	// make a token from the abandoned target indistinguishable from the restored
	// outgoing runtime.
	i.agentRuntimeGeneration++
	return nil
}
