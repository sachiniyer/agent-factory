package session

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
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
	// srcRoot and dstRoot anchor the two homes for the copy itself.
	srcRoot carryRoot
	dstRoot carryRoot
	// transcript is the conversation file, relative to both homes. The layout
	// is kept identical so the provider finds it where it would have written it.
	transcript string
	// aux is Claude's per-session directory (tool results, subagent
	// transcripts), relative to both homes. Empty for codex.
	aux string
	// committed marks a carry re-planned from a pending record: the identity
	// checkpoint only records a carry whose pre-commit copy landed.
	committed bool
	// srcFiles is the source store's plain rollouts as locateCodex enumerated
	// them, reused by the staleness check of the same plan rather than walked
	// again under the swap's fences (#7152). The plan clears it before it
	// returns, so a recorded plan never holds the listing.
	srcFiles map[string]struct{}
}

// HandoffConversation reports what the agent serving a session after a
// replacement can read of its predecessor's conversation: the carry a
// same-agent account swap committed, the reason a carry that applied could
// not complete, or the zero value — a cross-agent-style fresh start where no
// carry applied at all. The control-socket and HTTP handoff/retry responses
// carry it additively so a caller can tell whether the conversation carried
// and, when it did not, why (#6768).
type HandoffConversation struct {
	// Carried means the replacement resumes the outgoing conversation.
	Carried bool `json:"carried"`
	// CarryFailure is why the operation starts a fresh conversation — the
	// carry that could not complete, or the resume af could not name. Empty
	// when no carry applied.
	CarryFailure string `json:"carry_failure,omitempty"`
}

var providerConversationIDRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// carrySupportedAgent names the providers whose conversation is a local,
// account-independent file af has verified can be resumed under another
// account (#4367). Gemini records no conversation id af can address.
func carrySupportedAgent(agent string) bool {
	return agent == tmux.ProgramClaude || agent == tmux.ProgramCodex
}

// AccountSwapCarryRecorded is the in-memory precondition of a carry: a
// carry-supporting provider with a recorded conversation that belongs to it.
// The provider is what the launch command resolves to (resolvedAgentLocked,
// the #6811 resolver), not the configured enum, so a program-override
// session is judged by the runtime it actually runs. A local claude/codex
// session with no recorded conversation — or one recorded under a different
// provider — reads false, matching admission's "no recorded conversation id"
// refusal (#6817 review). It reads no file, so it is safe under a manager
// lock; whether the carry would actually happen is AccountSwapCarryPossible.
func (i *Instance) AccountSwapCarryRecorded() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	agent := i.resolvedAgentLocked()
	if !carrySupportedAgent(agent) {
		return false
	}
	conv := i.reportedConversation
	if len(i.Tabs) > 0 {
		conv = i.Tabs[0].Conversation
	}
	return conv.HasID() && conv.Agent == agent
}

// AccountSwapCarryIntended reports whether a same-agent account swap would
// ATTEMPT to carry its conversation, judging only the preconditions the
// confirming user can know before the swap runs (#4367, #4504). It is a
// NECESSARY, not sufficient, signal: the daemon re-checks filesystem state
// (transcript presence, staleness, account homes, copy success) at admission,
// after the user confirms — so a true return means "the carry is on", not "it
// will succeed". A false return means no carry is even attempted, so the
// replacement starts fresh.
//
// current and recorded describe the outgoing runtime (CurrentAgentName,
// AgentProgram); target is the requested enum; targetResolved is the agent
// that enum's resolved command launches (the daemon's resolved_agents answer,
// falling back to the enum when the daemon did not classify one); outgoing is
// the live tab's recorded conversation. The same-agent judgment goes through
// HandoffTargetIsCurrent, not an enum compare, for the same reason the picker
// uses it: program_overrides can make a resolved identity differ from the enum
// (#4430 review), so a naive target == current misclassifies a redirected
// self-handoff. An opaque target (targetResolved == "") is never
// carry-intended: the daemon cannot prove it launches a carry-supporting
// provider, so the replacement starts fresh.
func AccountSwapCarryIntended(current, target, targetResolved, recorded string, outgoing AgentConversationData) bool {
	if !HandoffTargetIsCurrent(current, target, targetResolved, recorded) {
		return false
	}
	if !carrySupportedAgent(targetResolved) {
		return false
	}
	return outgoing.HasID() && outgoing.Agent == targetResolved
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
	// sourceOnly plans without a target account: every check that needs only
	// the outgoing side runs, and target is ignored. AccountSwapCarryPossible
	// asks it before any target is chosen (#7152).
	sourceOnly bool
}

