package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The conversation identity a REMOTE session carries across the agent-server
// wire (#5571 W7.2). The in-sandbox agent-server runs the local runtime, so it is
// the process that injects a Claude session id or captures a Codex rollout; the
// daemon that owns the row never sees either unless the server reports it. These
// helpers are the two ends of that report: the daemon records what the server
// says it launched, and sends the recorded id back when it asks for a launch, so
// a restore comes up on the same conversation instead of the provider's latest.

// SetResumeConversation asks the NEXT launch of this instance to come up on conv
// instead of a fresh conversation — the same request a root-agent heal makes
// through InstanceOptions.ResumeConversation. Like that request it is honored
// only when the resolved command can express it (planCreateConversation).
func (i *Instance) SetResumeConversation(conv AgentConversationData) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.carriedConversation = conv
}

// ConversationResumableHere reports whether conv names a conversation the agent
// could actually resume on THIS machine, from its own provider store. A resume
// flag for an id whose transcript is not here makes the provider exit on launch,
// so a launch into a fresh workspace — whose store holds none of the old
// sandbox's history — must start a new conversation rather than fail. Only
// providers af can inspect return true.
func ConversationResumableHere(program, workingDir string, conv AgentConversationData) bool {
	if !conv.HasID() {
		return false
	}
	switch strings.TrimSpace(conv.Agent) {
	case tmux.ProgramClaude:
		state, err := InspectClaudeProjectConversations(program, workingDir, conv)
		return err == nil && state.RecordedExists
	case tmux.ProgramCodex:
		home, err := tmux.CodexHomeFromCommand(program, workingDir)
		return err == nil && CodexRolloutPathByID(home, conv.ID) != ""
	default:
		return false
	}
}

// RecordReportedConversation records the conversation a remote agent-server
// reported it launched or resumed. An empty report never erases a recorded id (a
// server that predates the field, or one that has not captured yet, reports
// none). Returns true when the in-memory value changed and should be persisted.
func (i *Instance) RecordReportedConversation(conv AgentConversationData) bool {
	if !conv.HasID() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	// Kept apart from Tabs[0]: a launch report arrives before the daemon-side
	// agent tab is seeded, and the seeding step applies it.
	i.reportedConversation = conv
	return i.setAgentConversationLocked(conv)
}
