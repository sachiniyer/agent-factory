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
	if pending.Mission != "" {
		pending.Mission = redactedMarker
	}
}
