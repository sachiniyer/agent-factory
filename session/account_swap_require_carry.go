package session

// CarryRequiredError is ValidateAccountSwapRequiringCarry's refusal: the swap's
// own plan would start the replacement on a fresh conversation, which
// require_carry refuses ("refuses a move when the conversation cannot be
// carried to the new account, rather than falling back to a fresh start").
// Reason is the plan's refusal prose; Code is its carry refusal code, the only
// part fit for an event row.
type CarryRequiredError struct {
	Reason string
}

func (e *CarryRequiredError) Error() string {
	return "require_carry refuses a swap that would start a fresh conversation: " + e.Reason
}

// Code is the carry refusal code (handoff.Carry*) naming Reason.
func (e *CarryRequiredError) Code() string {
	return carryRefusalCode(e.Reason)
}

// ValidateAccountSwapRequiringCarry is ValidateAccountSwap for a move made
// under require_carry: when the swap's own plan for a new carry would fall back
// to a fresh conversation, it refuses with a *CarryRequiredError before
// anything is recorded or the runtime is touched, so the caller defers the move
// instead (#7282). The engine's checks run against store snapshots that can
// lag (carryAheadStores); this is the plan against the live stores, so a state
// the snapshot missed — a rollout the target filed since, reviewer's probe P4
// — is still refused rather than started fresh.
//
// It is the live mover's entry point (#6776); the limit watcher's swaps never
// consult require_carry and keep ValidateAccountSwap. A committed swap is not
// refused here: its conversation was decided at the identity checkpoint, after
// the outgoing runtime had stopped.
func (i *Instance) ValidateAccountSwapRequiringCarry(name string) error {
	return i.validateAccountSwapPlan(name, "", false, false, true, "", true)
}

// requiredCarryRefusal is require_carry's verdict on a swap plan: a refusal
// when the plan carries nothing for a new carry, nil otherwise.
func requiredCarryRefusal(requireCarry, committed bool, carry *conversationCarry, fallback string) error {
	if !requireCarry || committed || carry != nil {
		return nil
	}
	if fallback == "" {
		fallback = carryNothingAppliesReason
	}
	return &CarryRequiredError{Reason: fallback}
}
