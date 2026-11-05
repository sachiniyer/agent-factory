package session

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	ConversationCaptureInjected         = "injected"
	ConversationCaptureCodexRollout     = "codex_rollout"
	ConversationCaptureClaudeTranscript = "claude_transcript"
	// ConversationCaptureCarried marks a conversation a same-agent account
	// swap copied into the incoming account's home and resumed (#4367).
	ConversationCaptureCarried = "carried"
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
		// A reloaded off-box row holds its handle until Launch seeds the agent tab.
		return i.reportedConversation
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
	if !i.runtimeTokenCurrentLocked(token, conv) {
		return false
	}
	return i.setAgentConversationLocked(conv)
}

// runtimeTokenCurrentLocked reports whether token still names the live process
// generation and conv belongs to its provider. Callers hold i.mu.
func (i *Instance) runtimeTokenCurrentLocked(token AgentRuntimeToken, conv AgentConversationData) bool {
	return !i.userKilled && token.agent != "" && token.generation == i.agentRuntimeGeneration &&
		conv.Agent == token.agent && i.resolvedAgentLocked() == token.agent
}

// noteAgentRuntimeReplaced invalidates every capture bound to the prior process.
// Handoff record mutation calls the locked increment before teardown; local
// recovery calls this after re-spawn so both replacement paths close the edge.
func (i *Instance) noteAgentRuntimeReplaced() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.agentRuntimeGeneration++
	i.touchLocked()
	i.clearAgentModelChangeLocked()
}

// resolvedAgentLocked detects the agent from the command the session actually
// runs. Two forms of that evidence, in precedence order: the live pane's
// program locally, and the launch-boundary runtime record — which is what a
// REMOTE session has instead of a local tmux binding (#5108). Either way a
// known command decides the answer outright, even when the answer is "no
// known agent": a program_overrides entry pointing an agent name at a plain
// shell must not resurrect the enum and start waiting on that agent's prompt
// glyph. Only with no recorded command at all does detection fall back to the
// configured enum.
func (i *Instance) resolvedAgentLocked() string {
	if ts := i.tmuxLocked(); ts != nil {
		if program := ts.Program(); strings.TrimSpace(program) != "" {
			return tmux.DetectAgentFromCommand(program)
		}
	}
	if program := strings.TrimSpace(i.runtimeProgram); program != "" {
		return tmux.DetectAgentFromCommand(program)
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
	i.touchLocked()
	return true
}

// retireConversationsLocked drops every conversation id this instance holds
// outside the agent tab — the carried request and the remote-reported handle.
// A provider conversation lives in ONE account's store, so crossing the account
// boundary must leave no id behind that a later respawn could name against the
// other account's home (#6189). The caller holds i.mu and clears the agent tab's
// own conversation itself.
func (i *Instance) retireConversationsLocked() {
	i.carriedConversation = AgentConversationData{}
	i.reportedConversation = AgentConversationData{}
}

// RetireConversationForFreshRuntime drops the agent tab's recorded
// conversation when a refused resume committed a FRESH pane: the id that
// could not be resumed cannot name the replacement's conversation, and
// leaving it recorded would suppress every later capture arm — the next
// restore would refuse it again and start fresh forever (#6769 review). The
// clear is conditional on the exact id that was refused: a conversation
// recorded between refusal and commit is newer evidence than the refusal and
// is left for the next restore's staleness check to judge. snap is the
// capture baseline taken BEFORE the fresh spawn, parked for the deferred arm
// so the replacement's own rollout — which may already exist — is not
// excluded from its own discovery.
func (i *Instance) RetireConversationForFreshRuntime(refusedID string, snap ConversationCaptureSnapshot) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.Tabs) > 0 && i.Tabs[0].Conversation.ID == refusedID && refusedID != "" {
		i.Tabs[0].Conversation = AgentConversationData{}
		i.touchLocked()
	}
	if snap.CapturesCodex() {
		s := snap
		i.pendingConversationCapture = &s
	}
}

