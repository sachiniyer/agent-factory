package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// refreshRootClaudeConversation keeps the live root's durable conversation id
// recoverable. It replaces the id only after its own transcript disappears;
// while that file exists, a newer project transcript may belong to another
// Claude process and is not evidence about this root.
func (m *Manager) refreshRootClaudeConversation(repoID, key, repoRoot string, rootAgent config.RootAgent, inst *session.Instance, st *rootEnsureState) {
	recorded := inst.AgentConversation()
	if recorded.Agent != tmux.ProgramClaude || !recorded.HasID() {
		return
	}
	if !m.rootClaudeTranscriptInspectionDue(st) {
		return
	}
	program, err := rootAgentTranscriptProgram(repoRoot, rootAgent)
	if err != nil {
		m.logRootClaudeTranscriptWarning(st,
			"root agent for %s could not verify its recorded claude conversation %s against the project transcript store: %v",
			repoRoot, recorded.ID, err)
		return
	}
	state, err := session.InspectClaudeProjectConversations(program, repoRoot, recorded)
	if err != nil {
		m.logRootClaudeTranscriptWarning(st,
			"root agent for %s could not verify its recorded claude conversation %s against the project transcript store: %v",
			repoRoot, recorded.ID, err)
		return
	}
	m.clearRootClaudeTranscriptWarning(st)
	if state.RecordedExists || !state.Latest.HasID() || strings.EqualFold(state.Latest.ID, recorded.ID) {
		return
	}

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		return
	}
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != inst || inst.AgentConversation() != recorded {
		return
	}
	status := inst.GetStatus()
	if status == session.Dead || status == session.Lost || status == session.Archived {
		return
	}
	if !inst.SetAgentConversation(state.Latest) {
		return
	}

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	data := inst.ToInstanceData()
	err = persistInstanceData(repoID, data)
	if err == nil {
		m.publishEvent(agentproto.EventSessionUpdated, data)
	}
	repoStartLock.Unlock()
	if err != nil {
		inst.SetAgentConversation(recorded)
		log.WarningLog.Printf("root agent for %s could not persist replacement claude conversation %s: %v",
			repoRoot, state.Latest.ID, err)
		return
	}
	log.WarningLog.Printf("root agent for %s recorded claude conversation %s has no transcript; persisted newest on-disk project conversation %s instead",
		repoRoot, recorded.ID, state.Latest.ID)
}

func (m *Manager) rootClaudeTranscriptInspectionDue(st *rootEnsureState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := nowFunc()
	if now.Before(st.nextClaudeTranscriptInspection) {
		return false
	}
	st.nextClaudeTranscriptInspection = now.Add(rootClaudeTranscriptInspectionInterval)
	return true
}

func (m *Manager) logRootClaudeTranscriptWarning(st *rootEnsureState, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	m.mu.Lock()
	if st.claudeTranscriptWarning == message {
		m.mu.Unlock()
		return
	}
	st.claudeTranscriptWarning = message
	m.mu.Unlock()
	log.WarningLog.Print(message)
}

func (m *Manager) clearRootClaudeTranscriptWarning(st *rootEnsureState) {
	m.mu.Lock()
	st.claudeTranscriptWarning = ""
	m.mu.Unlock()
}

// rootAgentTranscriptProgram applies the same program_overrides lookup the
// create path applies after the root profile selects its program. Transcript
// verification must inspect the environment of the command that actually runs,
// not the unresolved enum label stored in the profile.
func rootAgentTranscriptProgram(repoRoot string, ra config.RootAgent) (string, error) {
	program := rootAgentProgramForProfile(repoRoot, ra)
	resolved, err := config.ResolveConfig(repoRoot)
	if err != nil {
		return "", err
	}
	return config.ResolveProgram(&resolved.Config, program), nil
}
