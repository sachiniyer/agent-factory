package session

import "time"

// AccountSwapData is the durable identity boundary for an automatic swap.
// From may be empty for the ambient identity; the pointer's presence, rather
// than either string, is the recovery obligation.
type AccountSwapData struct {
	From                    string `json:"from,omitempty"`
	To                      string `json:"to"`
	ConversationID          string `json:"conversation_id,omitempty"`
	ReplacementPanesStarted bool   `json:"replacement_panes_started,omitempty"`
}

// AccountLimitObservationData is durable evidence that one named identity hit
// a provider quota wall. Agent is part of the key because equal account labels
// name unrelated credential stores for different providers.
type AccountLimitObservationData struct {
	Agent   string    `json:"agent"`
	Account string    `json:"account"`
	ResetAt time.Time `json:"reset_at,omitempty"`
}
