package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// refreshRootClaudeConversation keeps the live root's durable conversation id
// recoverable. It replaces the id only after its own transcript disappears;
// while that file exists, a newer project transcript may belong to another
// Claude process and is not evidence about this root.
func (m *Manager) refreshRootClaudeConversation(repoID, key, repoRoot string, inst *session.Instance, st *rootEnsureState) {
	// The recorded conversation and the account pin come off ONE locked
	// projection: selectAccountLocked rewrites the account under i.mu during a
	// handoff commit, so reading inst.Account bare is a data race — and even
	// two locked reads could land on either side of that commit and pair one
	// swap's conversation with the next account's transcript store (#4400
	// review).
	snapshot := inst.ToInstanceData()
	var recorded session.AgentConversationData
	if snapshot.AgentConversation != nil {
		recorded = *snapshot.AgentConversation
	}
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
	program, scopeErr := claudeAccountTranscriptProgram(program, snapshot.Account)
	if scopeErr != nil {
		m.logRootClaudeTranscriptWarning(st,
			"root agent for %s could not verify its recorded claude conversation %s against the account-scoped transcript store: %v",
			repoRoot, recorded.ID, scopeErr)
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
			repoRoot, recorded.ID, m.rootClaudeTranscriptBudget())
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

func (m *Manager) rootClaudeTranscriptBudget() time.Duration {
	if m.claudeTranscriptInspectBudget > 0 {
		return m.claudeTranscriptInspectBudget
	}
	return rootClaudeTranscriptInspectBudget
}

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
	// BUFFERED, so an abandoned inspection can deliver and exit rather than
	// parking forever on a send nobody will receive — the leak a bound of this
	// shape introduces if the channel is unbuffered.
	done := make(chan inspection, 1)
	inspect := m.inspectClaudeProjectConversations
	if inspect == nil {
		inspect = session.InspectClaudeProjectConversations
	}
	go func() {
		state, err := inspect(program, repoRoot, recorded)
		m.mu.Lock()
		st.claudeTranscriptInspecting = false
		m.mu.Unlock()
		done <- inspection{state: state, err: err}
	}()

	timer := time.NewTimer(m.rootClaudeTranscriptBudget())
	defer timer.Stop()
	select {
	case result := <-done:
		return result.state, true, result.err
	case <-timer.C:
		// The read is still running and will finish whenever the store
		// answers; its result is dropped. Deliberately not consumed on a later
		// tick: the next interval re-reads, a store that has come back answers
		// promptly, and a stale answer about a transcript store is worth less
		// than the machinery to carry it.
		return session.ClaudeProjectConversationState{}, false, nil
	}
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

// rootAgentResolvedProgram applies the same program_overrides lookup the
// create path applies after the root profile selects its program. Transcript
// verification must inspect the environment of the command that actually runs,
// not the unresolved enum label stored in the profile — and the account-pin
// namespace check must derive the replacement's agent from the same resolved
// command, because an override can cross registries entirely (#4400 review).
func rootAgentResolvedProgram(repoRoot string, ra config.RootAgent) (string, error) {
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

// claudeAccountTranscriptProgram scopes a claude transcript inspection to the
// account the root launches as. Registration relocates claude's ENTIRE config
// root — transcripts included — so the ambient store a bare program inspects
// never holds an account-scoped root's conversation: the recorded id reads
// absent there and is substituted or dropped exactly while being carried
// (#4400 review). The CLAUDE_CONFIG_DIR prefix lands where
// CommandEnvironmentFromCommand models leading shell assignments — the same
// position a program-local override would occupy — so it loses to a
// command-string assignment exactly the way the launch's exported injection
// does. Returns the program unchanged when the account is empty or the
// resolved program is not claude.
func claudeAccountTranscriptProgram(program, account string) (string, error) {
	if strings.TrimSpace(account) == "" {
		return program, nil
	}
	if agent := sessionenv.AgentForCommand(program); agent != tmux.ProgramClaude {
		return program, nil
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	// Selected, not Dir: this path is read once per inspection interval for the
	// life of the root, and a registration can be invalidated between intervals —
	// an `accounts/<agent>` ancestor swapped for a symlink redirects the entire
	// transcript scan outside the registry, where it would persist a foreign
	// store's newest conversation id over the recorded one. Selected re-proves
	// the ancestors and the leaf on every call, so an invalidated registration
	// surfaces here as an inspection warning instead of a durable write (#4400
	// review round 2).
	selected, err := agentaccount.Selected(home, tmux.ProgramClaude, account)
	if err != nil {
		return "", err
	}
	return "CLAUDE_CONFIG_DIR=" + shellquote.Quote(selected.Dir) + " " + program, nil
}
