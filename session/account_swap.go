package session

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// accountSwapStopper is implemented by runtimes that can conclusively stop the
// current agent while retaining the session record and workspace. The daemon
// commits the new account only after this returns, so an unanswered teardown can
// never leave a live old identity recorded as the new one.
type accountSwapStopper interface {
	stopForAccountSwap(*Instance, bool) error
}

type accountSwapLaunchPlan struct {
	account             string
	base                string
	program             string
	proof               sessionenv.AccountLaunchProof
	conversation        AgentConversationData
	conversationCapture ConversationCaptureSnapshot
	// agent, crossAgent, and manual are the admission's own arguments, kept so
	// a failed conversation carry can rebuild this plan as a fresh one.
	agent      string
	crossAgent bool
	manual     bool
	// carry is the same-agent conversation copy this plan resumes (#4367);
	// carryFallback says why a carry that applied launches fresh instead.
	carry         *conversationCarry
	carryFallback string
}

func cloneAccountSwapLaunchPlan(plan *accountSwapLaunchPlan) *accountSwapLaunchPlan {
	if plan == nil {
		return nil
	}
	copy := *plan
	copy.proof.GeneratedArgs = append([]string(nil), plan.proof.GeneratedArgs...)
	copy.conversationCapture = cloneConversationCaptureSnapshot(plan.conversationCapture)
	copy.carry = plan.carry.clone()
	return &copy
}

func respawnLaunchProgram(i *Instance, resolvedProgram, declarationBase string,
	trustBase, resume bool, prepared *accountSwapLaunchPlan,
) (string, sessionenv.AccountLaunchProof) {
	if prepared != nil {
		if prepared.conversation.HasID() {
			i.SetAgentConversation(prepared.conversation)
		}
		return prepared.program, prepared.proof
	}
	program := resolvedProgram
	if resume {
		program = prepareResumeConversation(i, program)
	} else {
		program = prepareLaunchConversation(i, program)
	}
	program = injectSystemPrompt(program, resolveSkillTarget(i, program))
	return program, accountLaunchProof(declarationBase, program, trustBase)
}

// AccountSelection reports the current account and whether af selected it. A
// non-empty account with auto=false is an explicit --account pin.
func (i *Instance) AccountSelection() (account string, auto bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Account, i.accountAutoSelected
}

// AccountAgent reports the agent namespace the pinned account was selected in —
// durable evidence that a program_overrides edit after the pin cannot move
// (#4430 review round 4). "" when the session is ambient.
func (i *Instance) AccountAgent() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.accountNamespaceLocked()
}

// accountNamespaceLocked answers which agent's registry Account was selected
// under. The durable field wins; a record older than it falls back to the
// requested program's enum — the only namespace its selection could have used,
// since the redirected-account handoff that separates label from enum is newer
// than the field. Caller holds i.mu.
func (i *Instance) accountNamespaceLocked() string {
	if i.Account == "" {
		return ""
	}
	if i.accountAgent != "" {
		return i.accountAgent
	}
	return sessionenv.AgentForCommand(i.Program)
}

func cloneAccountSwapData(data *AccountSwapData) *AccountSwapData {
	if data == nil {
		return nil
	}
	copy := *data
	copy.OriginalStartupStateUnknown = cloneBoolPointer(data.OriginalStartupStateUnknown)
	return &copy
}

// PendingAccountSwap reports the committed move whose replacement notice and
// task have not yet been confirmed delivered.
func (i *Instance) PendingAccountSwap() (from, to string, pending bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.pendingAccountSwap == nil {
		return "", "", false
	}
	return i.pendingAccountSwap.From, i.pendingAccountSwap.To, true
}

// SupportsAutomaticAccountSwap reports whether this runtime can replace its
// credential boundary without changing backend kind.
func (i *Instance) SupportsAutomaticAccountSwap() bool {
	backend := i.currentBackend()
	return backend != nil && backend.Type() == "local"
}

// ValidateAccountSwap checks the complete identity boundary before the current
// runtime is touched. Automatic replacement is deliberately local-only: Docker
// account creation remains supported, but a crash-safe automatic reprovision
// needs a durable container identity and immutable provision plan of its own.
func (i *Instance) ValidateAccountSwap(name string) error {
	return i.validateAccountSwap(name, "", false, false, true)
}

