package session

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	ConversationCaptureInjected         = "injected"
	ConversationCaptureCodexRollout     = "codex_rollout"
	ConversationCaptureClaudeTranscript = "claude_transcript"
	// ConversationCaptureCarried marks a conversation a same-agent account
	// swap copied into the incoming account's home and resumed (#4367).
	ConversationCaptureCarried = "carried"
	// ConversationCaptureOperator marks an id an operator recorded with
	// `af sessions set-conversation` (#6769). It is persisted so the repair
	// keeps its standing across a daemon restart: the follow check leaves it
	// until the runtime moves to another thread (#7120 review), and the
	// restore and carry staleness guards disregard only the threads it
	// overrode — never one whose rollout appeared after it, or one the pane
	// moved to since (#7122, operatorRepairWindow.disregards).
	ConversationCaptureOperator = "operator"
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
	// RepairStamped and RepairHeldID belong to an operator record only:
	// whether af has observed what the runtime held open when the repair was
	// made, and that thread's id ("" = it held none). The follow check leaves
	// the repair standing until the runtime holds something else, and the
	// staleness guards exempt that thread by id. They are persisted with the
	// record so a daemon restart keeps the observation instead of mistaking a
	// thread the pane moved to while the daemon was down for the one it held
	// at the repair (#7122). A write that replaces the record — a follow, a
	// capture, a carry — drops them; a handoff ledger entry keeps the whole
	// outgoing record, stamp included, for a revert to restore as it was.
	RepairStamped bool   `json:"repair_stamped,omitempty"`
	RepairHeldID  string `json:"repair_held_id,omitempty"`
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
		strings.TrimSpace(c.CaptureKind) == "" &&
		!c.RepairStamped &&
		strings.TrimSpace(c.RepairHeldID) == ""
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
	return i.runtimeTokenLiveLocked(token) && conv.Agent == token.agent
}

// runtimeTokenLiveLocked reports whether token still names the live process
// generation: the instance has not been killed and its runtime has neither
// been replaced nor re-resolved since the token was taken. Callers hold i.mu.
func (i *Instance) runtimeTokenLiveLocked(token AgentRuntimeToken) bool {
	return !i.userKilled && token.agent != "" && token.generation == i.agentRuntimeGeneration &&
		i.resolvedAgentLocked() == token.agent
}

// noteAgentRuntimeReplaced invalidates every capture bound to the prior process.
// Handoff record mutation calls the locked increment before teardown; local
// recovery calls this after re-spawn so both replacement paths close the edge.
func (i *Instance) noteAgentRuntimeReplaced() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.agentRuntimeGeneration++
	i.retireLimitWallShadowOnRuntimeReplaceLocked()
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
	if conv.HasID() {
		// A recorded conversation discharges the parked baseline's whole
		// purpose: the arm it waited for no longer needs its before-image
		// (#6946).
		i.pendingConversationCapture = nil
	}
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
	// A relaunch verdict describes a respawn under the outgoing account's
	// provider store; it must not survive the crossing either (#6868).
	i.respawnReport = nil
	// A parked capture baseline binds that same store — across the boundary
	// it could only arm a watch on a home the replacement never writes to
	// (#6946).
	i.pendingConversationCapture = nil
}

// PendingConversationCapture is a capture baseline parked by a respawn that
// retired a refused conversation id (#6769): the replacement's rollout can
// already exist by the time the daemon's deferred arm would build a fresh
// before-image, so the pre-spawn snapshot rides on the instance until the
// replacement's own arm takes it. generation stamps the runtime generation
// the park was installed under — the refused resume's OUTGOING one — and the
// park releases only once the runtime has moved past it: an arm still bound
// to that generation belongs to the outgoing process, would commit that
// process's discoveries, and then die on the generation gate — spending the
// only before-image that names the replacement's already-minted rollout on a
// recording that can never land (#6946).
type PendingConversationCapture struct {
	Snapshot   ConversationCaptureSnapshot
	generation uint64
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
		i.pendingConversationCapture = &PendingConversationCapture{
			Snapshot: s,
			// The park is installed while the runtime replacement that will
			// consume it is still in flight — the respawn bumps
			// agentRuntimeGeneration only at the end of its body — so the
			// stamp is the OUTGOING generation: the arm that may spend this
			// baseline is the next one's, never this one's (#6946).
			generation: i.agentRuntimeGeneration,
		}
	}
}

