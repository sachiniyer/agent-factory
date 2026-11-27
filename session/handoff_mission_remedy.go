package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
)

// The operator-facing remedies for a row that still owes its takeover brief
// from a prior handoff (#4429). Two refusals share this text so they cannot
// drift: the off-box account swap admission (account_swap_offbox.go) and the
// RuntimeActionHandoff admission (runtime_action.go). Both reach a row whose
// pending mission is unresolved, and both must point the operator at the verbs
// that actually resolve the block on exactly that row — never a verb the
// daemon would refuse there.

// pendingHandoffMissionAutoRetryableView mirrors
// Instance.PendingHandoffMissionAutoRetryable off the lifecycle view both
// refusals already format from, so the remedy text and the daemon's routing
// read the same verdict.
func (v LifecycleView) pendingHandoffMissionAutoRetryableView() bool {
	return v.PendingHandoffMission && v.HandoffDeliveryStatus == PromptNotDelivered
}

// canRetryPendingHandoffMissionDeliveryView mirrors
// Instance.CanRetryPendingHandoffMissionDelivery off the lifecycle view: the
// daemon routes retry-limit through retryPendingHandoff only when this is true,
// so the remedy names a resend only on the rows that admit one.
func (v LifecycleView) canRetryPendingHandoffMissionDeliveryView() bool {
	dead := v.Liveness == LiveLost || v.Liveness == LiveDead || v.Liveness == LiveArchived
	return v.PendingHandoffMission && ambiguousHandoffDelivery(v.HandoffDeliveryStatus) &&
		!v.UserKilled && !dead &&
		(v.InFlightOp == OpNone || v.InFlightOp == OpReplacing) &&
		(v.Liveness == LiveRunning || v.Liveness == LiveReady || v.StartupStateUnknown)
}

// canConfirmPendingHandoffDeliveryView mirrors
// Instance.CanConfirmPendingHandoffDelivery off the lifecycle view, so the
// remedy names --delivered only on the rows that admit a retire.
func (v LifecycleView) canConfirmPendingHandoffDeliveryView() bool {
	dead := v.Liveness == LiveLost || v.Liveness == LiveDead || v.Liveness == LiveArchived
	return v.PendingHandoffMission && !v.UserKilled && !dead &&
		(v.InFlightOp == OpNone || v.InFlightOp == OpReplacing) &&
		confirmableHandoffDelivery(v.HandoffDeliveryStatus) &&
		(v.StartupStateUnknown || v.Liveness == LiveRunning || v.Liveness == LiveReady || v.Liveness == LiveLimitReached)
}

// pendingHandoffMissionRemedy renders the operator-facing remedies for a row
// that still owes its handoff mission, from the same predicates the daemon
// routes on (CanRetryPendingHandoffMissionDelivery, CanConfirmPendingHandoffDelivery
// and PendingHandoffMissionAutoRetryable) — mirrored off the lifecycle view both
// refusals already format from, so the off-box swap refusal and
// RuntimeActionHandoff's refusal cannot drift. The scheduled failover reaches
// the off-box admission at LiveLimitReached once the startup-unknown fence is
// past, where CanRetryPendingHandoffMissionDelivery is false (it needs Running,
// Ready or startup-unknown): retry-limit runs the plain limit resume there, so
// naming it as a resend would lie. A positively not-delivered mission belongs to
// automatic recovery; neither retry nor confirm admits it.
func pendingHandoffMissionRemedy(v LifecycleView) string {
	retry := shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit"}, v.Title)
	delivered := shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit", "--delivered"}, v.Title)
	switch {
	case v.pendingHandoffMissionAutoRetryableView():
		// Positive non-delivery: handoff recovery (ResumePendingHandoffs)
		// transfers the mission and the next failover tick is admitted; no
		// operator verb resends it, and CanConfirm refuses a retire too.
		return "handoff recovery replays it automatically; no resend is needed"
	case v.canRetryPendingHandoffMissionDeliveryView():
		// An ambiguous verdict on a running/ready (or startup-unknown) row:
		// the daemon re-verifies the pane and resubmits, and --delivered
		// retires a mission that already landed.
		return fmt.Sprintf("`%s` resends it, `%s` retires it", retry, delivered)
	case v.canConfirmPendingHandoffDeliveryView():
		// An ambiguous verdict at the limit wall: --delivered retires a
		// mission that already landed; retry-limit only un-stalls the agent
		// and leaves the mission owed, resent with retry-limit once the agent
		// is running.
		return fmt.Sprintf("`%s` retires it if it already landed; `%s` only un-stalls the agent and leaves the mission owed (resent with `%s` once the agent is running)", delivered, retry, retry)
	default:
		// Dead/archived rows keep their restore/kill handles; neither a
		// resend nor an attestation is meaningful against a runtime that no
		// longer exists.
		return "recovery or an explicit restore retires it; no resend is meaningful here"
	}
}