// ManualAccountSwapProgram decides which program a manual account swap
// launches: agent's own command for a cross-agent handoff, the recorded
// program otherwise. The daemon asks it once, at admission, and hands the same
// crossAgent to ValidateManualAccountSwap and SelectAccountForHandoff so the
// frozen launch and the record cannot disagree.
//
// "Same agent" is HandoffTargetIsCurrent, the same-target guard's predicate:
// with program_overrides.aider = "codex" running a codex pane, `--to codex`
// whose own override resolves to aider is CROSS-agent, while `--to aider` is
// the same-agent account change despite the enum differing from Program.
//
// accountOnly (no --to) is same-agent by construction and skips the predicate.
// Its agent is the running IDENTITY, not an enum whose override produced the
// pane, so resolving it through its own override answers a question nobody
// asked: with program_overrides.claude = "codex" and program_overrides.codex =
// "gemini", a claude-configured codex pane would read "codex" as a gemini
// launch and turn `--account work` into a cross-agent handoff (#4430 review).
// Resolution does config I/O, so this must not run under i.mu.
func (i *Instance) ManualAccountSwapProgram(agent string, accountOnly bool) (program string, crossAgent bool) {
	program = i.AgentProgram()
	agent = strings.TrimSpace(agent)
	if accountOnly || agent == "" ||
		HandoffTargetIsCurrent(i.CurrentAgentName(), agent, handoffEffectiveAgent(i, agent), program) {
		return program, false
	}
	return agent, true
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
	return i.validateAccountSwapPlan(name, agent, crossAgent, manual, recordLaunch, "")
}

// validateAccountSwapPlan is validateAccountSwap with a carry failure this
// attempt already hit: a non-empty carryFailure plans a fresh conversation
// whose notice repeats it.
func (i *Instance) validateAccountSwapPlan(name, agent string, crossAgent, manual, recordLaunch bool, carryFailure string) error {
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
	resolution := resolveLaunchProgramForInstance(i)
	// A cross-agent swap resolves the target enum's own command and namespace;
	// a same-agent one keeps the recorded program. With program_overrides.aider
	// = "codex" running a codex pane, `--to codex` whose override resolves to
	// aider IS cross-agent, while `--to aider` — and an account-only request,
	// whose agent is the running identity rather than an enum — is not (#4430
	// review).
	if crossAgent {
		resolved := resolveResolvedConfigForInstance(i)
		resolution.command = resolveProgramForAgent(i, agent)
		resolution.trustBase = builtInProgramOverride(resolved, agent, resolution.command)
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
		if runtime := i.RuntimeProgram(); runtime != "" {
			resolution = launchProgramResolution{command: runtime}
		} else if pane := i.ResolvedPaneProgram(); pane != "" {
			resolution = launchProgramResolution{command: pane}
		}
	}
	resolvedProgram := resolution.command
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
	})
	var launchProgram string
	var conversation AgentConversationData
	if carry != nil {
		var ok bool
		if launchProgram, conversation, ok = carry.launch(resolvedProgram); !ok {
			carry, carryFallback = nil, "the resolved program cannot resume a specific conversation"
		}
	}
	if carry == nil {
		conversationID := newSessionID()
		if pending != nil && pending.To == name && pending.ConversationID != "" {
			conversationID = pending.ConversationID
		}
		launchProgram, conversation = planLaunchConversation(conversationID, resolvedProgram)
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
	launchProgram = injectSystemPrompt(launchProgram,
		resolveSkillTargetForAccount(launchProgram, accountNamespace, name))
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
				preflight.ProgramError(agent, resolvedProgram, err))
		}
	}
	proof := accountLaunchProof(resolvedProgram, launchProgram, resolution.trustBase)
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
		os.Environ(), accountScope.Agent, launchProgram, sessionEnvPassthroughForInstance(i))
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
					preflight.ProgramError(replacementProgram, replacementProgram, err))
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
		if path := carry.carriedCodexRolloutPath(); path != "" {
			conversationCapture.expectCarriedCodexRollout(conversation, path)
		}
	}
	if !recordLaunch {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return fmt.Errorf("account swap for %q lost the limit-resume fence during launch preflight", i.Title)
	}
	i.accountSwapLaunch = &accountSwapLaunchPlan{
		account: name, base: resolvedProgram, program: launchProgram,
		proof: proof, conversation: conversation, conversationCapture: conversationCapture,
		agent: agent, crossAgent: crossAgent, manual: manual,
		carry: carry, carryFallback: carryFallback,
	}
	return nil
}