// TakePendingConversationCapture hands a respawn-parked capture baseline to
// the next deferred arming, consuming it. ok=false means the arm builds its
// baseline the usual way.
func (i *Instance) TakePendingConversationCapture() (ConversationCaptureSnapshot, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingConversationCapture == nil {
		return ConversationCaptureSnapshot{}, false
	}
	snap := *i.pendingConversationCapture
	i.pendingConversationCapture = nil
	return snap, true
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
	if rewritten, ok, _ := prepareExactResumeConversation(i, program); ok {
		return rewritten
	}
	return program
}

// prepareExactResumeConversation rewrites program to resume this session's own
// recorded conversation, or refuses: a recorded Codex id that is missing,
// provably stale (a newer top-level rollout for the same working directory
// exists), or unconfirmable (a newer rollout cannot be correlated either way)
// no longer names the conversation the pane is in, so a restore must start
// fresh rather than continue a thread the user left (#6769). A refused
// Codex resume comes back with the reason; every other non-resume case
// (nothing recorded, the command cannot express a resume) stays reason-free
// because there is nothing to warn about.
func prepareExactResumeConversation(i *Instance, program string) (string, bool, string) {
	conv, carried := i.resumableConversationAndSource()
	if !conv.HasID() {
		return program, false, ""
	}
	rewritten, ok := tmux.ResumeProgramWithConversationID(program, conv.Agent, conv.ID)
	if !ok {
		return program, false, ""
	}
	if !carried && conv.Agent == tmux.ProgramCodex {
		if reason := i.staleRecordedCodexReason(program, conv.ID); reason != "" {
			return program, false, reason
		}
	}
	return rewritten, true, ""
}

// resumableConversation is the conversation a restore would resume: the
// recorded one, or a launch pinned to a specific conversation before the agent
// tab exists (an adopted workspace's archived one): the same exact id, never a
// guess.
func (i *Instance) resumableConversation() AgentConversationData {
	conv, _ := i.resumableConversationAndSource()
	return conv
}

