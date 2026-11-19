package session

// HandoffConversation reports what the agent serving a session after a
// replacement can read of its predecessor's conversation: the carry a
// same-agent account swap committed, the reason a carry that applied could
// not complete, or the zero value — a cross-agent-style fresh start where no
// carry applied at all. The control-socket and HTTP handoff/retry responses
// carry it additively so a caller can tell whether the conversation carried
// and, when it did not, why (#6768).
type HandoffConversation struct {
	// Carried means the replacement resumes the outgoing conversation.
	Carried bool `json:"carried"`
	// CarryFailure is why the operation starts a fresh conversation — the
	// carry that could not complete, or the resume af could not name. Empty
	// when no carry applied.
	CarryFailure string `json:"carry_failure,omitempty"`
	// ResumeUnverified is why the replacement resumed its committed
	// conversation without the store listing's proof (a committed
	// re-launch's blind verdict, #7596): the transcript store could not be
	// listed. It is its own channel — never CarryFailure, whose readers
	// render a carry failure as a fresh conversation where a resume keeps
	// the earlier turns (#7602 review).
	ResumeUnverified string `json:"resume_unverified,omitempty"`
}

// PreparedAccountSwapConversation reports the conversation outcome of the
// launch plan admission froze, for rendering a notice before the checkpoint.
func (i *Instance) PreparedAccountSwapConversation() HandoffConversation {
	i.mu.RLock()
	defer i.mu.RUnlock()
	plan := i.accountSwapLaunch
	if plan == nil {
		return HandoffConversation{}
	}
	return HandoffConversation{Carried: plan.carry != nil, CarryFailure: plan.carryFallback}
}

// PendingAccountSwapConversation reports the committed swap's conversation
// outcome, for the notice delivered to its replacement.
func (i *Instance) PendingAccountSwapConversation() HandoffConversation {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pending := i.pendingAccountSwap
	if pending == nil {
		return HandoffConversation{}
	}
	return HandoffConversation{
		Carried:          pending.CarriedConversationID != "",
		CarryFailure:     pending.CarryFallback,
		ResumeUnverified: pending.ResumeUnverifiedReason,
	}
}

// stampHandoffConversationLocked writes the pending swap's conversation
// outcome onto the ledger entry the swap's commit recorded, so the answer
// survives the record's retirement — the "and afterwards" half of #6768. A
// manual swap appends its AgentHandoff atomically with the pending record, so
// the trailing entry is always this swap's; an automatic swap records no
// entry at all and the ToAccount guard simply misses. The caller holds i.mu.
func (i *Instance) stampHandoffConversationLocked() {
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || len(i.Tabs) == 0 || len(i.Tabs[0].Handoffs) == 0 {
		return
	}
	last := &i.Tabs[0].Handoffs[len(i.Tabs[0].Handoffs)-1]
	if last.ToAccount != pending.To {
		return
	}
	last.Conversation = HandoffConversation{
		Carried:          pending.CarriedConversationID != "",
		CarryFailure:     pending.CarryFallback,
		ResumeUnverified: pending.ResumeUnverifiedReason,
	}
}
