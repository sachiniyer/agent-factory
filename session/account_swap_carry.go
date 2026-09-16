package session

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// conversationCarry is how a same-agent account swap keeps its conversation
// (#4367). Both claude and codex scope an account by relocating the provider's
// whole home, so the incoming account cannot see the outgoing transcript until
// af copies it across. Once copied, the replacement resumes that exact id.
//
// This is deliberately narrower than a handoff: the format is the same
// provider's, so the incoming process reads its own history. A cross-agent
// handoff still transfers mission + worktree and never a transcript
// (docs/design/agent-handoff.md D2).
type conversationCarry struct {
	agent string
	id    string
	// sourceAccount is the account whose home held the conversation; empty is
	// the ambient identity.
	sourceAccount string
	srcHome       string
	dstHome       string
	// transcript is the conversation file, relative to both homes. The layout
	// is kept identical so the provider finds it where it would have written it.
	transcript string
	// aux is Claude's per-session directory (tool results, subagent
	// transcripts), relative to both homes. Empty for codex.
	aux string
	// committed marks a carry re-planned from a pending record: the identity
	// checkpoint only records a carry whose pre-commit copy landed.
	committed bool
}

// HandoffConversation reports what a same-agent replacement can read of its
// predecessor's conversation. The zero value is today's cross-agent-style
// fresh start, where no carry applied at all.
type HandoffConversation struct {
	// Carried means the replacement resumes the outgoing conversation.
	Carried bool
	// CarryFailure is the clause explaining why a carry that applied fell back
	// to a fresh conversation.
	CarryFailure string
}

var providerConversationIDRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// carrySupportedAgent names the providers whose conversation is a local,
// account-independent file af has verified can be resumed under another
// account (#4367). Gemini records no conversation id af can address.
func carrySupportedAgent(agent string) bool {
	return agent == tmux.ProgramClaude || agent == tmux.ProgramCodex
}

type accountSwapCarryRequest struct {
	// agent is the provider the resolved replacement program runs.
	agent      string
	crossAgent bool
	// outgoing is the live tab's conversation at validation time.
	outgoing AgentConversationData
	// current is the account the outgoing runtime used; empty is ambient.
	current string
	target  string
	pending *AccountSwapData
	program string
	workDir string
	// fallback is a carry failure this attempt already hit.
	fallback string
}

// planAccountSwapCarry decides whether a swap carries its conversation. It
// returns the carry to perform, or the reason a carry that applies cannot
// happen; (nil, "") means no carry applies (a cross-agent handoff, or an agent
// without verified carry support), which keeps today's fresh start unchanged.
func planAccountSwapCarry(req accountSwapCarryRequest) (*conversationCarry, string) {
	if req.crossAgent || !carrySupportedAgent(req.agent) {
		return nil, ""
	}
	if req.fallback != "" {
		return nil, req.fallback
	}
	carry := &conversationCarry{agent: req.agent, id: strings.TrimSpace(req.outgoing.ID), sourceAccount: req.current}
	if pending := req.pending; pending != nil && pending.To == req.target {
		// A committed transaction already chose its conversation. Never start a
		// new carry after the checkpoint: the live slot no longer describes the
		// outgoing runtime, and the recorded mission already told its story.
		if pending.CarriedConversationID == "" {
			return nil, pending.CarryFallback
		}
		carry.id, carry.sourceAccount, carry.committed =
			pending.CarriedConversationID, pending.CarrySourceAccount, true
	} else if !req.outgoing.HasID() || req.outgoing.Agent != req.agent {
		return nil, fmt.Sprintf("af had no recorded %s conversation id for the previous session", req.agent)
	}
	if !providerConversationIDRE.MatchString(carry.id) {
		return nil, fmt.Sprintf("the recorded conversation id is not a %s session id", req.agent)
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return nil, "af could not locate its account registry"
	}
	target, err := agentaccount.Selected(home, req.agent, req.target)
	if err != nil || target.Dir == "" {
		// resolveAccountForProvision reports the real refusal for this.
		return nil, "the new account's home could not be resolved"
	}
	carry.dstHome = target.Dir
	srcHome, reason := carrySourceHome(home, req.agent, carry.sourceAccount, req.program, req.workDir)
	if reason != "" {
		return nil, reason
	}
	carry.srcHome = srcHome
	if reason := carry.locate(req.program, req.workDir); reason != "" {
		return nil, reason
	}
	return carry, ""
}