// AccountSwapConversationCapture returns the provider-store before-image
// frozen by account-swap preflight. The caller starts discovery only after the
// same plan has launched, preserving the before/after boundary.
func (i *Instance) AccountSwapConversationCapture() (ConversationCaptureSnapshot, error) {
	plan, err := i.accountSwapLaunchForRespawn()
	if err != nil {
		return ConversationCaptureSnapshot{}, err
	}
	return cloneConversationCaptureSnapshot(plan.conversationCapture), nil
}

func (i *Instance) accountSwapLaunchForRespawn() (*accountSwapLaunchPlan, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.accountSwapLaunch == nil || i.accountSwapLaunch.account != i.Account {
		return nil, fmt.Errorf("account swap for %q has no launch plan preflighted for account %q", i.Title, i.Account)
	}
	return cloneAccountSwapLaunchPlan(i.accountSwapLaunch), nil
}

// StopForAccountSwap conclusively stops the old runtime. It does not change the
// recorded account; the daemon persists that commit only after this boundary.
func (i *Instance) StopForAccountSwap() error {
	backend := i.currentBackend()
	i.mu.RLock()
	op := i.inFlightOp
	i.mu.RUnlock()
	if op != OpRespawning {
		return fmt.Errorf("account swap for %q requires the limit-resume fence", i.Title)
	}
	if backend == nil {
		return fmt.Errorf("session %q has no backend on record", i.Title)
	}
	stopper, ok := backend.(accountSwapStopper)
	if !ok {
		return fmt.Errorf("account swapping is not supported by the %s backend", backend.Type())
	}
	return stopper.stopForAccountSwap(i, false)
}

// StopRemainingPanesForAccountSwap rechecks every local pane when the agent
// probe reported that tab zero was absent. The probe is only a hint: a retry
// still needs ProvenNoPane or a blindness-aware teardown before it can treat
// any credential-bearing pane as gone.
func (i *Instance) StopRemainingPanesForAccountSwap() error {
	backend := i.currentBackend()
	i.mu.RLock()
	op := i.inFlightOp
	i.mu.RUnlock()
	if op != OpRespawning {
		return fmt.Errorf("account swap for %q requires the limit-resume fence", i.Title)
	}
	if backend == nil || backend.Type() != "local" {
		return nil
	}
	stopper, ok := backend.(accountSwapStopper)
	if !ok {
		return fmt.Errorf("account swapping is not supported by the %s backend", backend.Type())
	}
	return stopper.stopForAccountSwap(i, true)
}

// SelectAccountAutomatically commits the scheduler's replacement identity in
// memory. The caller must persist it before starting the replacement runtime.
func (i *Instance) SelectAccountAutomatically(from, name string) (AgentConversationData, error) {
	return i.selectAccount(from, name, true)
}

func (i *Instance) selectAccount(from, name string, automatic bool) (AgentConversationData, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.selectAccountLocked(from, name, "", automatic)
}