// TakePendingConversationCaptureForRuntime hands a respawn-parked capture
// baseline to the deferred arm that will spend it — and only to an arm that
// still can. current=false means the caller's token names a runtime
// generation the instance has already left: nothing was consumed and the
// park stays for the arm the replacement sends. Even a live token leaves the
// park in place while its generation has not moved past the one the baseline
// predates (#6946): the respawn parks BEFORE bumping the runtime generation,
// so an arm admitted inside that window still belongs to the outgoing
// process — its commits would fail the generation gate, dropping the only
// before-image that names the replacement's already-minted rollout.
//
// The token check and the take share this one lock hold, so a generation
// bump cannot land between them and let a stale arm consume the park.
func (i *Instance) TakePendingConversationCaptureForRuntime(token AgentRuntimeToken) (*PendingConversationCapture, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.runtimeTokenLiveLocked(token) {
		return nil, false
	}
	pending := i.pendingConversationCapture
	if pending == nil || i.agentRuntimeGeneration <= pending.generation {
		return nil, true
	}
	i.pendingConversationCapture = nil
	return pending, true
}

// RestorePendingConversationCapture puts back a baseline an admitted arm
// consumed but could not keep — the launch the manager refused while closing
// down for shutdown is the only post-take decline left. A park installed by
// a newer respawn meanwhile wins: it is fresher evidence, stamped with a
// later generation.
func (i *Instance) RestorePendingConversationCapture(park *PendingConversationCapture) {
	if park == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingConversationCapture == nil {
		i.pendingConversationCapture = park
	}
}

// DropPendingConversationCapture clears a parked baseline that can no longer
// serve any arm — the row recorded a conversation, was killed, or was
// archived while it waited (#6946). These clears are explicit: the park's
// only consumer is an arm that actually proceeds, so the paths that make
// consumption permanently moot name themselves rather than letting a decline
// drop it incidentally.
func (i *Instance) DropPendingConversationCapture() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pendingConversationCapture = nil
}

// DropPendingConversationCaptureIfMoot clears the parked baseline exactly when
// the row can never arm again — a recorded conversation, a kill, or an
// archive — with the condition read under the same i.mu hold that clears, so
// a respawn parking its baseline between the read and the drop cannot have it
// destroyed underneath it (#6946 review). Reports whether the row is moot so
// the caller can still skip it.
func (i *Instance) DropPendingConversationCaptureIfMoot() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	moot := i.userKilled || i.liveness == LiveArchived
	if !moot {
		if len(i.Tabs) > 0 {
			moot = i.Tabs[0].Conversation.HasID()
		} else {
			moot = i.reportedConversation.HasID()
		}
	}
	if moot {
		i.pendingConversationCapture = nil
	}
	return moot
}