// carrySourceHome resolves the provider home the outgoing runtime used. A
// registered account is its registry directory; the ambient identity follows
// the same environment model the provider-store readers already use.
func carrySourceHome(home, agent, account, program, workDir string) (string, string) {
	if strings.TrimSpace(account) != "" {
		source, err := agentaccount.Selected(home, agent, account)
		if err != nil || source.Dir == "" {
			return "", fmt.Sprintf("the previous account %q is no longer registered for %s", account, agent)
		}
		return source.Dir, ""
	}
	switch agent {
	case tmux.ProgramClaude:
		dir, _, err := claudeTranscriptLaunchContext(program, workDir)
		if err != nil {
			return "", "af could not resolve the ambient Claude config directory"
		}
		return dir, ""
	case tmux.ProgramCodex:
		dir, err := tmux.CodexHomeFromCommand(program, workDir)
		if err != nil {
			return "", "af could not resolve the ambient Codex home"
		}
		return dir, ""
	}
	return "", fmt.Sprintf("%s conversations cannot be carried", agent)
}

// locate finds the conversation artifact and checks it exists before any
// runtime is stopped, so a missing transcript becomes a stated fresh start
// from the outset rather than a surprise at copy time. The copy still handles
// the artifact disappearing in between.
func (c *conversationCarry) locate(program, workDir string) string {
	switch c.agent {
	case tmux.ProgramClaude:
		return c.locateClaude(program, workDir)
	case tmux.ProgramCodex:
		return c.locateCodex()
	}
	return fmt.Sprintf("%s conversations cannot be carried", c.agent)
}

func (c *conversationCarry) locateClaude(program, workDir string) string {
	launch, err := tmux.CommandEnvironmentFromCommand(program, workDir)
	if err != nil || !launch.WorkingDirKnown() {
		return "af could not resolve the directory Claude files this conversation under"
	}
	// Claude names a project after the directory it was launched in. Try the
	// path as recorded and, where it differs, as the kernel reports it, since a
	// symlinked worktree parent yields a different project name.
	candidates := []string{launch.WorkingDir}
	if resolved, err := filepath.EvalSymlinks(launch.WorkingDir); err == nil && resolved != launch.WorkingDir {
		candidates = append(candidates, resolved)
	}
	for _, dir := range candidates {
		project := filepath.Join("projects", claudeProjectName(dir))
		transcript := filepath.Join(project, c.id+".jsonl")
		if carryArtifactPresent(c.srcHome, transcript) ||
			(c.committed && carryArtifactPresent(c.dstHome, transcript)) {
			c.transcript = transcript
			c.aux = filepath.Join(project, c.id)
			return ""
		}
	}
	return "its transcript is missing from the previous account's home"
}

func (c *conversationCarry) locateCodex() string {
	source, reason := codexRolloutRelPath(c.srcHome, c.id)
	if reason != "" {
		return reason
	}
	existing, reason := codexRolloutRelPath(c.dstHome, c.id)
	if reason != "" {
		return reason
	}
	switch {
	case source == "" && existing == "":
		return "its rollout is missing from the previous account's home"
	case source == "" && !c.committed:
		return "its rollout is missing from the previous account's home"
	case source == "":
		c.transcript = existing
	case existing != "" && existing != source:
		// Codex resolves a thread id by scanning its sessions tree; two rollouts
		// for one id would leave the resume ambiguous.
		return "the new account already files this conversation under a different rollout"
	default:
		c.transcript = source
	}
	return ""
}

// codexRolloutRelPath returns the one rollout for id beneath home, relative to
// home, or "" when there is none.
func codexRolloutRelPath(home, id string) (string, string) {
	var matches []string
	for path := range codexRolloutFiles(home) {
		if strings.EqualFold(codexConversationIDFromPath(path), id) {
			matches = append(matches, path)
		}
	}
	switch len(matches) {
	case 0:
		return "", ""
	case 1:
		rel, err := filepath.Rel(home, matches[0])
		if err != nil || !filepath.IsLocal(rel) {
			return "", "af could not place the Codex rollout inside its home"
		}
		return rel, ""
	default:
		return "", fmt.Sprintf("%d Codex rollouts record this conversation", len(matches))
	}
}

