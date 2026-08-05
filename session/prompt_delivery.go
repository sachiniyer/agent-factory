package session

// Prompt-delivery marking (#2960-adjacent, #2948).
//
// "Has a prompt been delivered to this session since I looked?" cannot be
// answered from lifecycle state, and several attempts to do so were wrong in the
// same way. Liveness is a LEVEL: a user's turn drives Ready→Running→Ready, so a
// check that runs after it settles sees an idle session and concludes nothing
// happened. stateEpoch is closer — it moves on both of those edges — but a
// delivery does not touch the lifecycle axes at all, so between the send and the
// poll observing the agent working, the epoch still reads unchanged.
//
// Adoption is an EVENT, not a state, and it needs a counter that only delivery
// moves. An observer captures it alongside the decision it is making and
// compares before acting: same count → nothing was delivered in between;
// different → something was, and a decision made before it is stale.

// NotePromptDelivery records that a prompt is being delivered to this session.
//
// Called immediately BEFORE the send, not after, and deliberately so. The
// daemon's delivery crosses a socket, which makes its failure ambiguous — "never
// sent" versus "sent, reply lost" — and for the question this answers, a
// possible delivery has to count as one. Marking first also removes the window
// where a delivery has landed but the mark has not, which is exactly the race
// that made the earlier lifecycle-based guards wrong.
func (i *Instance) NotePromptDelivery() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.promptDeliveries++
}

// PromptDeliveries returns the number of prompt deliveries marked on this
// session. Capture it before a decision and compare before applying it; the
// value itself carries no meaning beyond "did this change".
func (i *Instance) PromptDeliveries() uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.promptDeliveries
}
