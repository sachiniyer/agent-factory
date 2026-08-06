package session

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	ConversationCaptureInjected     = "injected"
	ConversationCaptureCodexRollout = "codex_rollout"
)

// AgentConversationData is the provider-specific conversation identity for a
// tab. It is additive/rollforward data: older records simply have no
// conversation object, and recovery falls back to the provider's latest-session
// behavior until a new id is captured.
type AgentConversationData struct {
	Agent       string    `json:"agent,omitempty"`
	ID          string    `json:"id,omitempty"`
	CapturedAt  time.Time `json:"captured_at,omitempty"`
	CaptureKind string    `json:"capture_kind,omitempty"`
}

// AgentRuntimeToken binds asynchronous provider discovery to one concrete
// process generation. Its fields stay private so callers can only obtain a
// valid token from an Instance snapshot, not reconstruct one from a matching
// agent name after the runtime has moved on.
type AgentRuntimeToken struct {
	agent      string
	generation uint64
}

// Agent reports the provider the captured runtime actually launched.
func (t AgentRuntimeToken) Agent() string { return t.agent }

func (c AgentConversationData) Empty() bool {
	return strings.TrimSpace(c.Agent) == "" &&
		strings.TrimSpace(c.ID) == "" &&
		c.CapturedAt.IsZero() &&
		strings.TrimSpace(c.CaptureKind) == ""
}

func (c AgentConversationData) HasID() bool {
	return strings.TrimSpace(c.Agent) != "" && strings.TrimSpace(c.ID) != ""
}

func conversationDataPtr(c AgentConversationData) *AgentConversationData {
	if c.Empty() {
		return nil
	}
	cp := c
	return &cp
}

// AgentConversation returns the Agent tab's recorded provider conversation, if
// one has been captured.
func (i *Instance) AgentConversation() AgentConversationData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if len(i.Tabs) == 0 {
		return AgentConversationData{}
	}
	return i.Tabs[0].Conversation
}

// SetAgentConversation records a provider conversation on the Agent tab.
// Returns true when the in-memory value changed and should be persisted.
func (i *Instance) SetAgentConversation(conv AgentConversationData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.setAgentConversationLocked(conv)
}

// AgentRuntimeToken snapshots the provider and runtime generation atomically.
// Capture callers take it before starting their goroutine and must use
// SetAgentConversationForRuntime to commit the eventual result.
func (i *Instance) AgentRuntimeToken() AgentRuntimeToken {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return AgentRuntimeToken{
		agent:      i.resolvedAgentLocked(),
		generation: i.agentRuntimeGeneration,
	}
}

// SetAgentConversationForRuntime commits conv only while token still names the
// live process generation it was captured for. This catches A→B as well as
// A→B→A handoffs; an agent-name comparison alone cannot distinguish the latter.
func (i *Instance) SetAgentConversationForRuntime(token AgentRuntimeToken, conv AgentConversationData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.userKilled || token.agent == "" || token.generation != i.agentRuntimeGeneration ||
		conv.Agent != token.agent || i.resolvedAgentLocked() != token.agent {
		return false
	}
	return i.setAgentConversationLocked(conv)
}

// noteAgentRuntimeReplaced invalidates every capture bound to the prior process.
// Handoff record mutation calls the locked increment before teardown; local
// recovery calls this after re-spawn so both replacement paths close the edge.
func (i *Instance) noteAgentRuntimeReplaced() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.agentRuntimeGeneration++
	i.clearAgentModelChangeLocked()
}

func (i *Instance) resolvedAgentLocked() string {
	if ts := i.tmuxLocked(); ts != nil {
		if program := ts.Program(); strings.TrimSpace(program) != "" {
			return tmux.DetectAgentFromCommand(program)
		}
	}
	return tmux.DetectAgentFromCommand(i.Program)
}

func (i *Instance) setAgentConversationLocked(conv AgentConversationData) bool {
	if len(i.Tabs) == 0 {
		return false
	}
	if i.Tabs[0].Conversation == conv {
		return false
	}
	i.Tabs[0].Conversation = conv
	return true
}

func prepareLaunchConversation(i *Instance, program string) string {
	rewritten, conversation := planCreateConversation(i, program)
	if conversation.HasID() {
		i.SetAgentConversation(conversation)
	}
	return rewritten
}

// planCreateConversation decides which conversation a FIRST launch comes up on.
// A create carrying a previous record's conversation (#2616 — the root agent's
// reap-and-recreate, the one heal that replaces the record instead of
// re-spawning into it) resumes that conversation, exactly as
// LocalBackend.respawn does for every other recovered session. Everything else
// falls through to the ordinary fresh-injection plan, so no normal create
// changes shape.
//
// The carry is honored only when the resolved command can actually express it.
// ResumeProgramWithConversationID refuses when the recorded agent is not the one
// the program will run (a root handed off to another agent, or a root_agents
// program repointed) and when the command already pins its own
// resume/session-id flag — both cases where forcing a resume would hand an id to
// a provider that has never heard of it. Falling back to a fresh agent there is
// the correct outcome; ensureRootAgent reports which of the two happened rather
// than letting them read alike.
func planCreateConversation(i *Instance, program string) (string, AgentConversationData) {
	if carried := i.carriedConversation; carried.HasID() {
		if rewritten, ok := tmux.ResumeProgramWithConversationID(program, carried.Agent, carried.ID); ok {
			return rewritten, carried
		}
		// A command that pins its own resume flag also lands here, and must not be
		// given a freshly injected --session-id on top of it — the injection helper
		// declines for the same reason, so the fall-through below is a no-op there.
	}
	return planLaunchConversation(i.ID, program)
}

// planLaunchConversation is the side-effect-free half of
// prepareLaunchConversation. Handoff preflight uses it to freeze the exact
// first-launch command before the outgoing pane is stopped; the conversation is
// committed to the instance only when that prepared plan is executed.
func planLaunchConversation(instanceID, program string) (string, AgentConversationData) {
	if tmux.DetectAgentFromCommand(program) != tmux.ProgramClaude {
		return program, AgentConversationData{}
	}
	id := instanceID
	if strings.TrimSpace(id) == "" {
		id = newSessionID()
	}
	rewritten, injected := tmux.ClaudeProgramWithSessionID(program, id)
	if !injected {
		return program, AgentConversationData{}
	}
	conversation := AgentConversationData{
		Agent:       tmux.ProgramClaude,
		ID:          id,
		CapturedAt:  time.Now(),
		CaptureKind: ConversationCaptureInjected,
	}
	return rewritten, conversation
}

func prepareResumeConversation(i *Instance, program string) string {
	if rewritten, ok := prepareExactResumeConversation(i, program); ok {
		return rewritten
	}
	return program
}

func prepareExactResumeConversation(i *Instance, program string) (string, bool) {
	conv := i.AgentConversation()
	if !conv.HasID() {
		return program, false
	}
	if rewritten, ok := tmux.ResumeProgramWithConversationID(program, conv.Agent, conv.ID); ok {
		return rewritten, true
	}
	return program, false
}