func (i *Instance) selectAccountLocked(from, name, accountAgent string, automatic bool) (AgentConversationData, error) {
	if i.inFlightOp != OpRespawning {
		return AgentConversationData{}, fmt.Errorf("selecting account for %q requires the limit-resume fence", i.Title)
	}
	var previous AgentConversationData
	if len(i.Tabs) > 0 {
		previous = i.Tabs[0].Conversation
		i.setAgentConversationLocked(AgentConversationData{})
	}
	// Invalidate asynchronous discovery attached to the stopped process before it
	// can put that process's account-local conversation back into the live slot.
	i.agentRuntimeGeneration++
	i.clearAgentModelChangeLocked()
	// The namespace the account was selected in is the agent of the frozen
	// launch command — the same resolution the registry's Selected answered.
	// Recording it keeps the pin stable when a program_overrides edit between
	// commit and a later respawn resolves the recorded Program differently
	// (#4430 review round 4). The caller-supplied value is the same answer the
	// locked admission proved; the frozen plan wins when both exist because it
	// is what will actually launch.
	if plan := i.accountSwapLaunch; plan != nil && plan.account == name {
		if agent := sessionenv.AgentForCommand(plan.base); agent != "" {
			accountAgent = agent
		}
	}
	if accountAgent == "" {
		accountAgent = i.accountNamespaceLocked()
	}
	if i.accountAgent != accountAgent {
		i.accountAgent = accountAgent
		i.touchLocked()
	}
	if i.Account != name {
		i.Account = name
		i.touchLocked()
	}
	if i.accountAutoSelected != automatic {
		i.accountAutoSelected = automatic
		i.touchLocked()
	}
	pending := &AccountSwapData{From: from, To: name}
	if plan := i.accountSwapLaunch; plan != nil && plan.account == name {
		switch {
		case plan.carry != nil:
			pending.CarriedConversationID = plan.carry.id
			pending.CarrySourceAccount = plan.carry.sourceAccount
		case plan.conversation.HasID():
			pending.ConversationID = plan.conversation.ID
		}
		pending.CarryFallback = plan.carryFallback
	}
	i.pendingAccountSwap = pending
	i.touchLocked()
	return previous, nil
}

// RestoreAccountSelectionUnderResumeFence rolls back an in-memory selection
// whose durable checkpoint failed. The stopped old runtime is not restarted;
// the next scheduler pass retries from the still-durable previous identity.
// accountAgent is the previous selection's namespace — captured beside the
// account name at admission, so the rollback restores the same pin (#4430).
func (i *Instance) RestoreAccountSelectionUnderResumeFence(name, accountAgent string, auto bool, conversation AgentConversationData) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return fmt.Errorf("restoring account for %q requires the limit-resume fence", i.Title)
	}
	if i.Account != name {
		i.Account = name
		i.touchLocked()
	}
	if i.accountAgent != accountAgent {
		i.accountAgent = accountAgent
		i.touchLocked()
	}
	if i.accountAutoSelected != auto {
		i.accountAutoSelected = auto
		i.touchLocked()
	}
	if i.pendingAccountSwap != nil {
		i.pendingAccountSwap = nil
		i.touchLocked()
	}
	i.accountSwapLaunch = nil
	i.setAgentConversationLocked(conversation)
	return nil
}

// ClearPendingAccountSwap retires exactly the delivery obligation the caller
// completed, without allowing a stale attempt to erase a later swap.
func (i *Instance) ClearPendingAccountSwap(from, to string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingAccountSwap == nil || i.pendingAccountSwap.From != from || i.pendingAccountSwap.To != to {
		return false
	}
	i.pendingAccountSwap = nil
	i.touchLocked()
	i.accountSwapLaunch = nil
	return true
}

