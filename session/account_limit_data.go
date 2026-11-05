package session

import "time"

// AccountSwapData is the durable identity boundary for an automatic swap.
// From may be empty for the ambient identity; the pointer's presence, rather
// than either string, is the recovery obligation.
type AccountSwapData struct {
	Manual  bool   `json:"manual,omitempty"`
	Mission string `json:"mission,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to"`
	// AccountAgent is the agent namespace the To account was selected in — the
	// credential-boundary agent of the command frozen at commit (#4430 review).
	// It must be durable because the post-commit recovery otherwise re-derives
	// the namespace from CURRENT configuration: a program_overrides flip plus a
	// daemon restart would resolve the same account name in a different
	// registry, and even ResolvedPaneProgram cannot arbitrate that — the attach
	// path rewrites the tmux program metadata from current config before any
	// retry reads it. Empty on automatic swaps (their namespace is the session's
	// agent) and on records written before the field existed, where recovery
	// falls back to the old derivation.
	AccountAgent string `json:"account_agent,omitempty"`
	// Program is the incoming launch command frozen at commit — the resolved
	// base before conversation injection. A committed cross-agent swap can be
	// recovered after the identity checkpoint but before the replacement
	// starts, when pane and runtime evidence still describe the OUTGOING
	// agent: without this record the retry would freeze the predecessor's
	// command, and the drift check would then refuse it against the committed
	// AccountAgent namespace on every attempt, stranding the session (#4430
	// review round 7). Empty on records written before the field existed —
	// recovery then falls back to the runtime/pane chain, guarded by
	// AccountAgent so a predecessor's evidence is never mistaken for the
	// committed incoming command.
	Program string `json:"program,omitempty"`
	// ConversationID is a freshly injected Claude id the replacement starts
	// with. It never names a carried conversation: restart recovery re-injects
	// it with --session-id, which would fork a carried one.
	ConversationID string `json:"conversation_id,omitempty"`
	// CarriedConversationID is the outgoing conversation a same-agent swap
	// copied into the incoming account and resumes (#4367). The checkpoint
	// records it only after the copy landed, and a restart re-plans a resume
	// plus an idempotent re-copy from it. At most one of it and ConversationID
	// is set.
	CarriedConversationID string `json:"carried_conversation_id,omitempty"`
	// CarrySourceAccount is the account whose home held the carried
	// conversation. Empty beside a carried id means the ambient identity.
	CarrySourceAccount string `json:"carry_source_account,omitempty"`
	// CarryFallback says why a same-agent swap that should have carried its
	// conversation starts a fresh one instead; the replacement's notice
	// repeats it.
	CarryFallback string `json:"carry_fallback,omitempty"`
	// CarriedLaunchStarted records that a replacement was launched to resume
	// the carried conversation. A committed carry whose replacement then has
	// to be launched again gives up on the resume rather than retrying it
	// forever: the new account may be unable to run it.
	CarriedLaunchStarted    bool                 `json:"carried_launch_started,omitempty"`
	ReplacementPanesStarted bool                 `json:"replacement_panes_started,omitempty"`
	MissionDeliveryStatus   PromptDeliveryStatus `json:"mission_delivery_status,omitempty"`
	// MissionDeliveryAttemptAt is the send-attempt instant that produced
	// MissionDeliveryStatus — the durable boundary the daemon's re-verification
	// watch binds pane turn evidence to, so a turn the agent began after the
	// send can retire an ambiguous notice verdict without an operator
	// attestation (#6767). Zero beside an ambiguous verdict means the evidence
	// predates this field and cannot be re-verified; the operator exits still
	// apply.
	MissionDeliveryAttemptAt time.Time `json:"mission_delivery_attempt_at,omitempty"`
	// OriginalStartupStateUnknown preserves the real lifecycle value while
	// ForStorage projects a pending replacement through the startup-unknown
	// fence understood by the immediately previous release. A current reader
	// restores the value and clears this compatibility-only marker.
	OriginalStartupStateUnknown *bool `json:"original_startup_state_unknown,omitempty"`
}

// AccountLimitObservationData is durable evidence that one named identity hit
// a provider quota wall. Agent is part of the key because equal account labels
// name unrelated credential stores for different providers.
type AccountLimitObservationData struct {
	Agent   string    `json:"agent"`
	Account string    `json:"account"`
	ResetAt time.Time `json:"reset_at,omitempty"`
	// ObservedAt is the instant af first observed THIS wall fact — or last
	// observed it as a new wall event, after the session left its parked
	// state. Re-seeing the unchanged banner still displayed advances nothing
	// (#6802 review). It is the fact's own event time, which is what a
	// retraction is ordered against; ResetAt is the provider's claimed
	// boundary and can be far from when the banner appeared (a lagging banner
	// may claim a reset already past), so it cannot stand in for the
	// sighting. Zero on rows written before the field existed — or dropped by
	// an older binary's rewrite — and a sighting-less observation orders
	// against no retraction at all: see AccountLimitObservationRetracted.
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// AccountLimitRetractionData is durable positive proof that an account was
// serving traffic at ProvenAt — the record timestamp of a usage reading the
// account's own store shows below the wall (#4404, epic #6776). It never
// rewrites the observation: both facts are kept, and admission decides which
// is newer by the time each happened. That ordering is what makes a
// retraction safe against a fresh wall — the wall's own ObservedAt outranks
// an older proof — and it is why a retraction is written only on positive
// proof, never on absence of evidence, a timeout, an unknown reading, or a
// busy pane (#6802 review: pane frames are not accepted-request evidence).
type AccountLimitRetractionData struct {
	Agent   string `json:"agent"`
	Account string `json:"account"`
	// ProvenAt is the usage record's own timestamp: ordering runs on when the
	// provider made the claim, never on when af happened to read it. A
	// reading recorded before the wall's last sighting is the older fact and
	// retracts nothing.
	ProvenAt time.Time `json:"proven_at"`
}

// AccountLimitObservationRetracted reports whether the retraction defeats the
// observation: the newer fact wins by when it happened, never by write order.
// The proof must postdate every time the observation claims:
//   - ObservedAt — a wall sighted after the proof lands is the newer fact and
//     survives;
//   - ResetAt — a wall whose claimed reset is still in the future at proof
//     time survives, because the proof did not happen "after the wall's
//     reset" (#4404's condition); and a reset claimed in the past at sighting
//     time is still ordered, since a wall observed after the proof may be
//     reporting a window that lags.
//
// An observation with no recorded sighting time orders against NOTHING and
// can never lose to a retraction: a row that lost ObservedAt to an older
// binary's rewrite could have been sighted after the proof, and there is no
// way to order them — the untimed wall stands until it expires or a fresher
// sighting stamps it. A zero ProvenAt defeats nothing either: a retraction
// with no event time orders against nothing.
func AccountLimitObservationRetracted(observation AccountLimitObservationData, retraction AccountLimitRetractionData) bool {
	if retraction.ProvenAt.IsZero() {
		return false
	}
	if observation.Agent != retraction.Agent || observation.Account != retraction.Account {
		return false
	}
	if observation.ObservedAt.IsZero() {
		return false
	}
	if !retraction.ProvenAt.After(observation.ObservedAt) {
		return false
	}
	if !observation.ResetAt.IsZero() && !retraction.ProvenAt.After(observation.ResetAt) {
		return false
	}
	return true
}
