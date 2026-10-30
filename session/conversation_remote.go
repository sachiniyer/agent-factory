package session

import (
	"fmt"
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

// persistedConversationOf is the agent conversation a stored row recorded: the
// agent tab's own, else the mirror every row carries.
func persistedConversationOf(data InstanceData) AgentConversationData {
	for idx, td := range data.Tabs {
		if idx == 0 && td.Conversation != nil && td.Conversation.HasID() {
			return *td.Conversation
		}
	}
	if data.AgentConversation != nil && data.AgentConversation.HasID() {
		return *data.AgentConversation
	}
	return AgentConversationData{}
}

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

// recordReportedConversationFrom records the conversation a remote agent-server
// reported it launched or resumed. An empty report never erases a recorded id (a
// server that predates the field, or one that has not captured yet, reports
// none). Returns true when the in-memory value changed and should be persisted.
//
// The report is bound to the runtime that produced it: rc is the client of the
// agent-server that answered, and it is validated under the instance lock
// against the instance's CURRENT runtime before either conversation field is
// written. A retired runtime's in-flight snapshot — still holding this instance
// pointer after a replacement was bound — is dropped, so it can never overwrite
// the replacement's identity (#5862). Checking the observation generation after
// Snapshot() returns is too late: the write has already happened.
func (i *Instance) recordReportedConversationFrom(rc *remoteAgentClient, conv AgentConversationData) bool {
	if !conv.HasID() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if rc == nil || i.remoteClient != rc {
		return false
	}
	return i.recordReportedConversationLocked(conv)
}

// RecordReportedConversationForTest records a report with no runtime fence, for
// tests that exercise persistence without binding a remote runtime.
func (i *Instance) RecordReportedConversationForTest(conv AgentConversationData) bool {
	if !conv.HasID() {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.recordReportedConversationLocked(conv)
}

func (i *Instance) recordReportedConversationLocked(conv AgentConversationData) bool {
	// Kept apart from Tabs[0]: a launch report arrives before the daemon-side
	// agent tab is seeded, and the seeding step applies it.
	i.reportedConversation = conv
	changed := i.setAgentConversationLocked(conv)
	if changed {
		i.conversationUnpersisted = true
	}
	return changed
}

// ConversationUnpersisted reports whether a reported conversation identity is
// still memory-only (#5826).
func (i *Instance) ConversationUnpersisted() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.conversationUnpersisted
}

// MarkConversationPersisted records that a whole-row write carrying persisted
// has landed. It clears the pending flag only when the conversation that was
// written is the one now held: an identity reported after the written snapshot
// was taken stays pending and is written on the next tick.
func (i *Instance) MarkConversationPersisted(persisted AgentConversationData) {
	i.mu.Lock()
	defer i.mu.Unlock()
	current := i.reportedConversation
	if len(i.Tabs) > 0 {
		current = i.Tabs[0].Conversation
	}
	if current == persisted {
		i.conversationUnpersisted = false
	}
}

// RefuseGuessedRespawn is the agent-server's guard for a restore-launch that
// carries the daemon's recorded conversation. When the agent's tmux session is
// definitively gone the launch would RESPAWN it, and a lazy respawn resumes by
// heuristic ("--continue", "resume --last"), which can attach the replacement to
// a different conversation than the recorded one (#4797). With an exact id
// recorded, af refuses that guess and leaves the session needing attention
// rather than resuming onto another conversation. A surviving session (or one
// whose state could not be established) is reattached, never respawned, so it
// proceeds.
func (i *Instance) RefuseGuessedRespawn(conv AgentConversationData) error {
	if !conv.HasID() {
		return nil
	}
	i.mu.RLock()
	ts := i.tmuxLocked()
	i.mu.RUnlock()
	if ts == nil || ts.ExistsOrUnknown() {
		return nil
	}
	return fmt.Errorf("the agent's tmux session is gone and would be respawned, but resuming conversation %s exactly "+
		"is not something a lazy respawn does (it resumes the latest conversation instead); not guessing", conv.ID)
}