func (b *LocalBackend) stopForAccountSwap(i *Instance, agentAlreadyAbsent bool) error {
	i.mu.RLock()
	gw := i.gitWorktree
	tabs := append([]*Tab(nil), i.Tabs...)
	i.mu.RUnlock()
	if len(tabs) == 0 || tabs[0].tmux == nil || gw == nil || gw.GetWorktreePath() == "" {
		return fmt.Errorf("account swap: session %q has no local agent runtime", i.Title)
	}
	for idx, tab := range tabs {
		if agentAlreadyAbsent && idx == 0 && tab != nil && tab.tmux != nil && (tab.tmux.ProvenNoPane() || tab.tmux.ClosedConclusivelyAndStillAbsent()) {
			continue
		}
		if tab == nil || !tab.Kind.HasTmux() || tab.tmux == nil {
			continue
		}
		if tab.tmux.ProvenNoPane() || tab.tmux.ClosedConclusivelyAndStillAbsent() {
			continue
		}
		process := idx > 0 && tab.Kind == TabKindProcess
		if process && keepFinishedProcessPane(i, tab) {
			// A finished command with nothing left running carries no identity
			// to stop, and the swap never relaunches it (#4506 review).
			continue
		}
		state, blind, err := tab.tmux.CloseAndWaitForPaneExitReportingBlindness()
		if idx == 0 && blind {
			return fmt.Errorf("account swap: cannot stop agent tab %q for %q: %w",
				tab.Name, i.Title, errors.Join(ErrAccountSwapAgentTeardownBlind, err))
		}
		switch {
		case process && state == tmux.PaneStateKnown && err == nil && !blind:
			stampProcessTabStopped(i, tab, TabStoppedByAccountSwap)
		case process && tab.inert && state == tmux.PaneStateKnown && err == nil:
			// Blind is expected for an inert tab: restore already found its
			// session gone, and nothing respawns a process tab, so this daemon
			// never observed a pane of it. The close still ran the marked
			// survivor sweep, and a survivor it could not stop comes back
			// unknown instead (#4506 review). A process tab that was running
			// until now and vanished unobserved is not inert, and still refuses
			// below.
		case state == tmux.PaneStateKnown && blind:
			return fmt.Errorf("account swap: cannot stop credential-bearing tab %q for %q: %w", tab.Name, i.Title,
				errors.Join(ErrAccountSwapAgentTeardownBlind, err))
		case state == tmux.PaneStateUnknown:
			if errors.Is(err, tmux.ErrSessionStillAlive) {
				return fmt.Errorf("account swap: failed to stop credential-bearing tab %q for %q: %w", tab.Name, i.Title, err)
			}
			return fmt.Errorf("account swap: cannot confirm credential-bearing tab %q stopped for %q: %w", tab.Name, i.Title,
				errors.Join(ErrAccountSwapAgentTeardownBlind, err))
		case err != nil:
			return fmt.Errorf("account swap: failed to stop credential-bearing tab %q for %q: %w", tab.Name, i.Title, err)
		}
	}
	return nil
}

func (b *LocalBackend) respawnFresh(i *Instance) error {
	plan, err := i.accountSwapLaunchForRespawn()
	if err != nil {
		return err
	}
	// Idempotent, and the only copy a restarted daemon performs. A carry that
	// can no longer complete — at this copy or already at validation — leaves
	// this launch and the pending record as a stated fresh start.
	if plan, err = i.ensureAccountSwapConversationCarried(plan); err != nil {
		return err
	}
	if plan.carry != nil {
		i.markCarriedLaunchStarted(plan.account)
	}
	if err := b.respawnWithConversation(i, false, plan); err != nil {
		stopErr := b.stopForAccountSwap(i, false)
		return fmt.Errorf("account swap: replacement pane set for %q is incomplete: %w",
			i.Title, errors.Join(err, stopErr))
	}
	return nil
}

func (i *Instance) markAccountSwapReplacementPanesStarted() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pendingAccountSwap == nil || i.pendingAccountSwap.To != i.Account {
		return fmt.Errorf("account swap for %q has no committed replacement to mark started", i.Title)
	}
	if !i.pendingAccountSwap.ReplacementPanesStarted {
		i.pendingAccountSwap.ReplacementPanesStarted = true
		i.touchLocked()
	}
	return nil
}

// ValidateAccountSwapReplacementPanes requires durable proof that every pane's
// tmux start returned successfully. A process command may legitimately finish
// before this check, so later liveness is not launch proof. A crash before the
// proof is checkpointed leaves false and forces the daemon to rebuild the whole
// replacement boundary before delivering its notice.
func (i *Instance) ValidateAccountSwapReplacementPanes() error {
	i.mu.RLock()
	account := i.Account
	pending := cloneAccountSwapData(i.pendingAccountSwap)
	i.mu.RUnlock()
	if pending == nil || account != pending.To {
		return fmt.Errorf("account swap for %q has no committed replacement to validate", i.Title)
	}
	if !pending.ReplacementPanesStarted {
		return fmt.Errorf("account swap replacement for %q has no durable proof that every pane started", i.Title)
	}
	return nil
}

