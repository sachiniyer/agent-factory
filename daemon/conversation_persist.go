package daemon

import "github.com/sachiniyer/agent-factory/session"

// A conversation identity reported by a remote agent-server and not yet durable
// is a durable change of its own (#5826): without it a Codex id reported after
// launch, while the session stays Running, lived in memory only and a daemon
// restart lost the resume handle. The poll persistence treats the pending flag
// as a reason to write, and clears it only after the write succeeded, so a
// failed write is retried on the next tick.

// conversationNeedsWrite reports a reported identity that is still memory-only.
func conversationNeedsWrite(instance *session.Instance) bool {
	return instance.ConversationUnpersisted()
}

// noteConversationWritten clears the pending flag after a successful write of
// data, for the conversation that write actually carried.
func noteConversationWritten(instance *session.Instance, data session.InstanceData, writeErr error) {
	if writeErr != nil {
		return
	}
	switch {
	case len(data.Tabs) > 0 && data.Tabs[0].Conversation != nil:
		instance.MarkConversationPersisted(*data.Tabs[0].Conversation)
	case data.AgentConversation != nil:
		instance.MarkConversationPersisted(*data.AgentConversation)
	}
}