// planAccountSwapCarry decides whether a swap carries its conversation. It
// returns the carry to perform, or the reason a carry that applies cannot
// happen; (nil, "") means no carry applies (a cross-agent handoff, or an agent
// without verified carry support), which keeps today's fresh start unchanged.
//
// It is also require_carry's answer before a target exists
// (AccountSwapCarryPossible, sourceOnly): a refusal that needs only the
// outgoing side belongs outside the target block, so both callers see it.
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
	if !req.sourceOnly {
		target, err := agentaccount.Selected(home, req.agent, req.target)
		if err != nil || target.Dir == "" {
			// resolveAccountForProvision reports the real refusal for this.
			return nil, "the new account's home could not be resolved"
		}
		carry.dstHome = target.Dir
		if carry.dstRoot, err = accountCarryRoot(home, req.agent, req.target, target.Dir); err != nil {
			return nil, "the new account's home is not where af registers accounts"
		}
	}
	srcRoot, sourceReason := carrySourceHome(home, req.agent, carry.sourceAccount, req.program, req.workDir)
	if sourceReason != "" && !carry.committed {
		return nil, sourceReason
	}
	// A committed carry's copy already landed in the new account, so a source
	// home that is gone since — an unregistered account — only means there is
	// nothing newer to bring; the zero root reads as a missing source.
	carry.srcHome = srcRoot.dir()
	carry.srcRoot = srcRoot
	if reason := carry.locate(req.program, req.workDir); reason != "" {
		if sourceReason != "" {
			return nil, sourceReason
		}
		return nil, reason
	}
	// An operator's standing repair is judged by the restore guard's rule
	// (staleRecordedCodexDecline): its provenance is re-checked, and only the
	// threads it overrode are disregarded (#7122).
	if !carry.committed {
		if reason := carry.staleReason(req.program, req.workDir, req.outgoing); reason != "" {
			return nil, reason
		}
	}
	carry.srcFiles = nil
	return carry, ""
}

// carrySourceHome resolves the provider home the outgoing runtime used. A
// registered account is its registry directory; the ambient identity follows
// the same environment model the provider-store readers already use.
func carrySourceHome(home, agent, account, program, workDir string) (carryRoot, string) {
	if strings.TrimSpace(account) != "" {
		source, err := agentaccount.Selected(home, agent, account)
		if err != nil || source.Dir == "" {
			return carryRoot{}, fmt.Sprintf("the previous account %q is no longer registered for %s", account, agent)
		}
		root, err := accountCarryRoot(home, agent, account, source.Dir)
		if err != nil {
			return carryRoot{}, "the previous account's home is not where af registers accounts"
		}
		return root, ""
	}
	switch agent {
	case tmux.ProgramClaude:
		dir, _, err := claudeTranscriptLaunchContext(program, workDir)
		if err != nil {
			return carryRoot{}, "af could not resolve the ambient Claude config directory"
		}
		return ambientCarryRoot(dir), ""
	case tmux.ProgramCodex:
		dir, err := tmux.CodexHomeFromCommand(program, workDir)
		if err != nil {
			return carryRoot{}, "af could not resolve the ambient Codex home"
		}
		return ambientCarryRoot(dir), ""
	}
	return carryRoot{}, fmt.Sprintf("%s conversations cannot be carried", agent)
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
	var refused string
	for _, project := range claudeProjectCandidates(launch.WorkingDir) {
		transcript := filepath.Join(project, c.id+".jsonl")
		roots := []carryRoot{c.srcRoot}
		if c.committed {
			roots = append(roots, c.dstRoot)
		}
		for _, root := range roots {
			present, err := probeCarryArtifact(root, transcript)
			if present {
				c.transcript = transcript
				c.aux = filepath.Join(project, c.id)
				return ""
			}
			if path := carrySymlinkPath(err); path != "" && refused == "" {
				refused = symlinkCarryReason(path)
			}
		}
	}
	if refused != "" {
		return refused
	}
	return "its transcript is missing from the previous account's home"
}