// SynchronizeAccountSwapRuntimeMetadata repairs the process-local launch state
// after a daemon restart. The running panes already have the selected account;
// this restores tmux's launch metadata and promotes a durable injected Claude
// id, or a carried conversation, before the pending marker can be cleared.
func (i *Instance) SynchronizeAccountSwapRuntimeMetadata() error {
	// The incoming agent is the resolved command's, not i.Program's enum: a
	// program_overrides redirect records the requested target while launching
	// the overridden command, and the live-pane answer a stopped swap lacks
	// falls back to that enum (#4430 review round 3). Resolved before the
	// instance lock — program resolution does config I/O. Inside the lock the
	// durable selection records win: they are the answers the committed
	// transaction actually froze, which re-resolution can only approximate once
	// the configuration has moved.
	resolvedAgent := HandoffEffectiveAgentForPath(i.Path, i.AgentProgram())
	i.mu.Lock()
	account := i.Account
	pending := cloneAccountSwapData(i.pendingAccountSwap)
	// Namespace preference order, most durable first: the selection record
	// (i.accountAgent) is the commit-time answer no config flip or restart can
	// move; a committed manual swap's AccountAgent predates it on records
	// written between the two fields; the pane's frozen program only survives
	// until an attach rewrites its metadata from CURRENT config; and the
	// pre-lock re-resolution is the legacy fallback for records older than all
	// of them (#4430 review round 4).
	agent := resolvedAgent
	if ts := i.tmuxLocked(); ts != nil {
		if frozen := sessionenv.AgentForCommand(ts.Program()); frozen != "" {
			agent = frozen
		}
	}
	if pending != nil && pending.AccountAgent != "" {
		agent = pending.AccountAgent
	}
	if i.accountAgent != "" {
		agent = i.accountAgent
	}
	if pending == nil || account != pending.To {
		i.mu.Unlock()
		return fmt.Errorf("account swap for %q has no committed replacement to synchronize", i.Title)
	}
	if pending.CarriedConversationID != "" {
		if err := i.synchronizeCarriedConversationLocked(agent, pending.CarriedConversationID); err != nil {
			i.mu.Unlock()
			return err
		}
	} else if pending.ConversationID != "" {
		if agent != tmux.ProgramClaude {
			i.mu.Unlock()
			return fmt.Errorf("account swap for %q has a Claude conversation id but its replacement agent is %q", i.Title, agent)
		}
		if len(i.Tabs) == 0 {
			i.mu.Unlock()
			return fmt.Errorf("account swap for %q has no agent tab to receive its replacement conversation", i.Title)
		}
		current := i.Tabs[0].Conversation
		if current.HasID() && (current.Agent != tmux.ProgramClaude || current.ID != pending.ConversationID) {
			i.mu.Unlock()
			return fmt.Errorf("account swap for %q has replacement conversation %q but the live agent records %s conversation %q",
				i.Title, pending.ConversationID, current.Agent, current.ID)
		}
		if !current.HasID() {
			i.setAgentConversationLocked(AgentConversationData{
				Agent:       tmux.ProgramClaude,
				ID:          pending.ConversationID,
				CapturedAt:  time.Now(),
				CaptureKind: ConversationCaptureInjected,
			})
		}
	}
	tabs := append([]*Tab(nil), i.Tabs...)
	i.mu.Unlock()
	passthrough := sessionEnvPassthroughForInstance(i)
	for _, tab := range tabs {
		if tab == nil || tab.tmux == nil || !tab.Kind.HasTmux() {
			continue
		}
		if err := tab.tmux.SetEnvPassthrough(passthrough); err != nil {
			return fmt.Errorf("restore account swap environment for tab %q: %w", tab.Name, err)
		}
		if err := setTabAccountEnvironment(tab, agent, account); err != nil {
			return fmt.Errorf("restore account swap environment for tab %q: %w", tab.Name, err)
		}
	}
	return nil
}

func setTabAccountEnvironment(tab *Tab, agent, account string) error {
	switch tab.Kind {
	case TabKindAgent:
		tab.tmux.SetAccountForAgent(agent, account)
	case TabKindShell:
		return tab.tmux.SetAccountShellEnvironmentForAgent(agent, account)
	default:
		tab.tmux.SetAccountEnvironmentForAgent(agent, account)
	}
	return nil
}