// resumableConversationAndSource distinguishes the tab-recorded id — af's own
// evidence about what the pane ran — from a carried id, which is a pin the
// adopt/swap paths set before the tab exists. Only the former is the restore
// staleness guard's business: a carried id's provenance was checked where it
// was set.
func (i *Instance) resumableConversationAndSource() (AgentConversationData, bool) {
	if conv := i.AgentConversation(); conv.HasID() {
		return conv, false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.carriedConversation, true
}

// staleRecordedCodexReason answers whether the recorded Codex conversation id
// still names the pane's conversation: "" when af can prove it does or when
// there is nothing newer to compare, a refusal reason when the rollout is
// missing, a newer same-worktree top-level thread exists, or the newer set
// cannot be correlated either way. Fail closed: "we cannot prove current"
// and "provably stale" both refuse, because silently resuming the wrong
// thread is the bug this guard exists to kill.
func (i *Instance) staleRecordedCodexReason(program, id string) string {
	workDir := i.GetWorktreePath()
	home, err := i.codexHomeForLaunch(program, workDir)
	if err != nil || strings.TrimSpace(home) == "" {
		return "af cannot resolve the Codex home this session writes to"
	}
	files, err := codexRolloutFiles(home)
	if err != nil {
		return "af could not verify the recorded conversation against the Codex home this session uses"
	}
	var recordedPath string
	for file := range files {
		if strings.EqualFold(codexConversationIDFromPath(file), id) {
			if recordedPath != "" {
				// Two rollouts claim the same id: "current" cannot be proven.
				return "af could not confirm the recorded conversation is still the newest one in this worktree"
			}
			recordedPath = file
		}
	}
	if recordedPath == "" {
		return "the recorded conversation's rollout is missing from the Codex home this session uses"
	}
	switch checkCodexRecordedStaleness(recordedPath, files, pathutil.ResolveForCompare(LaunchCaptureWorkingDir(program, workDir))) {
	case codexRolloutStale:
		return staleCarryReason
	case codexRolloutUncorrelated:
		return "af could not confirm the recorded conversation is still the newest one in this worktree"
	case codexRolloutRecordedGone:
		return "the recorded conversation's rollout is missing from the Codex home this session uses"
	}
	return ""
}

// codexStalenessVerdict is the outcome of comparing a recorded rollout against
// the newer files in its store.
type codexStalenessVerdict int

const (
	codexRolloutCurrent codexStalenessVerdict = iota
	codexRolloutStale
	codexRolloutUncorrelated
	codexRolloutRecordedGone
)

// checkCodexRecordedStaleness is the mechanical half shared by the restore
// guard and the carry staleness check: stat the recorded rollout, keep the
// enumerated files strictly newer than it, and correlate those to launchDir —
// an empty launchDir leaves every newer top-level file a candidate, which the
// callers' fail-closed reading treats correctly. files is the store listing
// the caller already took, so a check never walks the store twice.
func checkCodexRecordedStaleness(recordedPath string, files map[string]struct{}, launchDir string) codexStalenessVerdict {
	recorded, err := os.Lstat(recordedPath)
	if err != nil {
		return codexRolloutRecordedGone
	}
	var newer []string
	for path := range files {
		if path == recordedPath {
			continue
		}
		if info, serr := os.Lstat(path); serr == nil && info.ModTime().After(recorded.ModTime()) {
			newer = append(newer, path)
		}
	}
	if len(newer) == 0 {
		return codexRolloutCurrent
	}
	matches, uncorrelated := codexRolloutFilesForWorkingDir(newer, launchDir)
	if len(matches) > 0 {
		return codexRolloutStale
	}
	if uncorrelated > 0 {
		return codexRolloutUncorrelated
	}
	return codexRolloutCurrent
}

// codexHomeForLaunch resolves the rollout store this session writes to the way
// the pane sees it. An account-scoped session's CODEX_HOME rides in through the
// session environment — never the command text — so the command model alone
// resolves the ambient home and every store walk misses the account's dir.
func (i *Instance) codexHomeForLaunch(program, workDir string) (string, error) {
	if account, _ := i.AccountSelection(); strings.TrimSpace(account) != "" {
		selected, err := selectAccountInNamespace(i.AccountAgent(), account)
		if err != nil {
			return "", fmt.Errorf("resolve account %q's Codex home: %w", account, err)
		}
		return selected.Dir, nil
	}
	return tmux.CodexHomeFromCommand(program, workDir)
}

// SessionCodexCaptureBaseline builds the capture snapshot a deferred arming
// needs when the launch-time one is gone — after a daemon restart, or for a
// session that predates capture. It binds the store and working directory to
// the command that actually launched the live runtime — durable evidence a
// program_overrides edit cannot move (#6769 review) — falling back to a fresh
// resolution only for rows that predate runtime evidence.
func (i *Instance) SessionCodexCaptureBaseline() ConversationCaptureSnapshot {
	program := i.RuntimeProgram()
	if strings.TrimSpace(program) == "" {
		program = resolveLaunchProgramForInstance(i).command
	}
	return i.sessionCodexCaptureBaselineFor(program)
}

// sessionCodexCaptureBaselineFor builds the snapshot against an explicit
// launch command. Only a local workspace can answer it: a remote pane writes
// its provider store inside its own sandbox, the daemon's account registry is
// a DIFFERENT store, and a remote row's empty worktree would leave rollout
// correlation unscoped — so a stray local rollout could be recorded as the
// remote session's identity. Remote identities arrive through the
// agent-server reporting path in conversation_remote.go (#6769 review).
func (i *Instance) sessionCodexCaptureBaselineFor(program string) ConversationCaptureSnapshot {
	if i.Capabilities().Workspace != WorkspaceLocalWorktree {
		return ConversationCaptureSnapshot{}
	}
	workDir := i.GetWorktreePath()
	if tmux.DetectAgentFromCommand(program) != tmux.ProgramCodex || i.AgentProgram() != tmux.ProgramCodex {
		return ConversationCaptureSnapshot{}
	}
	home, err := i.codexHomeForLaunch(program, workDir)
	if err != nil || strings.TrimSpace(home) == "" {
		return ConversationCaptureSnapshot{}
	}
	return BeginConversationCaptureAtCodexHomeAndWorkingDir(home, LaunchCaptureWorkingDir(program, workDir))
}

// conversationResumableHere is ConversationResumableHere with the session's own
// store resolution: an account-scoped codex session's rollout lives in the
// account's registry dir, which the ambient command model cannot see.
func (i *Instance) conversationResumableHere(program, workingDir string, conv AgentConversationData) bool {
	account, _ := i.AccountSelection()
	if strings.TrimSpace(account) == "" || strings.TrimSpace(conv.Agent) != tmux.ProgramCodex {
		return ConversationResumableHere(program, workingDir, conv)
	}
	home, err := i.codexHomeForLaunch(program, workingDir)
	return err == nil && CodexRolloutPathByID(home, conv.ID) != ""
}

// ExactResume reports whether a restore of this session would resume its own
// conversation, by the check every restore makes (prepareExactResumeConversation
// on the program resolveLaunchProgramForInstance resolves), and when it would
// not, why: archived, the session would come back as a fresh agent.
//
// An off-box session (docker, ssh, hook) never qualifies: its archive reaps the
// sandbox, and the restore provisions a fresh one whose agent store does not hold
// the old transcript, so the agent-server starts a new conversation
// (launchWorkspace, ConversationResumableHere) whatever the command can express.
func (i *Instance) ExactResume() (bool, string) {
	if i.Capabilities().Workspace == WorkspaceRemote {
		return false, "this session runs in a sandbox, and the sandbox af restores it into does not keep its agent's conversation"
	}
	program := resolveLaunchProgramForInstance(i).command
	_, ok, refusal := prepareExactResumeConversation(i, program)
	if ok {
		return true, ""
	}
	if refusal != "" {
		return false, refusal
	}
	agent := tmux.DetectAgentFromCommand(program)
	if agent == "" {
		return false, "af does not know which agent this session runs"
	}
	if _, ok := tmux.ResumeProgramWithConversationID(agent, agent, "id"); !ok {
		return false, "af cannot resume a " + agent + " conversation by its id"
	}
	if !i.resumableConversation().HasID() {
		return false, "af recorded no conversation id for this " + agent + " session"
	}
	return false, "af cannot resume this " + agent + " session's recorded conversation with the command it would launch"
}

// ResumeConversationOutcome reports what a re-spawn of this session's runtime
// does to its conversation — the answer a retry-limit response owes (#6768):
// Carried when the launch resumes this session's own conversation, whether by
// the recorded id or the worktree-scoped resume form the exact-only rewrite
// keeps; otherwise the reason it starts a fresh conversation, the fact the
// respawn could previously only log.
func (i *Instance) ResumeConversationOutcome() HandoffConversation {
	if i.Capabilities().Workspace == WorkspaceRemote {
		return HandoffConversation{CarryFailure: "this session runs in a sandbox, and the sandbox af restores it into does not keep its agent's conversation"}
	}
	program := resolveLaunchProgramForInstance(i).command
	if _, ok, _ := prepareExactResumeConversation(i, program); ok {
		return HandoffConversation{Carried: true}
	}
	if tmux.ProgramResumesConversation(program) {
		return HandoffConversation{Carried: true}
	}
	if _, why := i.ExactResume(); why != "" {
		return HandoffConversation{CarryFailure: why}
	}
	return HandoffConversation{CarryFailure: "af cannot prove the launched program resumes this session's conversation"}
}
