package bugreport

import "github.com/sachiniyer/agent-factory/session"

func redactHandoffAccount(entry *session.AgentHandoff) {
	entry.From.ID = ""
	if entry.FromAccount != "" {
		entry.FromAccount = redactedMarker
	}
	if entry.ToAccount != "" {
		entry.ToAccount = redactedMarker
	}
	// The ledger's carry-failure reason (#6768) is the same prose the pending
	// record's CarryFallback gets the marker for: it can name the previous
	// account ("the previous account %q is no longer registered"), and once
	// that account is unregistered the registry scrub cannot find it — so the
	// label would ship verbatim. Presence survives via Carried, the text does
	// not.
	if entry.Conversation.CarryFailure != "" {
		entry.Conversation.CarryFailure = redactedMarker
	}
}

func redactAccountSwapMission(pending *session.AccountSwapData) {
	pending.ConversationID = ""
	// A carried conversation id (#4367) is the same resumable handle, and is
	// cleared the same way.
	pending.CarriedConversationID = ""
	if pending.Mission != "" {
		pending.Mission = redactedMarker
	}
	// The carry-fallback reason is af's own prose, but it can name the
	// previous account ("the previous account %q is no longer registered"), so
	// it takes the label's trade: presence survives, the text does not.
	if pending.CarryFallback != "" {
		pending.CarryFallback = redactedMarker
	}
}
