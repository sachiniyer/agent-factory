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
// The target is compared against ResolvedAgent — what the pane is ACTUALLY
// running — not the stored enum. program_overrides can point an agent name at
// an arbitrary command, and #1116's rule is that every agent-keyed decision
// keys off the resolved command. Keying the same-agent check off the enum would
// let "hand codex off to codex" through whenever an override was in play.
func (i *Instance) ValidateHandoffTarget(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("handoff target agent is required (one of %s)", tmux.SupportedProgramsString())
	}
	if !tmux.IsSupportedProgram(target) {
		return fmt.Errorf("unknown agent %q: handoff target must be one of %s", target, tmux.SupportedProgramsString())
	}
	if current := i.ResolvedAgent(); current == target {
		return fmt.Errorf("session is already running %s", target)
	}
	return nil
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
	if err := i.ValidateHandoffTarget(target); err != nil {
		return AgentHandoff{}, err
	}
	target = strings.TrimSpace(target)

	i.mu.Lock()
	defer i.mu.Unlock()

	if len(i.Tabs) == 0 {
		return AgentHandoff{}, fmt.Errorf("session %q has no agent tab to hand off", i.Title)
	}

	// Record the outgoing agent by what it actually resolved to, falling back to
	// the stored enum when no tmux session has been bound yet (tests, and an
	// instance handed off before its first start).
	outgoing := i.Tabs[0].Conversation
	if strings.TrimSpace(outgoing.Agent) == "" {
		outgoing.Agent = tmux.DetectAgentFromCommand(i.Program)
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

	return entry, nil
}

// RevertHandoff undoes a SwapAgentProgram whose runtime swap then failed,
// restoring the outgoing agent as the recorded program and putting its
// conversation id back.
//
// This exists because the record and the runtime must not disagree. If the pane
// still runs codex while Program says claude, every later decision that keys off
// the program — respawn flag injection, readiness heuristics, the next handoff's
// same-agent check — is made against an agent that is not there. A stale ledger
// entry for a swap that never happened is the same class of lie, so the entry
// comes off too.
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
	return nil
}
