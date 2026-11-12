package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// AccountSwapCarryPossible reports whether a same-agent account swap of this
// session would carry its conversation rather than start fresh. It is
// require_carry's input to the handoff engine, and it answers by running the
// swap's own carry plan (planAccountSwapCarry) without a target: every check
// the live swap makes on the outgoing side runs here too — a recorded
// conversation of the provider's own, a source home af can resolve, a
// transcript there it can locate (a single plain rollout, not compressed, not
// continued by a thread/revert rollout), a recorded conversation still the
// newest in its worktree, and a launch command that can resume a named
// conversation (#7152).
//
// What only a target can answer — its home, a rollout of the thread it
// already holds — is judged when the move plans against that target, and a
// conversation can still change between this answer and that plan.
//
// It reads the provider's store, so it must never run under a manager lock;
// AccountSwapCarryRecorded is the in-memory precondition for that.
func (i *Instance) AccountSwapCarryPossible() bool {
	return i.accountSwapCarryRefusal() == ""
}

// accountSwapCarryRefusal is why AccountSwapCarryPossible is false, or "".
func (i *Instance) accountSwapCarryRefusal() string {
	// The same program a same-agent swap plans with: the runtime af recorded
	// at launch, else the pane's frozen command (validateAccountSwapPlan).
	i.mu.RLock()
	program := strings.TrimSpace(i.runtimeProgram)
	ts := i.tmuxLocked()
	account := i.Account
	outgoing := i.reportedConversation
	if len(i.Tabs) > 0 {
		outgoing = i.Tabs[0].Conversation
	}
	i.mu.RUnlock()
	if program == "" && ts != nil {
		program = strings.TrimSpace(ts.Program())
	}
	if program == "" {
		return "af has no record of the command this session launched, so it cannot locate its conversation"
	}
	agent := tmux.DetectAgentFromCommand(program)
	if !carrySupportedAgent(agent) {
		return agent + " conversations cannot be carried"
	}
	carry, reason := planAccountSwapCarry(accountSwapCarryRequest{
		agent:      agent,
		outgoing:   outgoing,
		current:    account,
		program:    program,
		workDir:    i.GetWorktreePath(),
		sourceOnly: true,
	})
	if carry == nil {
		return reason
	}
	if _, _, ok := carry.launch(program); !ok {
		return "the resolved program cannot resume a specific conversation"
	}
	return ""
}
