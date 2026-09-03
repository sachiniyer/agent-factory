package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// refreshRootClaudeConversation keeps the live root's durable conversation id
// recoverable. It replaces the id only after its own transcript disappears;
// while that file exists, a newer project transcript may belong to another
// Claude process and is not evidence about this root.
func (m *Manager) refreshRootClaudeConversation(repoID, key, repoRoot string, inst *session.Instance, st *rootEnsureState) {
	recorded := inst.AgentConversation()
	if recorded.Agent != tmux.ProgramClaude || !recorded.HasID() {
		return
	}
	if !m.rootClaudeTranscriptInspectionDue(st) {
		return
	}
	program := inst.ResolvedPaneProgram()
	if strings.TrimSpace(program) == "" {
		m.logRootClaudeTranscriptWarning(st,
			"root agent for %s could not verify its recorded claude conversation %s against the project transcript store: live pane launch command is unavailable",
			repoRoot, recorded.ID)
		return
	}
	state, inspected, err := m.inspectRootClaudeTranscript(st, program, repoRoot, recorded)
	if !inspected {
		// NOT INSPECTED THIS TICK — and that is the whole distinction. It is
		// not "the transcript is gone", which is the finding that REPLACES the
		// root's recorded conversation id a few lines below; nothing here
		// establishes anything about the store. So the id stands, the advisory
		// check is simply skipped, and the throttle brings it back.
		m.logRootClaudeTranscriptWarning(st,
			"root agent for %s could not verify its recorded claude conversation %s against the project transcript store: the inspection did not finish within %s, so it was not inspected this tick — the recorded conversation is unchanged and the check runs again on its next interval",
			repoRoot, recorded.ID, rootClaudeTranscriptInspectBudget)
		return
	}
	if err != nil {
		m.logRootClaudeTranscriptWarning(st,
			"root agent for %s could not verify its recorded claude conversation %s against the project transcript store: %v",
			repoRoot, recorded.ID, err)
		return
	}
	m.clearRootClaudeTranscriptWarning(st)
	if state.RecordedExists || !state.Resume.HasID() || strings.EqualFold(state.Resume.ID, recorded.ID) {
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
	if !inst.SetAgentConversation(state.Resume) {
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
		m.warn().Printf("root agent for %s could not persist replacement claude conversation %s: %v",
			repoRoot, state.Resume.ID, err)
		return
	}
	m.warn().Printf("root agent for %s recorded claude conversation %s has no transcript; persisted newest on-disk project conversation %s instead",
		repoRoot, recorded.ID, state.Resume.ID)
}

// inspectClaudeProjectConversations is the transcript read this bound exists
// for. A package var so a test can drive the stalled-store case; production
// assigns it once.
var inspectClaudeProjectConversations = session.InspectClaudeProjectConversations

// inspectRootClaudeTranscript runs one transcript inspection for a live root
// and reports whether it FINISHED, so a caller can tell "the store says X" from
// "the store did not answer in time".
//
// THE BOUND IS ON THE CALLER, NOT ON THE READ, and the distinction is not a
// technicality. The work here is os.ReadDir plus a stat per entry — blocking
// syscalls that no context can cancel, unlike the git children every other
// probe in this package bounds. Threading a context into them would be a
// fiction: the syscall returns when the mount answers and not a moment sooner.
// So the inspection is moved onto its own goroutine and the poll goroutine
// stops waiting, which is #3721's remedy rather than #3760's, chosen for the
// same reason #3721 chose it — the thing that must not block is the caller.
//
// It is safe to abandon because the inspection is ADVISORY while the root is
// live (see rootEnsureState): its only effect is replacing a recorded
// conversation id whose transcript has disappeared, and skipping that for one
// interval costs a resumable id being stale for 30 more seconds.
//
// Single-flighted per root, because a deadline that releases the caller does
// not release the read: on a store that never answers, one goroutine per
// throttle interval would pile up for the life of the daemon. The result of an
// abandoned inspection is dropped rather than consumed late — the next
// interval re-reads, and a store that has come back answers it promptly.
func (m *Manager) inspectRootClaudeTranscript(st *rootEnsureState, program, repoRoot string, recorded session.AgentConversationData) (session.ClaudeProjectConversationState, bool, error) {
	m.mu.Lock()
	if st.claudeTranscriptInspecting {
		m.mu.Unlock()
		return session.ClaudeProjectConversationState{}, false, nil
	}
	st.claudeTranscriptInspecting = true
	m.mu.Unlock()

	type inspection struct {
		state session.ClaudeProjectConversationState
		err   error
	}
	// RED (#3782 item 3): run it on the poll goroutine and wait forever, which
	// is what master does — one stalled transcript store stops RefreshStatuses,
	// RestoreLostSessions and the settlement retries for every session on the
	// box.
	result := inspection{}
	result.state, result.err = inspectClaudeProjectConversations(program, repoRoot, recorded)
	m.mu.Lock()
	st.claudeTranscriptInspecting = false
	m.mu.Unlock()
	return result.state, true, result.err
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
	m.warn().Print(message)
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
	repo, err := config.RepoFromPath(repoRoot)
	if err != nil {
		return "", err
	}
	resolved, err := config.ResolveConfigForRepo(repo)
	if err != nil {
		return "", err
	}
	return config.ResolveProgram(&resolved.Config, program), nil
}
