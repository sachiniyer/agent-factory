package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// demotePendingAccountSwapCarry rewrites a committed carry as the fresh
// conversation the rebuilt plan will actually launch, so the durable record
// never claims a carried id that did not start.
func (i *Instance) demotePendingAccountSwapCarry(account, reason string) error {
	i.mu.Lock()
	pending := i.pendingAccountSwap
	plan := i.accountSwapLaunch
	if pending == nil || pending.To != account || plan == nil || plan.account != account || plan.carry != nil {
		i.mu.Unlock()
		return fmt.Errorf("account swap for %q changed while its conversation carry fell back", i.Title)
	}
	pending.CarriedConversationID = ""
	pending.CarrySourceAccount = ""
	pending.CarriedLaunchStarted = false
	pending.CarryFallback = reason
	// A blind-resume reason belongs to the outcome being demoted: the new
	// committed conversation must not inherit it (#7602 review).
	pending.ResumeUnverifiedReason = ""
	pending.ConversationID = ""
	if plan.conversation.HasID() {
		pending.ConversationID = plan.conversation.ID
	}
	// The record no longer names the committed carry: the respawn makes the
	// rewrite durable before its spawn, as it does a re-minted id (#7216
	// review R2), whatever a later re-plan concludes (#7217 review R1).
	i.pendingAccountSwapUnwritten = true
	manual, from := pending.Manual, pending.From
	i.touchLocked()
	i.mu.Unlock()
	if manual {
		i.refreshPendingManualAccountSwapMission(from, account)
	}
	return nil
}

// refreshPendingManualAccountSwapMission re-renders a committed manual
// mission after its conversation outcome changed. The stored mission was
// rendered for a carried conversation; delivering it now would tell a fresh
// agent that its history is intact.
//
// The brief's identity fields are reconstructed from the pending record's
// OWN durable facts — the same three the daemon threaded onto the
// commit-time brief in daemon/account_swap.go (#4778): the handoff ledger's
// From.Agent records the OUTGOING agent at commit, the pending record's
// AccountAgent/Program records the INCOMING namespace, and its CrossAgent
// pins the frozen admission verdict. None of these re-derive identity from
// CurrentAgentName, which at refresh time may already describe the INCOMING
// runtime the checkpoint installed (a committed codex->claude swap that
// re-plans its committed Claude conversation after a /clear has the new
// agent bound) or be empty after teardown — From == To collapses the brief
// onto Render's same-agent branch (and then lies a carry was attempted
// across the agent boundary, contradicting agent-handoff D2), while
// From == "" falls off the same-agent gate entirely ("It was being done by
// another agent").
//
// When the record carries the verdict, the verdict ALONE picks the branch
// (#7632 review R1): a recorded cross-agent swap renders the cross-agent
// attribution whatever the labels say, and a recorded same-agent swap
// renders same-agent wording even where the labels legitimately differ — a
// namespace is where the account lives, not which agent runs, so the drift
// shape (outgoing codex, account selected in the gemini registry) must not
// split a recorded verdict onto the cross-agent branch. Only a record
// written before the verdict field falls back to the name comparison — the
// From vs AccountAgent check ManualAccountSwapProgram ran at commit, which
// admission's HandoffTargetIsCurrent refusal keeps honest — and a legacy
// record never narrates a carry attempt either (review R2): the
// carry-failure clause is trustworthy only against a recorded same-agent
// verdict, so the refresh renders the neutral wording true under either.
func (i *Instance) refreshPendingManualAccountSwapMission(from, to string) {
	var reason, fromAgent string
	if handoff, ok := i.LastHandoff(); ok {
		reason = handoff.Reason
		fromAgent = strings.TrimSpace(handoff.From.Agent)
	}
	// The outgoing agent name durably survives teardown in the ledger; the
	// live tiers do not. Fall back to CurrentAgentName only when the ledger
	// recorded nothing, the original refresh did.
	if fromAgent == "" {
		fromAgent = i.CurrentAgentName()
	}
	// Recover the incoming effective agent the commit recorded as the
	// credential boundary's namespace. Empty only on records that predate
	// the durable pin (#4430) or where it was not yet threaded: pending's
	// AccountAgent wins, then the frozen launch command's own agent, then
	// the surviving instance pin — the same precedence committedAccountSwap
	// and validateAccountSwapPlan apply, never a fresh live-resolution
	// (a config flip since commit must not relabel the transaction).
	i.mu.RLock()
	pending := i.pendingAccountSwap
	var accountAgent string
	var crossAgent *bool
	if pending != nil {
		accountAgent = pending.AccountAgent
		crossAgent = pending.CrossAgent
	}
	i.mu.RUnlock()
	if strings.TrimSpace(accountAgent) == "" && pending != nil {
		accountAgent = sessionenv.AgentForCommand(pending.Program)
	}
	if strings.TrimSpace(accountAgent) == "" {
		accountAgent = i.AccountAgent()
	}
	brief := i.BuildMissionBrief(accountAgent, "", reason)
	brief.From = fromAgent
	conversation := i.PendingAccountSwapConversation()
	switch {
	case crossAgent != nil && *crossAgent:
		brief.CrossAgent = true
	case crossAgent != nil:
		// A recorded same-agent verdict means the agent did not change, so
		// the brief's To is the outgoing identity — not the account
		// namespace, a registry fact that drifted labels cannot turn into
		// an agent boundary (#7632 review R1).
		brief.To = brief.From
	default:
		// A record written before the verdict field admits no frozen
		// verdict; re-derive it from the recovered identities. Admission
		// never pins a non-empty AccountAgent matching the outgoing agent
		// for a cross-agent commit — admission IS the
		// HandoffTargetIsCurrent refusal of that pair (#4430 round 5) — so
		// the string compare is the verdict for every record old enough to
		// need it. And it never narrates a carry attempt: the
		// carry-failure clause is trustworthy only against a recorded
		// same-agent verdict, so the record renders the neutral wording
		// true under either (#7632 review R2).
		brief.CrossAgent = strings.TrimSpace(fromAgent) != "" &&
			strings.TrimSpace(accountAgent) != "" &&
			strings.TrimSpace(fromAgent) != strings.TrimSpace(accountAgent)
		conversation.CarryFailure = ""
	}
	brief.Conversation = conversation
	mission := brief.Render()
	i.mu.Lock()
	defer i.mu.Unlock()
	pending = i.pendingAccountSwap
	if pending == nil || !pending.Manual || pending.From != from || pending.To != to || pending.Mission == mission {
		return
	}
	pending.Mission = mission
	i.touchLocked()
}
