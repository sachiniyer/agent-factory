package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// ValidateAccountSwap checks the complete identity boundary before the current
// runtime is touched. Automatic replacement is deliberately local-only: Docker
// account creation remains supported, but a crash-safe automatic reprovision
// needs a durable container identity and immutable provision plan of its own.
func (i *Instance) ValidateAccountSwap(name string) error {
	return i.validateAccountSwap(name, "", false, false, true)
}

// ValidateManualAccountSwap uses the same launch proof with an operator-selected
// identity. agent names the handoff target; crossAgent is
// ManualAccountSwapProgram's decision for it.
func (i *Instance) ValidateManualAccountSwap(name, agent string, crossAgent bool) error {
	return i.validateAccountSwap(name, agent, crossAgent, true, true)
}

// CheckManualAccountSwap performs the manual launch proof without recording a
// launch plan. The daemon uses it before a project-lock identity probe so an
// independent domain refusal can remain visible; a successful check grants no
// authority to mutate and is repeated under the proven policy lock.
func (i *Instance) CheckManualAccountSwap(name, agent string, crossAgent bool) error {
	return i.validateAccountSwap(name, agent, crossAgent, true, false)
}

func (i *Instance) validateAccountSwap(name, agent string, crossAgent, manual, recordLaunch bool) error {
	return i.validateAccountSwapPlan(name, agent, crossAgent, manual, recordLaunch, "", false)
}

