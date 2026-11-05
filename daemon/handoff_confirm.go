package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
)

// ConfirmHandoffDeliveryRequest asks the daemon to retire a pending handoff
// mission WITHOUT resending it (#4429). It is the operator's attestation half
// of the ambiguous-delivery exit: after inspecting the pane and seeing the
// incoming agent already acting on its takeover brief, the operator confirms —
// the daemon probes that a runtime answers at this binding, then settles the
// replacement fence, clears the startup-unknown flag, and retires the durable
// obligation in one commit.
//
// It is deliberately a separate verb rather than a flag on ResumeFromLimit: a
// daemon predating it must REFUSE, not silently run a retry — a resend is the
// exact double-delivery the confirm exists to prevent.
type ConfirmHandoffDeliveryRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id; see ResumeFromLimitRequest.ID. The verb
	// retires a durable obligation, so a misroute would mark a mission
	// delivered on the wrong session.
	ID string `json:"id"`
}

type ConfirmHandoffDeliveryResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Conversation is what the confirmed replacement can read of the previous
	// conversation (#6768): the carry its committed swap recorded, or the
	// fresh start an agent handoff always means.
	Conversation session.HandoffConversation `json:"conversation"`
	// The confirmation mutates durable state before its settlement write; a
	// persist failure after that point is a committed outcome, not a refusal.
	MutationOutcome
}

func (s *controlServer) ConfirmHandoffDelivery(req ConfirmHandoffDeliveryRequest, resp *ConfirmHandoffDeliveryResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	performed, conversation, err := s.manager.confirmHandoffDeliveryConversation(req)
	resp.OK = performed
	resp.Conversation = conversation
	if !resp.MutationOutcome.record(err) {
		return err
	}
	if !resp.OK {
		resp.Reason = "the session changed or another operation owns it"
	}
	return nil
}

// confirmHandoffDelivery retires a pending handoff delivery obligation on the
// operator's attestation (#4429). It is the counterpart to retryPendingHandoff
// for the "the mission already landed" half of an ambiguous verdict — no
// resend, no second submission, just the settle the wedge never offered.
//
// Locking mirrors the send paths exactly: per-(repo,title) target lock FIRST,
// then the per-session op lock, with identity and eligibility re-verified
// under both. The probe below is what an attestation cannot supply: a runtime
// must answer at this binding for "it landed" to be confirmable — a dead pane
// means restore/kill owns the row instead.
func (m *Manager) confirmHandoffDelivery(req ConfirmHandoffDeliveryRequest) (bool, error) {
	performed, _, err := m.confirmHandoffDeliveryConversation(req)
	return performed, err
}

func (m *Manager) confirmHandoffDeliveryConversation(req ConfirmHandoffDeliveryRequest) (bool, session.HandoffConversation, error) {
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return false, session.HandoffConversation{}, err
	}
	if instance == nil {
		return false, session.HandoffConversation{}, fmt.Errorf("session %q not found", title)
	}

	// Admission before the locks: this verb covers both pending obligations —
	// the agent-handoff mission and the manual account swap's notice. Neither
	// is eligible without a recorded ambiguous-or-positive verdict.
	if !instance.CanConfirmPendingHandoffDelivery() && !instance.CanConfirmPendingManualAccountSwapDelivery() {
		return false, session.HandoffConversation{}, fmt.Errorf("session %q has no unconfirmed handoff delivery to settle — inspect `af sessions get %s` for its delivery state", title, title)
	}

	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return false, session.HandoffConversation{}, nil
	}
	m.mu.Unlock()

	unlock := m.lockTarget(repoID, instance.Title)
	defer unlock()

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		return false, session.HandoffConversation{}, nil
	}
	defer opLock.Unlock()

	// Re-verify under the locks: a kill, archive, or another handoff may have
	// moved the session since the admission check.
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != instance || instance.IsTearingDown() {
		return false, session.HandoffConversation{}, nil
	}
	mission := instance.PendingHandoffMission()
	confirmAgent := mission != "" && instance.CanConfirmPendingHandoffDelivery()
	from, to, swapPending := instance.PendingAccountSwap()
	confirmSwap := swapPending && instance.CanConfirmPendingManualAccountSwapDelivery()
	if !confirmAgent && !confirmSwap {
		return false, session.HandoffConversation{}, nil
	}
	// Capture the conversation outcome while the pending record still holds it:
	// the confirm below retires it (#6768).
	var conversation session.HandoffConversation
	switch {
	case confirmSwap:
		conversation = instance.PendingAccountSwapConversation()
	case confirmAgent:
		conversation = session.HandoffConversation{CarryFailure: crossAgentConversationReason}
	}

	// af's half of the proof: a runtime must answer at this binding. The
	// operator attests the mission landed; the probe keeps that attestation
	// from resurrecting a tombstone-shaped pane. probeUnknown refuses rather
	// than guesses — an unreachable runtime cannot have its delivery confirmed.
	// This is an operator one-shot RPC under the per-session op lock, so the
	// probe uses probeLivenessForOperator's budget (remoteConfirmProbeTimeout),
	// not the poll loop's 5s tie-break budget — see remoteloss.go.
	switch probe := probeLivenessForOperator(instance, instance.AgentServer()); probe {
	case probeAlive:
	default:
		return false, session.HandoffConversation{}, fmt.Errorf(
			"session %q's runtime could not be confirmed live (%s); "+
				"if the pane is gone, restore or kill owns this row — confirmation retires the mission, it does not resurrect the runtime",
			title, probe.notAliveReason())
	}

	switch {
	case confirmAgent:
		err = instance.ConfirmPendingHandoffDelivery(mission)
	case confirmSwap:
		err = instance.ConfirmPendingManualAccountSwapDelivery(from, to)
	}
	if err != nil {
		return false, session.HandoffConversation{}, err
	}
	if perr := m.persistSettlement(repoID, key, instance); perr != nil {
		return true, conversation, &mutationCommittedError{err: fmt.Errorf(
			"confirmed the pending handoff delivery, but could not persist its settlement: %w", perr)}
	}
	m.clearPendingHandoffRetry(repoID, instance)
	m.info().Printf("handoff %q: pending delivery confirmed by operator; mission retired without resend", title)
	return true, conversation, nil
}
