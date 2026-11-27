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
// RuntimeActionHandoff's refusal cannot drift. The remedy describes what
// actually happens to THIS row, for every state the refusal can fire on:
//  1. Not delivered at LiveLimitReached: handoff recovery transfers the mission
//     into the stored prompt (limit_auto_resume defaults OFF), and retry-limit
//     sends it once transferred.
//  2. Not delivered on a running/ready row: ResumePendingHandoffs resubmits the
//     mission automatically; no operator verb is needed, so none is named.
//  3. Ambiguous on a running/ready/startup-unknown row: retry-limit resends it
//     and --delivered retires it.
//  4. Ambiguous at the limit wall: --delivered retires it if it already landed;
//     retry-limit only un-stalls the agent and leaves the mission owed.
//  5. A live row whose delivery is still being verified or settled — no verdict
//     recorded yet, or an op in flight that blocks both retry and confirm: the
//     refusal clears once the verdict settles; neither a resend nor an
//     attestation is meaningful against it yet.
//  6. A dead, lost or archived row: restore or kill; neither a resend nor an
//     attestation is meaningful against a runtime that no longer exists.
func pendingHandoffMissionRemedy(v LifecycleView) string {
	retry := shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit"}, v.Title)
	delivered := shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit", "--delivered"}, v.Title)
	dead := v.Liveness == LiveLost || v.Liveness == LiveDead || v.Liveness == LiveArchived
	// An op other than the handoff's own OpReplacing fence blocks both the
	// retry and the confirm predicates, so a verdict recorded underneath it is
	// not yet actionable — the refusal clears once that op settles.
	opSettling := v.InFlightOp != OpNone && v.InFlightOp != OpReplacing
	switch {
	case dead:
		// State 6: a runtime that no longer exists keeps its restore/kill
		// handles; neither a resend nor an attestation is meaningful here.
		return "recovery or an explicit restore retires it; no resend is meaningful here"
	case v.HandoffDeliveryStatus == "" || opSettling:
		// State 5: a live row whose delivery verdict has not been recorded, or
		// which carries an op the retry and confirm predicates both refuse
		// under. The handoff's delivery is still being verified or settled,
		// and the refusal clears once it settles; name no verb the daemon
		// would refuse against a verdict it has not seen.
		return "the handoff's delivery is still being verified; the refusal clears once it settles"
	case v.pendingHandoffMissionAutoRetryableView() && v.Liveness == LiveLimitReached:
		// State 1: positive non-delivery at the limit wall. Handoff recovery
		// (ResumePendingHandoffs -> retryPendingHandoff) transfers the mission
		// into the stored Prompt and clears the obligation
		// (daemon/handoff_recovery.go:212) — it does not send it, because
		// limit_auto_resume defaults OFF (daemon/limit_resume.go:54-58). The
		// operator sends the transferred prompt with retry-limit, which runs
		// the plain limit resume now that the mission is no longer pending;
		// neither a resend nor --delivered admits a not-delivered row.
		return fmt.Sprintf("handoff recovery transfers the mission into the session's pending prompt; send it with `%s` once transferred (automatic retry is off by default)", retry)
	case v.pendingHandoffMissionAutoRetryableView():
		// State 2: positive non-delivery on a running/ready row. ResumePendingHandoffs
		// resubmits the mission directly (daemon/handoff_recovery.go:226) — no
		// transfer is involved and no operator verb is needed, so none is
		// named; neither a resend nor --delivered admits a not-delivered row.
		return "handoff recovery resubmits the mission automatically; no operator action is needed"
	case v.canRetryPendingHandoffMissionDeliveryView():
		// State 3: an ambiguous verdict on a running/ready (or startup-unknown)
		// row: the daemon re-verifies the pane and resubmits, and --delivered
		// retires a mission that already landed.
		return fmt.Sprintf("`%s` resends it, `%s` retires it", retry, delivered)
	case v.canConfirmPendingHandoffDeliveryView():
		// State 4: an ambiguous verdict at the limit wall: --delivered retires
		// a mission that already landed; retry-limit only un-stalls the agent
		// and leaves the mission owed, resent with retry-limit once the agent
		// is running.
		return fmt.Sprintf("`%s` retires it if it already landed; `%s` only un-stalls the agent and leaves the mission owed (resent with `%s` once the agent is running)", delivered, retry, retry)
	default:
		// Unreachable for a row that reaches this refusal (every reachable
		// shape is named above), but keep a safe fallback rather than an empty
		// string.
		return "recovery or an explicit restore retires it; no resend is meaningful here"
	}
}
