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