// validateAccountSwapPlan is validateAccountSwap with a carry failure this
// attempt already hit: a non-empty carryFailure plans a fresh conversation
// whose notice repeats it.
func (i *Instance) validateAccountSwapPlan(name, agent string, crossAgent, manual, recordLaunch bool, carryFailure string, requireCarry bool) error {
	backend := i.currentBackend()
	i.mu.RLock()
	current := i.Account
	auto := i.accountAutoSelected
	pending := cloneAccountSwapData(i.pendingAccountSwap)
	op := i.inFlightOp
	pendingCleanup := len(i.pendingTabCleanup)
	tabs := append([]*Tab(nil), i.Tabs...)
	var outgoing AgentConversationData
	if len(i.Tabs) > 0 {
		outgoing = i.Tabs[0].Conversation
	}
	i.mu.RUnlock()
	if op != OpRespawning {
		return fmt.Errorf("account swap for %q requires the limit-resume fence", i.Title)
	}
	committedManual := pending != nil && pending.Manual && pending.To == current && name == current
	if strings.TrimSpace(current) != "" && !auto && !manual && !committedManual {
		return fmt.Errorf("account %q was explicitly pinned for session %q and will not be overridden", current, i.Title)
	}
	if backend == nil {
		return fmt.Errorf("session %q has no backend on record", i.Title)
	}
	if typ := backend.Type(); typ != "local" {
		return fmt.Errorf("automatic account swapping is supported only by the local backend, not %s", typ)
	}
	if pendingCleanup > 0 {
		return fmt.Errorf("cannot switch accounts for session %q while %d prior tab teardown(s) remain unconfirmed; restart af to retry that cleanup, then retry the account swap", i.Title, pendingCleanup)
	}
	refuse := func(err error) error {
		return fmt.Errorf("cannot switch session %q to account %q: %w", i.Title, name, err)
	}
	resolution, err := resolveLaunchProgramForInstance(i)
	if err != nil {
		return refuse(err)
	}
	// paneGeneratedArgs seeds the launch proof (accountLaunchProof) with the
	// pane's still-recorded af-authored argument declaration when the swap base
	// is the pane's frozen program. That program already carries af's own
	// injected flags (e.g. codex's startup-update-check flag #5433); recomputing
	// the declaration from that base treats af's words as the user's — an
	// undeclared argument — so the proof must seed from the recorded declaration
	// instead. It is populated only for a same-agent swap whose resolved base
	// matches the pane's frozen program, regardless of whether that base came
	// from RuntimeProgram or the empty-evidence pane-fallback; cross-agent and
	// built-in override bases either do not carry af's words or keep the
	// positional-extension shape GeneratedArgsBetween already describes.
	var paneGeneratedArgs []string
	// paneTrustedExecutable is the executable provenance the pane recorded
	// alongside paneGeneratedArgs. It is empty for an untrusted absolute
	// override (the launcher never vouched for the path) and the detected path
	// for a built-in launch; carrying it through the proof keeps an untrusted
	// override rejected rather than letting the proof infer the base's first
	// word, which would launder a repository-controlled executable into the
	// account boundary's trust.
	var paneTrustedExecutable string
	// A cross-agent swap resolves the target enum's own command and namespace;
	// a same-agent one keeps the recorded program. With program_overrides.aider
	// = "codex" running a codex pane, `--to codex` whose override resolves to
	// aider IS cross-agent, while `--to aider` — and an account-only request,
	// whose agent is the running identity rather than an enum — is not (#4430
	// review).
	if crossAgent {
		if resolution, err = resolveLaunchProgramForAgent(i, agent); err != nil {
			return refuse(err)
		}
	} else {
		// A same-agent or account-only swap promises to keep the agent that is
		// RUNNING — which is the command that positively established this pane,
		// not the stored enum re-resolved under today's overrides. With
		// program_overrides.claude = "codex" at launch and a later edit to
		// "gemini", a fresh resolution would launch gemini while the swap still
		// claims same-agent (#4430 review round 6). The recorded runtime is not
		// a built-in declaration, so trustBase resets with the command. A
		// committed transaction retrying post-restart may have no runtime
		// record left, but its pane still runs the command the checkpoint
		// launched — that evidence outranks a fresh resolution too.
		established := i.RuntimeProgram()
		runtimeEstablished := established != ""
		if !runtimeEstablished {
			established = i.ResolvedPaneProgram()
		}
		switch {
		case pending != nil && pending.To == name && pending.Program != "":
			// The committed transaction froze the incoming command at commit.
			// A restart can reach this retry while the pane still runs the
			// OUTGOING agent — the checkpoint precedes the replacement — so
			// the established evidence is the predecessor's and must not win
			// (#4430 review round 7).
			resolution = launchProgramResolution{command: pending.Program}
			// Re-derive the built-in provenance of the frozen command the same
			// way the fresh resolution and the established-evidence branch do:
			// the committed base can carry af's own detected/overridden flags
			// (the default claude override freezes the detected path plus
			// --dangerously-skip-permissions), and without trustBase the proof
			// declares only af-appended words, refusing the frozen flag as an
			// undeclared user argument on every retry. The pane-declaration
			// seeding gate below cannot rescue this either: pending.Program is
			// the pre-injection base while the pane records its program
			// post-injection, so the exact-match gate never fires here.
			// Unlike the fresh-resolution and established-evidence branches,
			// the frozen command is a matter of durable record, not a fresh
			// resolution: program_overrides may have been edited between the
			// durable commit and the restart's recovery, so the provenance
			// question is only "is this argv the one af detected" — answered
			// by the built-in candidate comparison alone, without requiring
			// the frozen command to still be today's effective override.
			// builtInProgramOverride's effective-resolution gate would refuse
			// a frozen built-in flagged base whenever the override moved,
			// wedging the retry forever (#6820 remainder).
			committedAgent := pending.AccountAgent
			if committedAgent == "" {
				committedAgent = sessionenv.AgentForCommand(pending.Program)
			}
			resolved, err := resolveResolvedConfigForInstance(i)
			if err != nil {
				return refuse(err)
			}
			resolution.trustBase = builtInOverrideCandidateMatches(resolved, committedAgent, pending.Program)
		case pending != nil && pending.To == name && pending.AccountAgent != "" &&
			tmux.DetectAgentFromCommand(established) != pending.AccountAgent:
			// A pending record written before Program existed: the committed
			// namespace disagrees with the surviving runtime evidence, so that
			// evidence is the predecessor's. Re-resolve the enum the commit
			// recorded in the handoff ledger; the drift check below still
			// refuses if configuration has since moved it off the committed
			// namespace.
			if target := i.PendingAccountSwapTarget(); target != "" {
				if resolution, err = resolveLaunchProgramForAgent(i, target); err != nil {
					return refuse(err)
				}
			}
		case established != "":
			resolution = launchProgramResolution{command: established}
			// The recorded runtime is the base af itself resolved at launch, so
			// when it is exactly af's detected built-in for the session's agent
			// (same argv, same resolution snapshot) the flag af appended to it
			// is af-authored, not undeclared (#5623). Any other command — a
			// repo/operator override, a different path, an extra word — keeps
			// trustBase false and stays refused.
			if runtimeEstablished {
				i.mu.RLock()
				enum := i.Program
				i.mu.RUnlock()
				resolved, err := resolveResolvedConfigForInstance(i)
				if err != nil {
					return refuse(err)
				}
				resolution.trustBase = builtInProgramOverride(resolved, enum, established)
			}
		}
		// When the resolved base IS the pane's frozen program, that command
		// already carries af's injected flags (the pane-fallback state after an
		// unverified reattach, a launch whose pane query failed, or a record
		// predating runtime-program evidence): seed the launch proof from the
		// pane's still-recorded declaration so af's own words are declared
		// rather than treated as undeclared user arguments (#5433). The string
		// match scopes the seeding to the base the pane actually recorded a
		// declaration for — a committed retry whose frozen base matches the
		// pane, or a fresh pane-fallback — and the proof fails closed when a
		// caller records no matching declaration (a user-supplied argument af
		// did not author keeps the base suffix from matching the recorded
		// words, so only af's own declared flags may pass). The seeding fires
		// whether the base came from RuntimeProgram or the empty-evidence
		// pane-fallback: a row last respawned by a version that recorded the
		// flagged pane program as RuntimeProgram (the poison the clean-base
		// respawn boundary now prevents) still reaches this gate with
		// runtimeEstablished true yet the same recorded declaration is still
		// the provenance the boundary already accepts for that base. Requiring
		// runtimeEstablished to be false (the earlier gate) left af's own flag
		// undeclared on that next swap and the swap was refused.
		if resolution.command != "" &&
			resolution.command == i.ResolvedPaneProgram() {
			paneGeneratedArgs = i.ResolvedPaneGeneratedArgs()
			paneTrustedExecutable = i.ResolvedPaneTrustedExecutable()
		}
	}
	resolvedProgram := resolution.command
	// What RuntimeProgram records for this launch, decided now and carried in the
	// plan. The launch base is clean unless it is the pane's frozen program with
	// af's own declared flags (the pane-fallback state). Then the clean base is
	// DERIVED from the launched command itself: the pane's frozen program minus
	// exactly the words af's recorded declaration says it authored, accepted only
	// when that removal is provable. It is never taken from a fresh resolution of
	// the configuration (an override may have changed before preflight while the
	// frozen pane command is what launches). A conversation carry's `resume <id>`
	// is added on top of this base at launch, so it never enters it. If the base
	// cannot be proven, nothing is recorded rather than false evidence.
	runtimeBase := resolvedProgram
	if len(paneGeneratedArgs) > 0 {
		var proven bool
		if runtimeBase, proven = sessionenv.StripDeclaredArgs(resolvedProgram, paneGeneratedArgs); !proven {
			runtimeBase = ""
		}
	}
	if args := tmux.ConversationSelectorArgs(resolvedProgram); len(args) > 0 {
		return fmt.Errorf("cannot switch session %q to account %q because its resolved program pins an existing conversation with arguments %s; an account swap must choose which conversation the replacement opens (the carried one or a fresh one), so remove those arguments and retry", i.Title, name, strings.Join(args, " "))
	}
	workDir := i.GetWorktreePath()
	carry, carryFallback := planAccountSwapCarry(accountSwapCarryRequest{
		agent:      tmux.DetectAgentFromCommand(resolvedProgram),
		crossAgent: crossAgent,
		outgoing:   outgoing,
		current:    current,
		target:     name,
		pending:    pending,
		program:    resolvedProgram,
		workDir:    workDir,
		fallback:   carryFailure,
		// A newer conversation refuses the carry only where af owns the
		// directory, as on the restore path (#7227), or cannot tell (#7272).
		newerIsEvidence: i.newerConversationCountsAgainstCarry,
	})
	var launchProgram string
	var conversation AgentConversationData
	if carry != nil {
		var ok bool
		if launchProgram, conversation, ok = carry.launch(resolvedProgram); !ok {
			carry, carryFallback = nil, carryUnresumableReason
		}
	}
	if err := requiredCarryRefusal(requireCarry, pending != nil && pending.To == name, carry, carryFallback); err != nil {
		return refuse(err)
	}
	// reminted reports that a committed replacement's recorded conversation id
	// was replaced by a fresh one, which the record takes below.
	var reminted bool
	if carry == nil {
		if pending != nil && pending.To == name && pending.ConversationID != "" {
			// A re-launch of a committed replacement: its id may already name a
			// transcript in the target account's store (#7214).
			launchProgram, conversation, reminted = i.committedConversationLaunch(
				resolvedProgram, workDir, name, pending.ConversationID)
		} else {
			launchProgram, conversation = planLaunchConversation(newSessionID(), resolvedProgram)
		}
	}
	// A swap selects the account in the namespace of the command it will
	// actually launch, not the enum the session was created under: a session
	// recorded as claude whose override resolves to codex is RUNNING codex,
	// and the account must come from the registry the launch's agent reads
	// (#4430 review). resolvedProgram is that frozen command — resolved above
	// from the same config — so no second resolution can disagree with it.
	// The committed manual transaction is the one caller whose account
	// namespace is a matter of record rather than resolution: the swap
	// already moved this session to name inside pending.AccountAgent's
	// registry, so its retry must resolve there even when program_overrides
	// have since moved the enum's resolution. The same namespace feeds the
	// skill target below: a redirect that selects codex's registry must write
	// the af skill under codex's account root, and the resolved-namespace
	// answer is what resolveSkillTargetForAccount compares the launch's
	// detected agent against (#4430 review round 5).
	accountNamespace := sessionenv.AgentForCommand(resolvedProgram)
	if pending != nil && pending.To == name {
		// A committed transaction's namespace is a matter of record, never a
		// fresh resolution: the commit already moved this session into name's
		// registry, and program_overrides may have moved since — re-resolving
		// could select a same-named account in another agent's registry that
		// the drift check then accepts because command and account agree with
		// each other instead of with the commit (#4430 review round 6). The
		// manual transaction records its namespace on the pending data; the
		// automatic one's record is the durable pin the commit itself
		// installed on the instance.
		accountNamespace = pending.AccountAgent
		if accountNamespace == "" {
			accountNamespace = i.AccountAgent()
		}
		if accountNamespace == "" {
			accountNamespace = sessionenv.AgentForCommand(resolvedProgram)
		}
	}
	// The CANDIDATE account and program, not the still-recorded fields: validation
	// must leave the outgoing identity intact, while the af skill has to land in
	// the root the replacement pane will actually read.
	env, err := launchEnvFor(i, workDir)
	if err != nil {
		return refuse(err)
	}
	launchProgram = injectSystemPromptWithEnv(launchProgram,
		resolveSkillTargetForAccount(launchProgram, accountNamespace, name, workDir), i.guidanceCaps(), env)
	// Same-agent manual swaps with a worktree always preflight, including an
	// unchanged command whose binary disappeared after the current process
	// started. Worktree-less projections cannot launch, so they retain the
	// storage-only path; cross-agent swaps must still refuse that absence.
	if crossAgent && workDir == "" {
		return fmt.Errorf("handoff target %s has no worktree path for launch preflight", agent)
	}
	if manual && workDir != "" {
		if _, err := preflight.CheckCommandAt(launchProgram, workDir); err != nil {
			return fmt.Errorf("handoff target %s failed launch preflight: %w", agent,
				swapPreflightCause(i.Title, workDir, agent, resolvedProgram, err))
		}
	}
	proof := accountLaunchProof(resolvedProgram, launchProgram, resolution.trustBase, paneGeneratedArgs, paneTrustedExecutable)
	if err := tmux.ValidateAccountLaunchSupport(name); err != nil {
		return fmt.Errorf("cannot switch session %q to account %q: %w", i.Title, name, err)
	}
	accountScope, err := selectAccountInNamespace(accountNamespace, name)
	if err != nil {
		return fmt.Errorf("cannot select account %q for session %q: %w", name, i.Title, err)
	}
	if err := refuseAccountAgentDrift(name, accountScope.Agent, launchProgram); err != nil {
		return fmt.Errorf("cannot switch session %q to account %q: %w", i.Title, name, err)
	}
	if !carry.sameCarryHome(accountScope.Dir) {
		return fmt.Errorf("cannot switch session %q to account %q: its conversation carry resolved %s but the launch resolved %s",
			i.Title, name, carry.dstHome, accountScope.Dir)
	}
	accountScope.TrustedExecutable = proof.TrustedExecutable
	accountScope.GeneratedArgs = proof.GeneratedArgs
	filtered := sessionenv.FilterForCommand(
		os.Environ(), accountScope.Agent, launchProgram, env.passthrough)
	if _, err := sessionenv.ApplyAccount(filtered, launchProgram, accountScope); err != nil {
		return fmt.Errorf("cannot switch session %q to account %q: %w", i.Title, name, err)
	}
	for idx, tab := range tabs {
		if idx == 0 {
			continue
		}
		if tab == nil || !tab.Kind.HasTmux() {
			continue
		}
		if tab.tmux == nil {
			return fmt.Errorf("cannot switch session %q to account %q because tab %q has no tmux binding to replace", i.Title, name, tab.Name)
		}
		if tab.Kind == TabKindProcess {
			// The swap stops a process tab and never relaunches it (#4479), so its
			// command is not a replacement command: preflighting it, or refusing
			// its arguments, would block a swap over something that will not run
			// (#4506 review). The binding check above is what the stop needs.
			continue
		}
		replacementProgram := tab.tmux.Program()
		if tab.Kind == TabKindShell {
			var err error
			replacementProgram, err = sessionenv.AccountShellCommand(replacementProgram)
			if err != nil {
				return fmt.Errorf("cannot switch session %q to account %q because tab %q has no proven account-scoped shell replacement: %w", i.Title, name, tab.Name, err)
			}
		}
		// A healthy manual handoff must prove every replacement command before
		// stopping any pane. Automatic recovery retains its existing retryable
		// behavior; only the manual path risks tearing down a healthy runtime.
		if manual && workDir != "" {
			if _, err := preflight.CheckCommandAt(replacementProgram, workDir); err != nil {
				return fmt.Errorf("cannot switch session %q to account %q because tab %q failed launch preflight: %w", i.Title, name, tab.Name,
					swapPreflightCause(i.Title, workDir, replacementProgram, replacementProgram, err))
			}
		}
		if args := tmux.ConversationSelectorArgs(replacementProgram); len(args) > 0 {
			return fmt.Errorf("cannot switch session %q to account %q because tab %q pins an existing conversation with arguments %s; an account swap restarts that tab under a separate conversation store", i.Title, name, tab.Name, strings.Join(args, " "))
		}
		if err := sessionenv.ValidateAccountEnvironmentCommand(replacementProgram, accountScope); err != nil {
			return fmt.Errorf("cannot switch session %q to account %q while tab %q pins another identity: %w", i.Title, name, tab.Name, err)
		}
	}
	var conversationCapture ConversationCaptureSnapshot
	if accountScope.Agent == tmux.ProgramCodex {
		workDir := i.GetWorktreePath()
		if strings.TrimSpace(workDir) == "" {
			return fmt.Errorf("cannot switch session %q to account %q: local worktree has no launch directory for Codex conversation capture", i.Title, name)
		}
		launch, err := tmux.CommandEnvironmentFromCommand(launchProgram, workDir)
		if err != nil {
			return fmt.Errorf("cannot prepare Codex conversation capture while switching session %q to account %q: %w", i.Title, name, err)
		}
		captureWorkingDir := ""
		if launch.WorkingDirKnown() {
			captureWorkingDir = launch.WorkingDir
		}
		conversationCapture = beginConversationCaptureAtCodexHomeAndWorkingDir(
			accountScope.Dir, captureWorkingDir)
		if err := conversationCapture.ScopeErr(); err != nil {
			return worktreeLookupError(i.Title, workDir, err)
		}
		if path := carry.carriedCodexRolloutPath(); path != "" {
			conversationCapture.expectCarriedCodexRollout(conversation, path)
		}
	}
	if !recordLaunch {
		return nil
	}
	i.mu.Lock()
	if i.inFlightOp != OpRespawning {
		i.mu.Unlock()
		return fmt.Errorf("account swap for %q lost the limit-resume fence during launch preflight", i.Title)
	}
	i.accountSwapLaunch = &accountSwapLaunchPlan{
		account: name, base: resolvedProgram, runtimeBase: runtimeBase, program: launchProgram,
		proof: proof, conversation: conversation, conversationCapture: conversationCapture,
		agent: agent, crossAgent: crossAgent, manual: manual,
		carry: carry, carryFallback: carryFallback, passthrough: env.passthrough, passthroughFrozen: true,
	}
	var replaced, missionFrom string
	refreshMission := false
	if live := i.pendingAccountSwap; reminted && live != nil && live.To == name && live.CarriedConversationID == "" &&
		live.ConversationID != conversation.ID {
		// The record names the conversation the re-launch starts, never the
		// stale id it no longer resumes, and says why the replacement starts
		// fresh: the API answer and the notice report CarryFallback (#7214,
		// #7216 review R4).
		replaced, live.ConversationID = live.ConversationID, conversation.ID
		live.CarryFallback = staleCommittedConversationReason
		i.pendingAccountSwapUnwritten = true
		refreshMission, missionFrom = live.Manual, live.From
		i.touchLocked()
	}
	i.mu.Unlock()
	if replaced != "" {
		log.WarningLog.Printf("account swap for %q re-launches its replacement on a fresh conversation %s instead of committed conversation %s, because %s",
			i.Title, conversation.ID, replaced, staleCommittedConversationReason)
		if refreshMission {
			// A manual mission was rendered for the conversation the replacement
			// no longer runs (demotePendingAccountSwapCarry's case).
			i.refreshPendingManualAccountSwapMission(missionFrom, name)
		}
	}
	// The pane and the API response say why; af's log must too (#7152). A
	// re-plan after a failed copy is the copy's own refusal, logged there.
	if carry == nil && carryFallback != "" && carryFailure == "" {
		remedy := ""
		if r := carryRefusalRemedy(carryFallback); r != "" {
			remedy = "; " + r
		}
		log.WarningLog.Printf("account swap for %q cannot carry conversation %q into account %q, so the replacement starts a fresh conversation: %s%s",
			i.Title, outgoing.ID, name, carryFallback, remedy)
	}
	return nil
}