// PendingConversationCaptureParkedForTest reports whether a parked baseline
// still waits for its deferred arm.
func (i *Instance) PendingConversationCaptureParkedForTest() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.pendingConversationCapture != nil
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
// Codex resume comes back with its typed reason; every other non-resume case
// (nothing recorded, the command cannot express a resume) is the zero decline
// because there is nothing to warn about.
func prepareExactResumeConversation(i *Instance, program string) (string, bool, resumeDecline) {
	conv, carried := i.resumableConversationAndSource()
	if !conv.HasID() {
		return program, false, resumeDecline{}
	}
	rewritten, ok := tmux.ResumeProgramWithConversationID(program, conv.Agent, conv.ID)
	if !ok {
		return program, false, resumeDecline{}
	}
	if !carried && conv.Agent == tmux.ProgramCodex {
		if decline := i.staleRecordedCodexDecline(program, conv); decline.reason != "" {
			return program, false, decline
		}
	}
	return rewritten, true, resumeDecline{}
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

// staleRecordedCodexDecline answers whether the recorded Codex conversation
// still names the pane's conversation: the zero decline when af can prove it
// does or when there is nothing newer to compare, a refusal when the rollout
// is missing, a newer same-worktree top-level thread exists, or the newer set
// cannot be correlated either way. Fail closed: "we cannot prove current"
// and "provably stale" both refuse, because silently resuming the wrong
// thread is the bug this guard exists to kill. The two are typed apart
// (resumeDecline.unverified), which decides whether an operator's repair is
// retired.
//
// An operator's repair (conv's kind is ConversationCaptureOperator) is
// judged the same way with one difference, and only after its provenance is
// re-checked (#7122): a newer thread the operator chose over never makes it
// stale (operatorRepairWindow.disregards). Any other — a thread whose rollout
// appeared after the repair, or one the pane moved to since — does, whether or
// not the follow check saw the pane move: the rule reads only the record and
// the store, so it holds across a daemon restart or a blind follow alike.
func (i *Instance) staleRecordedCodexDecline(program string, conv AgentConversationData) resumeDecline {
	workDir := i.GetWorktreePath()
	home, err := i.codexHomeForLaunch(program, workDir)
	if err != nil || strings.TrimSpace(home) == "" {
		return declineResumeUnverified("af cannot resolve the Codex home this session writes to")
	}
	files, err := codexRolloutFiles(home)
	if err != nil {
		return declineResumeUnverified("af could not verify the recorded conversation against the Codex home this session uses")
	}
	var recordedPath string
	for file := range files {
		if strings.EqualFold(codexConversationIDFromPath(file), conv.ID) {
			if recordedPath != "" {
				// Two rollouts claim the same id: "current" cannot be proven,
				// now or later.
				return declineResume("af could not confirm the recorded conversation is still the newest one in this worktree: two rollouts record it")
			}
			recordedPath = file
		}
	}
	if recordedPath == "" {
		return declineResume("the recorded conversation's rollout is missing from the Codex home this session uses")
	}
	launchDir := LaunchCaptureWorkingDir(program, workDir)
	var repair operatorRepairWindow
	if conv.CaptureKind == ConversationCaptureOperator {
		if decline := operatorRepairProvenance(recordedPath, launchDir); decline.reason != "" {
			return decline
		}
		repair = operatorRepairWindowOf(conv, time.Now())
	}
	switch checkCodexRecordedStaleness(recordedPath, files, pathutil.ResolveForCompare(launchDir), repair) {
	case codexRolloutStale:
		return declineResume(staleCarryReason)
	case codexRolloutUncorrelated:
		// A newer rollout whose header af cannot classify yet — Codex may be
		// mid-write — is a look that failed, not evidence about the id.
		return declineResumeUnverified("af could not confirm the recorded conversation is still the newest one in this worktree")
	case codexRolloutRecordedGone:
		return declineResume("the recorded conversation's rollout is missing from the Codex home this session uses")
	}
	return resumeDecline{}
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
//
// repair is an operator repair's window (operatorRepairWindowOf): a newer
// file it disregards is a thread the operator chose over and is dropped; any
// other, including one af cannot date, stays a candidate exactly as it would
// for any record. The zero window drops nothing.
func checkCodexRecordedStaleness(recordedPath string, files map[string]struct{}, launchDir string, repair operatorRepairWindow) codexStalenessVerdict {
	recorded, err := os.Lstat(recordedPath)
	if err != nil {
		return codexRolloutRecordedGone
	}
	var newer []string
	for path := range files {
		if path == recordedPath {
			continue
		}
		if info, serr := os.Lstat(path); serr == nil && info.ModTime().After(recorded.ModTime()) &&
			!repair.disregards(path, info.ModTime()) {
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

// SessionCodexHomeDir resolves the Codex account home this session's live
// runtime actually writes against — the account dir for a named selection,
// or the ambient home derived from the launch command's CODEX_HOME. It is
// bound to the command that launched the runtime (RuntimeProgram evidence,
// with the launch resolution's fallback for rows that predate it), so a
// program_overrides edit cannot redirect the read to a different home
// (#6817 review). "" when the session isn't a local codex runtime or its
// home cannot be resolved.
func (i *Instance) SessionCodexHomeDir() string {
	if i.Capabilities().Workspace != WorkspaceLocalWorktree {
		return ""
	}
	// The provider is what the launch command resolves to, not the
	// configured enum — resolvedAgentLocked is the #6811 resolver: a
	// session whose program_overrides entry launches codex under another
	// agent name still writes a codex home (#6817 review).
	if i.ResolvedAgent() != tmux.ProgramCodex {
		return ""
	}
	program := i.RuntimeProgram()
	if strings.TrimSpace(program) == "" {
		// A project config that cannot be read leaves the home unknown, which is
		// this method's "" — never a home derived from the global config (#7077).
		resolution, err := resolveLaunchProgramForInstance(i)
		if err != nil {
			return ""
		}
		program = resolution.command
	}
	if tmux.DetectAgentFromCommand(program) != tmux.ProgramCodex {
		return ""
	}
	home, err := i.codexHomeForLaunch(program, i.GetWorktreePath())
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return home
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
		// An unreadable project config is "cannot baseline", the empty
		// snapshot every other unresolvable store answers — never a store
		// bound to the global config's command (#7077).
		resolution, err := resolveLaunchProgramForInstance(i)
		if err != nil {
			log.WarningLog.Printf("session %q: no conversation capture baseline: %v", i.Title, err)
			return ConversationCaptureSnapshot{}
		}
		program = resolution.command
	}
	return i.sessionCodexCaptureBaselineFor(program)
}

// RetakeSessionCodexCaptureBaseline: compile-only stub for the failing-first
// commit.
func (i *Instance) RetakeSessionCodexCaptureBaseline() (ConversationCaptureSnapshot, error) {
	return i.SessionCodexCaptureBaseline(), nil
}

// sessionCodexCaptureBaselineFor builds the snapshot against an explicit
// launch command. Only a local workspace can answer it: a remote pane writes
// its provider store inside its own sandbox, the daemon's account registry is
// a DIFFERENT store, and a remote row's empty worktree would leave rollout
// correlation unscoped — so a stray local rollout could be recorded as the
// remote session's identity. Remote identities arrive through the
// agent-server reporting path in conversation_remote.go (#6769 review).
func (i *Instance) sessionCodexCaptureBaselineFor(program string) ConversationCaptureSnapshot {
	home, workingDir, ok := i.sessionCodexCaptureStoreFor(program)
	if !ok {
		return ConversationCaptureSnapshot{}
	}
	return BeginConversationCaptureAtCodexHomeAndWorkingDir(home, workingDir)
}

// SessionCodexCaptureScope binds the same store and launch directory as
// SessionCodexCaptureBaseline but walks nothing: no before-image is taken. A
// recorded row's loop only runs the follow check, which asks the runtime what
// it holds and never reads a baseline, so arming it must not walk the store
// on the poll goroutine — ~15 walks of one shared store after every restart,
// a stalled poll on a hung mount — nor let a failing walk end the loop
// (#7120 review).
func (i *Instance) SessionCodexCaptureScope() ConversationCaptureSnapshot {
	program := i.RuntimeProgram()
	if strings.TrimSpace(program) == "" {
		resolution, err := resolveLaunchProgramForInstance(i)
		if err != nil {
			log.WarningLog.Printf("session %q: no conversation capture scope: %v", i.Title, err)
			return ConversationCaptureSnapshot{}
		}
		program = resolution.command
	}
	home, workingDir, ok := i.sessionCodexCaptureStoreFor(program)
	if !ok {
		return ConversationCaptureSnapshot{}
	}
	if workingDir != "" {
		workingDir = pathutil.ResolveForCompare(workingDir)
	}
	return ConversationCaptureSnapshot{
		startedAt:    time.Now(),
		codexHome:    home,
		workingDir:   workingDir,
		receiptState: newCodexReceiptState(),
		scopeOnly:    true,
	}
}

// sessionCodexCaptureStoreFor resolves the Codex store and launch directory a
// capture for this session binds to, or ok=false when there is none.
func (i *Instance) sessionCodexCaptureStoreFor(program string) (home, workingDir string, ok bool) {
	if i.Capabilities().Workspace != WorkspaceLocalWorktree {
		return "", "", false
	}
	workDir := i.GetWorktreePath()
	// The provider is what the launch command resolves to, not the
	// configured enum — resolvedAgentLocked is the #6811 resolver: a
	// session whose program_overrides entry launches codex under another
	// agent name still writes a codex home and must baseline its store
	// (#6817 review, #6769 recording guarantee). The first conjunct already
	// refuses every command that does not resolve to codex, so the enum
	// conjunct only ever rejected this override case — matching
	// SessionCodexHomeDir and the create-time capture gate above.
	if tmux.DetectAgentFromCommand(program) != tmux.ProgramCodex || i.ResolvedAgent() != tmux.ProgramCodex {
		return "", "", false
	}
	home, err := i.codexHomeForLaunch(program, workDir)
	if err != nil || strings.TrimSpace(home) == "" {
		return "", "", false
	}
	return home, LaunchCaptureWorkingDir(program, workDir), true
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
	return i.exactResumeVerdict()
}

// exactResumeVerdict is the sandbox-agnostic verdict a local (non-remote)
// respawn of this session would reach: whether it resumes its own recorded
// conversation, and when not, why. ExactResume reaches it after its blanket
// refusal for a remote-workspace (sandbox) restore, and handoffConversationForRespawn
// shares it for a local session's respawn. A remote-workspace session's respawn
// is not judged here: ResumeConversationOutcome answers it separately with an
// honest non-carry, because the driving daemon cannot prove a sandbox carry.
func (i *Instance) exactResumeVerdict() (bool, string) {
	resolution, err := resolveLaunchProgramForInstance(i)
	if err != nil {
		// The restore would refuse on the same read, so it resumes nothing;
		// the reason is the actionable one the restore will give (#7077).
		return false, err.Error()
	}
	program := resolution.command
	_, ok, decline := prepareExactResumeConversation(i, program)
	if ok {
		return true, ""
	}
	if decline.reason != "" {
		return false, decline.reason
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

// sandboxResumeUnprovenReason is the honest not-carried a sandbox session
// answered before its relaunch could report its own outcome — and still
// answers whenever a respawn left no report to read: a relaunch call that
// never ran, an older agent-server build that answers without one, or a
// report that could not be made out (#6868).
const sandboxResumeUnprovenReason = "this session runs in a sandbox, and af cannot prove from the driving daemon that its respawn carried the conversation"

// sandboxReprovisionedReport is the outcome the driving daemon records itself
// when a respawn's branch was a re-provision: no in-sandbox relaunch ran, so
// no relaunch report exists and the answer is a truthful not-carried (#6868).
// noteRespawnConversationReport stamps it with the session's title.
var sandboxReprovisionedReport = RelaunchConversationReport{
	CarryFailure: "the respawn re-provisioned this session's sandbox — the in-place relaunch that could resume its conversation never ran",
}

// ResumeConversationOutcome reports what a re-spawn of this session's runtime
// does to its conversation — the answer a retry-limit response owes (#6768):
// Carried when the launch resumes this session's own conversation, whether by
// the recorded id or the worktree-scoped resume form the exact-only rewrite
// keeps; otherwise the reason it starts a fresh conversation, the fact the
// respawn could previously only log.
//
// A sandbox (remote-workspace) session's respawn goes through
// relaunchOrRecoverSandbox (session/relaunch.go): an in-place relaunch when the
// sandbox supports it (CanRelaunchInPlace), re-provisioning only when it does
// not. The relaunch-vs-re-provision decision is made inside that call, so the
// answer here is whatever the branch that ACTUALLY ran reported (#6868): an
// in-place relaunch carries the sandbox's own resume verdict back over the
// wire, a re-provision records its own not-carried, and a respawn that left
// nothing to read — a relaunch that never ran, or an older sandbox build that
// sends no report — answers exactly as it did before the report existed. The
// caller reads it AFTER the respawn returns; asked before, a sandbox session
// reports the no-report answer.
func (i *Instance) ResumeConversationOutcome() HandoffConversation {
	if i.Capabilities().Workspace == WorkspaceRemote {
		return i.sandboxRespawnConversationOutcome()
	}
	return i.handoffConversationForRespawn()
}

// sandboxRespawnConversationOutcome translates the relaunch's own report —
// recorded by remoteAgentServer.Relaunch or by decideRecovery when the branch
// was a re-provision — into the carried/not-carried answer a sandbox session's
// retry-limit response owes (#6868). Only a report that binds to this session
// can carry it: a report for a different session or a different conversation,
// a report that names nothing, and a report that itself answers fresh all come
// back not-carried with the reason that is true for the respawn that ran.
func (i *Instance) sandboxRespawnConversationOutcome() HandoffConversation {
	report := i.respawnConversationReport()
	if report == nil {
		return HandoffConversation{CarryFailure: sandboxResumeUnprovenReason}
	}
	switch {
	case report.Session == "":
		return HandoffConversation{CarryFailure: "the sandbox's relaunch report did not name the session it describes"}
	case report.Session != i.Title:
		return HandoffConversation{CarryFailure: "the sandbox's relaunch report describes a different session"}
	}
	if !report.Carried {
		if report.CarryFailure != "" {
			return HandoffConversation{CarryFailure: report.CarryFailure}
		}
		return HandoffConversation{CarryFailure: sandboxResumeUnprovenReason}
	}
	if resumed, recorded := report.Conversation, i.resumableConversation(); resumed.HasID() && recorded.HasID() &&
		(resumed.Agent != recorded.Agent || resumed.ID != recorded.ID) {
		return HandoffConversation{CarryFailure: "the sandbox's relaunch resumed a different conversation than this session's recorded one"}
	}
	return HandoffConversation{Carried: true}
}

// noteRelaunchConversationReport records the resume verdict an in-sandbox
// relaunch just reported over the wire, bound to the remote runtime that
// produced it — the same staleness fence recordReportedConversationFrom uses,
// so a retired runtime's late answer cannot be attributed to its replacement
// (#6868). A nil report — an agent-server build that predates the field —
// simply leaves nothing recorded.
func (i *Instance) noteRelaunchConversationReport(rc *remoteAgentClient, report *RelaunchConversationReport) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if rc == nil || i.remoteClient != rc {
		return
	}
	i.respawnReport = report
}

// noteRespawnConversationReport records a respawn outcome the driving daemon
// settled itself — a re-provision, which has no in-sandbox relaunch to report
// for it (#6868). The record is stamped with this session's title so the read
// side's session binding treats it exactly like a wire report.
func (i *Instance) noteRespawnConversationReport(report RelaunchConversationReport) {
	i.mu.Lock()
	defer i.mu.Unlock()
	report.Session = i.Title
	i.respawnReport = &report
}

// clearRespawnConversationReport drops the verdict an earlier respawn left;
// decideRecovery calls it as a new respawn picks its branch, so a stale report
// can never describe the branch this one takes (#6868).
func (i *Instance) clearRespawnConversationReport() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.respawnReport = nil
}

// respawnConversationReport returns the verdict the last respawn recorded.
func (i *Instance) respawnConversationReport() *RelaunchConversationReport {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.respawnReport
}

// handoffConversationForRespawn is the determination a local (non-remote)
// respawn of this session's runtime makes about its conversation — the answer a
// local session's Recover owes. It is sandbox-agnostic: a remote-workspace
// (sandbox) session never reaches it, because ResumeConversationOutcome answers
// a sandbox respawn separately above, so ExactResume's blanket sandbox refusal
// is not used here.
func (i *Instance) handoffConversationForRespawn() HandoffConversation {
	outcome, _ := i.respawnConversationPlan()
	return outcome
}

// respawnConversationPlan is handoffConversationForRespawn plus the
// conversation the respawn resumes when it resumes one by id (#6868): an
// in-sandbox relaunch reports both back over the wire, so the driving daemon
// can bind a carried answer to THIS session's recorded conversation. The
// resumed id is empty for a worktree-scoped resume — the command picks its
// conversation at launch — and for a fresh start.
func (i *Instance) respawnConversationPlan() (HandoffConversation, AgentConversationData) {
	resolution, err := resolveLaunchProgramForInstance(i)
	if err != nil {
		// No carry can be promised for a launch whose command is unknown; the
		// respawn itself refuses on the same read (#7077).
		return HandoffConversation{CarryFailure: err.Error()}, AgentConversationData{}
	}
	program := resolution.command
	if _, ok, _ := prepareExactResumeConversation(i, program); ok {
		return HandoffConversation{Carried: true}, i.resumableConversation()
	}
	if tmux.ProgramResumesConversation(program) {
		return HandoffConversation{Carried: true}, AgentConversationData{}
	}
	if ok, why := i.exactResumeVerdict(); !ok {
		return HandoffConversation{CarryFailure: why}, AgentConversationData{}
	}
	return HandoffConversation{CarryFailure: "af cannot prove the launched program resumes this session's conversation"}, AgentConversationData{}
}
