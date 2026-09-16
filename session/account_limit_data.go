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
	CarryFallback           string               `json:"carry_fallback,omitempty"`
	ReplacementPanesStarted bool                 `json:"replacement_panes_started,omitempty"`
	MissionDeliveryStatus   PromptDeliveryStatus `json:"mission_delivery_status,omitempty"`
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
}