func (c *conversationCarry) locateCodex() string {
	srcFiles, err := codexRolloutFiles(c.srcHome)
	if err != nil {
		return "af could not enumerate the Codex rollout store"
	}
	c.srcFiles = srcFiles
	source, reason := codexRolloutRelPathIn(c.srcHome, srcFiles, c.id)
	if reason != "" {
		return reason
	}
	existing, reason := codexRolloutRelPath(c.dstHome, c.id)
	if reason != "" {
		return reason
	}
	switch {
	case source == "" && (existing == "" || !c.committed):
		// The sessions tree is walked without following links, so a symlinked
		// store reads as empty: name the link rather than a missing rollout.
		for _, root := range []carryRoot{c.srcRoot, c.dstRoot} {
			if path := carrySymlinkPath(probeCarryDir(root, "sessions")); path != "" {
				return symlinkCarryReason(path)
			}
		}
		if reason := codexCarryOtherFormsReason(c.srcHome, c.id); reason != "" {
			return reason
		}
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
	// A plain rollout is not the thread's current one after a backtrack (#7141).
	return c.codexRevertReason()
}

// staleCarryReason is what a replacement is told when the recorded
// conversation is not the one the user was last in.
const staleCarryReason = "the conversation af recorded is no longer the newest one in this worktree " +
	"(a new one was started, for example with /clear or /new, or an older one was resumed)"

// staleReason refuses a carry whose recorded conversation is no longer the
// newest in this worktree. The recorded id is written at launch and at
// delivery capture only, so a /clear in Claude or /new in Codex leaves it
// naming a conversation the user abandoned; resuming that and saying
// "continue" is worse than a stated fresh start. Newer is by modification
// time, strictly: the live runtime appends to the conversation it is in.
// outgoing is the record being carried; only a Codex operator repair is
// judged differently, and the kind buys a Claude record nothing — no repair
// ever writes one.
func (c *conversationCarry) staleReason(program, workDir string, outgoing AgentConversationData) string {
	switch c.agent {
	case tmux.ProgramClaude:
		transcripts, err := claudeProjectTranscripts(filepath.Join(c.srcHome, filepath.Dir(c.transcript)))
		if err != nil {
			return "af could not list the conversations in this worktree to confirm the recorded one is current"
		}
		var recorded *claudeTranscript
		for idx := range transcripts {
			if strings.EqualFold(transcripts[idx].id, c.id) {
				recorded = &transcripts[idx]
			}
		}
		if recorded == nil {
			return "its transcript is missing from the previous account's home"
		}
		for _, transcript := range transcripts {
			if transcript.id != recorded.id && transcript.modTime.After(recorded.modTime) {
				return staleCarryReason
			}
		}
	case tmux.ProgramCodex:
		return c.staleCodexReason(program, workDir, outgoing)
	}
	return ""
}

func (c *conversationCarry) staleCodexReason(program, workDir string, outgoing AgentConversationData) string {
	// Only rollouts written after the recorded one can be a newer thread, so
	// only those have their session metadata read: a shared home can hold
	// thousands of rollouts for other worktrees.
	files := c.srcFiles
	if files == nil {
		var err error
		if files, err = codexRolloutFiles(c.srcHome); err != nil {
			return "af could not enumerate the previous account's rollout store"
		}
	}
	launchDir := workDir
	if launch, err := tmux.CommandEnvironmentFromCommand(program, workDir); err == nil && launch.WorkingDirKnown() {
		launchDir = launch.WorkingDir
	}
	recordedPath := filepath.Join(c.srcHome, c.transcript)
	var repair operatorRepairWindow
	if outgoing.CaptureKind == ConversationCaptureOperator {
		if decline := operatorRepairProvenance(recordedPath, launchDir); decline.reason != "" {
			return decline.reason
		}
		repair = operatorRepairWindowOf(outgoing, time.Now())
	}
	switch checkCodexRecordedStaleness(recordedPath, files, pathutil.ResolveForCompare(launchDir), repair) {
	case codexRolloutRecordedGone:
		return "its rollout is missing from the previous account's home"
	case codexRolloutStale:
		return staleCarryReason
	case codexRolloutUncorrelated:
		return "af could not confirm the recorded conversation is still the newest one in this worktree"
	}
	return ""
}

// codexRolloutRelPath returns the one rollout for id beneath home, relative to
// home, or "" when there is none.
func codexRolloutRelPath(home, id string) (string, string) {
	// A store that cannot be enumerated is not "no match": the walk stops at
	// the first failure, so the map it leaves behind is partial and the one
	// match it may hold proves nothing about uniqueness (#5471 review).
	files, err := codexRolloutFiles(home)
	if err != nil {
		return "", "af could not enumerate the Codex rollout store"
	}
	return codexRolloutRelPathIn(home, files, id)
}

// codexRolloutRelPathIn is codexRolloutRelPath over a store already
// enumerated in full.
func codexRolloutRelPathIn(home string, files map[string]struct{}, id string) (string, string) {
	var matches []string
	for path := range files {
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

// copy performs the carry. Only the transcript decides success. For a codex
// carry the commit has a second half: the durable carried-prefix watermark
// telling the usage reader that the copied bytes hold the SOURCE account's
// claims (#6802 review). A watermark that cannot be recorded does NOT fail
// the carry (#6776 review): the copied span is quarantined in the rails
// file instead, which makes every claim in it untrusted for proof and
// retraction until a real watermark lands. But a carry COMMITS copied
// bytes only when a durable record making them untrustworthy has landed —
// watermark OR quarantine marker (#6852 round-2 review). When neither can
// be written the install is unwound and the carry fails: the caller
// starts a fresh conversation whose brief says why, which is exactly the
// pre-quarantine behavior for a bookkeeping failure.
func (c *conversationCarry) copy() error {
	// The plan ran while the outgoing runtime could still backtrack (#7141).
	if reason := c.codexRevertReason(); reason != "" {
		return carryFailure(reason, nil)
	}
	outcome, err := carryConversationFileOutcome(c.srcRoot, c.dstRoot, c.transcript, !c.committed)
	if err != nil {
		return err
	}
	if err := c.commitCarriedPrefix(outcome); err != nil {
		if qerr := c.quarantineUnrecorded(err, outcome.covered); qerr != nil && outcome.installed {
			// No durable record exists to keep this copy's claims out of
			// the target's usage evidence — committing it would be the
			// silent-poison failure the quarantine exists to prevent.
			// Unwind the bytes this run installed; a verify-only outcome
			// wrote nothing it could uncommit, so it proceeds.
			restoreCarryDisplacement(c.dstRoot, c.transcript, outcome)
			return carryFailure("neither its usage watermark nor a quarantine record could be recorded",
				fmt.Errorf("watermark: %w; quarantine: %w", err, qerr))
		}
	}
	// The commit landed: the displaced original is gone for good now.
	discardCarryDisplacement(c.dstRoot, c.transcript, outcome)
	if c.aux == "" {
		return nil
	}
	if err := carryConversationTree(c.srcRoot, c.dstRoot, c.aux); err != nil {
		log.WarningLog.Printf("account swap carried %s conversation %s without all of its per-session files: %v",
			c.agent, c.id, err)
	}
	return nil
}

// commitCarriedPrefix records the carry's watermark durably: the first
// covered bytes of the carried rollout are the source account's records, and
// the usage indexer must skip claims whose line begins below that mark
// (internal/rateindex reads handoff.State.CarriedPrefixes). Codex writes
// rate_limits usage claims into its rollout files, and Claude writes
// usage_limit_reached refusal records into its transcripts — both agents
// carry files a usage reader can mistake for the target account's records
// (#6944 review), so both leave the watermark. Other carries copy nothing
// the reader would quarantine.
//
// A failure here does not unwind the copy (#6776 review): the caller
// quarantines the rollout through the rails file's unrecorded-carries
// marker instead, so the file's claims stay untrusted for usage proof
// while the conversation still carries.
func (c *conversationCarry) commitCarriedPrefix(outcome carryFileOutcome) error {
	if !carriesUsageClaims(c.agent) || outcome.covered == 0 {
		return nil
	}
	path := c.carriedTranscriptPath()
	if c.dstRoot.base == "" || path == "" {
		return carryFailure("the carried-prefix watermark has no AF home to land in", nil)
	}
	// The store lives under the AF home the carry itself resolved, not a
	// fresh GetConfigDir lookup — the watermark must land beside the
	// accounts/ tree the copy targeted.
	instances := filepath.Join(c.dstRoot.base, "instances")
	store := handoff.NewStore(filepath.Join(instances, handoff.StateFileName))
	if err := store.RecordCarriedPrefix(path, outcome.covered); err != nil {
		return carryFailure("the carried-prefix watermark could not be recorded", err)
	}
	// A real watermark retires the unrecorded-carry quarantine for this
	// file — but only when it COVERS the span the failed carry installed:
	// a shorter re-carry must not lift distrust its boundary cannot
	// account for (#6776 review). The comparison runs on the recorded
	// watermark, which may exceed this carry's covered — the record only
	// ever grows.
	watermark := outcome.covered
	if state, err := store.Load(); err == nil {
		if recorded := state.CarriedPrefixes[path]; recorded > watermark {
			watermark = recorded
		}
	}
	rails := handoff.NewRailsStore(filepath.Join(instances, handoff.RailsFileName),
		filepath.Join(instances, handoff.StateFileName))
	retired, err := rails.ClearUnrecordedCarry(path, watermark)
	if err != nil {
		log.WarningLog.Printf("account swap carried %s conversation %s but its quarantine record could not be retired: %v",
			c.agent, c.id, err)
	} else if !retired {
		log.WarningLog.Printf("account swap carried %s conversation %s with a watermark too short to retire an earlier unrecorded carry — rollout %s stays quarantined from usage accounting",
			c.agent, c.id, path)
	}
	return nil
}

// quarantineUnrecorded records in the rails file that this carry copied
// covered source-account bytes but could not record its watermark
// (cause). Claims in that span then count as carried poison — so the
// target's readings cannot free a wall on records that still belong to
// the source account (#6776 review). Returns nil once a durable record
// exists — a marker, or the account-level flag a full quarantine writes.
// When the write itself cannot land the error is the caller's signal
// that NOTHING stands between the copied span and the target's usage
// evidence: an installed copy must then be unwound rather than
// committed (#6852 round-2 review).
func (c *conversationCarry) quarantineUnrecorded(cause error, covered int64) error {
	path := c.carriedTranscriptPath()
	if c.dstRoot.base == "" || path == "" {
		err := fmt.Errorf("the rollout cannot be named for quarantine")
		log.WarningLog.Printf("account swap carried %s conversation %s without its watermark (%v), and %s — its usage claims are unquarantined",
			c.agent, c.id, cause, err)
		return err
	}
	instances := filepath.Join(c.dstRoot.base, "instances")
	store := handoff.NewRailsStore(
		filepath.Join(instances, handoff.RailsFileName),
		filepath.Join(instances, handoff.StateFileName))
	quarantined, err := store.NoteUnrecordedCarry(path, c.dstHome, covered, time.Now())
	if err != nil {
		log.WarningLog.Printf("account swap carried %s conversation %s without its watermark (%v) or a quarantine record — rollout %s cannot be excluded from usage accounting: %v",
			c.agent, c.id, cause, path, err)
		return err
	}
	if quarantined {
		log.WarningLog.Printf("account swap quarantine is at its cap — account home %s is untrusted for wall retraction until a watermark covering rollout %s records, or the rails are repaired",
			c.dstHome, path)
		return nil
	}
	log.WarningLog.Printf("account swap carried %s conversation %s but its watermark could not be recorded (%v) — rollout %s is quarantined from usage accounting for the copied span",
		c.agent, c.id, cause, path)
	return nil
}

func (c *conversationCarry) clone() *conversationCarry {
	if c == nil {
		return nil
	}
	cloned := *c
	cloned.srcRoot.components = append([]string(nil), c.srcRoot.components...)
	cloned.dstRoot.components = append([]string(nil), c.dstRoot.components...)
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
	if err := i.validateAccountSwapPlan(plan.account, plan.agent, plan.crossAgent, plan.manual, true, reason); err != nil {
		return reason, fmt.Errorf("account swap for %q could not carry its conversation (%s), and a fresh conversation could not be prepared either: %w",
			i.Title, reason, err)
	}
	return reason, nil
}

// ensureAccountSwapConversationCarried is the respawn-time half, run for
// every plan about to launch. A carry re-runs its idempotent copy (a daemon
// restart, or a session-level caller that never ran the pre-commit step). A
// plan that launches fresh — because that copy failed, or because restart
// validation already found the carry impossible — must not leave the pending
// record claiming the carried id: restart metadata sync would then reject the
// fresh id it launched, and the notice would say the conversation continues.
func (i *Instance) ensureAccountSwapConversationCarried(plan *accountSwapLaunchPlan) (*accountSwapLaunchPlan, error) {
	reason := plan.carryFallback
	if plan.carry != nil {
		copyReason, err := i.carryOrReplan(plan)
		if err != nil || copyReason == "" {
			return plan, err
		}
		reason = copyReason
	} else if !i.pendingAccountSwapClaimsCarry(plan.account) {
		return plan, nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = "the carried conversation could not be resumed"
	}
	if err := i.demotePendingAccountSwapCarry(plan.account, reason); err != nil {
		return nil, err
	}
	return i.accountSwapLaunchForRespawn()
}

func (i *Instance) pendingAccountSwapClaimsCarry(account string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	pending := i.pendingAccountSwap
	return pending != nil && pending.To == account && pending.CarriedConversationID != ""
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
	pending.CarriedLaunchStarted = false
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

// stampHandoffConversationLocked writes the pending swap's conversation
// outcome onto the ledger entry the swap's commit recorded, so the answer
// survives the record's retirement — the "and afterwards" half of #6768. A
// manual swap appends its AgentHandoff atomically with the pending record, so
// the trailing entry is always this swap's; an automatic swap records no
// entry at all and the ToAccount guard simply misses. The caller holds i.mu.
func (i *Instance) stampHandoffConversationLocked() {
	pending := i.pendingAccountSwap
	if pending == nil || !pending.Manual || len(i.Tabs) == 0 || len(i.Tabs[0].Handoffs) == 0 {
		return
	}
	last := &i.Tabs[0].Handoffs[len(i.Tabs[0].Handoffs)-1]
	if last.ToAccount != pending.To {
		return
	}
	last.Conversation = HandoffConversation{
		Carried:      pending.CarriedConversationID != "",
		CarryFailure: pending.CarryFallback,
	}
}

// carriedCodexRolloutPath is where the capture snapshot must expect the
// carried rollout, so capture never mistakes it for a newly minted thread.
func (c *conversationCarry) carriedCodexRolloutPath() string {
	if c == nil || c.agent != tmux.ProgramCodex {
		return ""
	}
	return filepath.Join(c.dstHome, c.transcript)
}

// carriesUsageClaims reports whether the carried file can hold provider
// usage records a reader would attribute to the target account: codex
// rollouts carry rate_limits claims and Claude transcripts carry
// usage_limit_reached records. Both need the carried-prefix watermark.
func carriesUsageClaims(agent string) bool {
	return agent == tmux.ProgramCodex || agent == tmux.ProgramClaude
}

// carriedTranscriptPath is the carried file's absolute path under the new
// account's home — the key the carried-prefix watermark records against.
func (c *conversationCarry) carriedTranscriptPath() string {
	if c == nil || !carriesUsageClaims(c.agent) {
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

// RecordAccountSwapConversationForRuntime is SetAgentConversationForRuntime
// for an account-swap replacement. When capture shows the replacement on a
// different Codex thread than the one carried — a provider that forked on
// resume — the pending record follows it, so restart recovery resumes the
// thread the replacement is actually writing, not the stale pre-fork one. The
// tab and the record change under one lock and reach disk in the same row.
//
// A carried-kind conv (ConversationCaptureCarried) is the carried id itself:
// captureCarriedCodexConversation fail-closes to it rather than adopt a
// sibling, so it must never rewrite a different CarriedConversationID. Only a
// conv captured as a new rollout — one whose provenance is independent of the
// carried thread — may follow a fork.
func (i *Instance) RecordAccountSwapConversationForRuntime(token AgentRuntimeToken, conv AgentConversationData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.runtimeTokenCurrentLocked(token, conv) {
		return false
	}
	changed := i.setAgentConversationLocked(conv)
	pending := i.pendingAccountSwap
	if pending != nil && pending.To == i.Account && pending.CarriedConversationID != "" &&
		conv.Agent == tmux.ProgramCodex && providerConversationIDRE.MatchString(conv.ID) &&
		pending.CarriedConversationID != conv.ID &&
		conv.CaptureKind != ConversationCaptureCarried {
		pending.CarriedConversationID = conv.ID
		i.touchLocked()
	}
	return changed
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

// abandonedCarryReason is what a replacement is told when a carried resume was
// launched once and the replacement then had to be launched again.
const abandonedCarryReason = "the replacement stopped before it was confirmed working on the carried conversation"

// AbandonCarriedConversationAfterFailedLaunch gives up on a committed carry
// whose resume was already launched once, before the replacement is launched
// again. The daemon calls it only when the replacement agent is gone. Nothing
// else can tell that the new account cannot run the carried conversation — a
// replacement that never became ready is left inert, and every retry would
// otherwise re-plan the same resume — so a second launch starts fresh and says
// why. A carry that never launched (a restart between checkpoint and launch)
// is kept.
func (i *Instance) AbandonCarriedConversationAfterFailedLaunch(account string) error {
	i.mu.RLock()
	pending := i.pendingAccountSwap
	abandon := pending != nil && pending.To == account &&
		pending.CarriedConversationID != "" && pending.CarriedLaunchStarted
	var id string
	if abandon {
		id = pending.CarriedConversationID
	}
	i.mu.RUnlock()
	if !abandon {
		return nil
	}
	log.WarningLog.Printf("account swap for %q launched carried conversation %s under account %q and must launch again; starting a fresh conversation instead",
		i.Title, id, account)
	if err := i.validateAccountSwapPlan(account, "", false, false, true, abandonedCarryReason); err != nil {
		return fmt.Errorf("account swap for %q could not prepare a fresh conversation after its carried launch failed: %w", i.Title, err)
	}
	return i.demotePendingAccountSwapCarry(account, abandonedCarryReason)
}

// markCarriedLaunchStarted records that the replacement is about to resume
// the carried conversation.
func (i *Instance) markCarriedLaunchStarted(account string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := i.pendingAccountSwap
	if pending == nil || pending.To != account || pending.CarriedConversationID == "" || pending.CarriedLaunchStarted {
		return
	}
	pending.CarriedLaunchStarted = true
	i.touchLocked()
}
