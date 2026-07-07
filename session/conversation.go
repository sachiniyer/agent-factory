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

// SetTabConversation records a provider conversation on a named tab. It is used
// by daemon-owned post-spawn capture; tabs without a matching name are ignored
// so a late capture racing with tab close degrades to a no-op.
func (i *Instance) SetTabConversation(name string, conv AgentConversationData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, tab := range i.Tabs {
		if tab.Name != name {
			continue
		}
		if tab.Conversation == conv {
			return false
		}
		tab.Conversation = conv
		return true
	}
	return false
}

func prepareLaunchConversation(i *Instance, program string) string {
	if tmux.DetectAgentFromCommand(program) != tmux.ProgramClaude {
		return program
	}
	id := i.ID
	if strings.TrimSpace(id) == "" {
		id = newSessionID()
	}
	rewritten, injected := tmux.ClaudeProgramWithSessionID(program, id)
	if !injected {
		return program
	}
	i.SetAgentConversation(AgentConversationData{
		Agent:       tmux.ProgramClaude,
		ID:          id,
		CapturedAt:  time.Now(),
		CaptureKind: ConversationCaptureInjected,
	})
	return rewritten
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