// launch rewrites the resolved program to resume the carried conversation.
// Account launch proof accepts it because the resume words are af's own
// trailing additions (GeneratedArgsBetween), exactly like an injected id.
func (c *conversationCarry) launch(resolvedProgram string) (string, AgentConversationData, bool) {
	rewritten, ok := tmux.ResumeProgramWithConversationID(resolvedProgram, c.agent, c.id)
	if !ok {
		return resolvedProgram, AgentConversationData{}, false
	}
	return rewritten, AgentConversationData{
		Agent:       c.agent,
		ID:          c.id,
		CapturedAt:  time.Now(),
		CaptureKind: ConversationCaptureCarried,
	}, true
}

// copy performs the carry. Only the transcript decides success.
func (c *conversationCarry) copy() error {
	if err := carryConversationFile(c.srcHome, c.dstHome, c.transcript, !c.committed); err != nil {
		return err
	}
	if c.aux == "" {
		return nil
	}
	if err := carryConversationTree(c.srcHome, c.dstHome, c.aux); err != nil {
		log.WarningLog.Printf("account swap carried %s conversation %s without all of its per-session files: %v",
			c.agent, c.id, err)
	}
	return nil
}

func (c *conversationCarry) clone() *conversationCarry {
	if c == nil {
		return nil
	}
	cloned := *c
	return &cloned
}

// CarryAccountSwapConversation copies a same-agent swap's outgoing
// conversation into the incoming account (#4367).
//
// The daemon calls it after the old runtime is conclusively stopped — the
// transcript is appended live and is only final then — and before the identity
// checkpoint, so the pending record only ever claims a conversation the new
// account already holds. A failed copy is not a failed swap: the launch plan is
// rebuilt as a fresh conversation whose notice says why. The error return is
// reserved for a fresh plan that cannot be built either. Without a plan there
// is nothing to carry; the replacement launch reports the missing plan itself.
func (i *Instance) CarryAccountSwapConversation() error {
	i.mu.RLock()
	plan := cloneAccountSwapLaunchPlan(i.accountSwapLaunch)
	i.mu.RUnlock()
	if plan == nil {
		return nil
	}
	_, err := i.carryOrReplan(plan)
	return err
}

// carryOrReplan performs plan's carry and, when the copy fails, replaces the
// recorded plan with a fresh one. It returns the fallback reason, if any.
func (i *Instance) carryOrReplan(plan *accountSwapLaunchPlan) (string, error) {
	if plan.carry == nil {
		return "", nil
	}
	copyErr := plan.carry.copy()
	if copyErr == nil {
		return "", nil
	}
	reason := carryFailureReason(copyErr)
	log.WarningLog.Printf("account swap for %q could not carry %s conversation %s into account %q, so the replacement starts a fresh conversation: %v",
		i.Title, plan.carry.agent, plan.carry.id, plan.account, copyErr)
	if err := i.validateAccountSwapPlan(plan.account, plan.agent, plan.manual, true, reason); err != nil {
		return reason, fmt.Errorf("account swap for %q could not carry its conversation (%s), and a fresh conversation could not be prepared either: %w",
			i.Title, reason, err)
	}
	return reason, nil
}

// ensureAccountSwapConversationCarried is the respawn-time half: it re-runs
// the idempotent copy for the plan about to launch (a daemon restart, or a
// session-level caller that never ran the pre-commit step), and demotes a
// committed carry whose copy can no longer be completed.
func (i *Instance) ensureAccountSwapConversationCarried(plan *accountSwapLaunchPlan) (*accountSwapLaunchPlan, error) {
	reason, err := i.carryOrReplan(plan)
	if err != nil || reason == "" {
		return plan, err
	}
	if err := i.demotePendingAccountSwapCarry(plan.account, reason); err != nil {
		return nil, err
	}
	return i.accountSwapLaunchForRespawn()
}

