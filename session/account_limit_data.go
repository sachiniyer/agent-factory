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
	AccountAgent            string               `json:"account_agent,omitempty"`
	ConversationID          string               `json:"conversation_id,omitempty"`
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