// demotePendingAccountSwapCarry rewrites a committed carry as the fresh
// conversation the rebuilt plan will actually launch, so the durable record
// never claims a carried id that did not start.
func (i *Instance) demotePendingAccountSwapCarry(account, reason string) error {
	i.mu.Lock()
	pending := i.pendingAccountSwap
	plan := i.accountSwapLaunch
	if pending == nil || pending.To != account || plan == nil || plan.account != account || plan.carry != nil {
		i.mu.Unlock()
		return fmt.Errorf("account swap for %q changed while its conversation carry fell back", i.Title)
	}
	pending.CarriedConversationID = ""
	pending.CarrySourceAccount = ""
	pending.CarryFallback = reason
	pending.ConversationID = ""
	if plan.conversation.HasID() {
		pending.ConversationID = plan.conversation.ID
	}
	manual, from := pending.Manual, pending.From
	i.touchLocked()
	i.mu.Unlock()
	if manual {
		i.refreshPendingManualAccountSwapMission(from, account)
	}
	return nil
}

// refreshPendingManualAccountSwapMission re-renders a committed manual
// mission after its conversation outcome changed. The stored mission was
// rendered for a carried conversation; delivering it now would tell a fresh
// agent that its history is intact.
func (i *Instance) refreshPendingManualAccountSwapMission(from, to string) {
	var reason string
	if handoff, ok := i.LastHandoff(); ok {
		reason = handoff.Reason
	}
	brief := i.BuildMissionBrief(i.CurrentAgentName(), "", reason)
	brief.Conversation = i.PendingAccountSwapConversation()
	mission := brief.Render()
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || pending.From != from || pending.To != to || pending.Mission == mission {
		return
	}
	pending.Mission = mission
	i.touchLocked()
}

// PreparedAccountSwapConversation reports the conversation outcome of the
// launch plan admission froze, for rendering a notice before the checkpoint.
func (i *Instance) PreparedAccountSwapConversation() HandoffConversation {
	i.mu.RLock()
	defer i.mu.RUnlock()
	plan := i.accountSwapLaunch
	if plan == nil {
		return HandoffConversation{}
	}
	return HandoffConversation{Carried: plan.carry != nil, CarryFailure: plan.carryFallback}
}

// PendingAccountSwapConversation reports the committed swap's conversation
// outcome, for the notice delivered to its replacement.
func (i *Instance) PendingAccountSwapConversation() HandoffConversation {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pending := i.pendingAccountSwap
	if pending == nil {
		return HandoffConversation{}
	}
	return HandoffConversation{Carried: pending.CarriedConversationID != "", CarryFailure: pending.CarryFallback}
}

// carriedCodexRolloutPath is where the capture snapshot must expect the
// carried rollout, so capture never mistakes it for a newly minted thread.
func (c *conversationCarry) carriedCodexRolloutPath() string {
	if c == nil || c.agent != tmux.ProgramCodex {
		return ""
	}
	return filepath.Join(c.dstHome, c.transcript)
}

// sameCarryHome reports whether the carry targets exactly the directory the
// account boundary resolved; the two derive from one registry function, so a
// mismatch is a refusal rather than a guess about which one is right.
func (c *conversationCarry) sameCarryHome(dir string) bool {
	if c == nil {
		return true
	}
	return filepath.Clean(c.dstHome) == filepath.Clean(dir)
}

// synchronizeCarriedConversationLocked restores a committed carry's
// conversation on the live slot after a daemon restart, where it would
// otherwise be missing: the checkpoint cleared the slot, and the launch that
// refilled it may not have reached disk. Claude resumes exactly the carried
// id. Codex capture may have followed a fork, so any Codex id it recorded
// stands. Callers hold i.mu.
func (i *Instance) synchronizeCarriedConversationLocked(agent, id string) error {
	if !carrySupportedAgent(agent) {
		return fmt.Errorf("account swap for %q carried a conversation but its replacement agent is %q", i.Title, agent)
	}
	if len(i.Tabs) == 0 {
		return fmt.Errorf("account swap for %q has no agent tab to receive its carried conversation", i.Title)
	}
	current := i.Tabs[0].Conversation
	if !current.HasID() {
		i.setAgentConversationLocked(AgentConversationData{
			Agent:       agent,
			ID:          id,
			CapturedAt:  time.Now(),
			CaptureKind: ConversationCaptureCarried,
		})
		return nil
	}
	if current.Agent != agent || (agent == tmux.ProgramClaude && current.ID != id) {
		return fmt.Errorf("account swap for %q carried %s conversation %q but the live agent records %s conversation %q",
			i.Title, agent, id, current.Agent, current.ID)
	}
	return nil
}
