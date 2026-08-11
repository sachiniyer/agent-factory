package session

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
}

func cloneAccountSwapLaunchPlan(plan *accountSwapLaunchPlan) *accountSwapLaunchPlan {
	if plan == nil {
		return nil
	}
	copy := *plan
	copy.proof.GeneratedArgs = append([]string(nil), plan.proof.GeneratedArgs...)
	copy.conversationCapture = cloneConversationCaptureSnapshot(plan.conversationCapture)
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
	program = injectSystemPrompt(program)
	return program, accountLaunchProof(declarationBase, program, trustBase)
}

// AccountSelection reports the current account and whether af selected it. A
// non-empty account with auto=false is an explicit --account pin.
func (i *Instance) AccountSelection() (account string, auto bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.Account, i.accountAutoSelected
}

func cloneAccountSwapData(data *AccountSwapData) *AccountSwapData {
	if data == nil {
		return nil
	}
	copy := *data
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
	backend := i.currentBackend()
	i.mu.RLock()
	program := i.Program
	path := i.Path
	current := i.Account
	auto := i.accountAutoSelected
	pending := cloneAccountSwapData(i.pendingAccountSwap)
	op := i.inFlightOp
	pendingCleanup := len(i.pendingTabCleanup)
	tabs := append([]*Tab(nil), i.Tabs...)
	i.mu.RUnlock()
	if op != OpRespawning {
		return fmt.Errorf("account swap for %q requires the limit-resume fence", i.Title)
	}
	if strings.TrimSpace(current) != "" && !auto {
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
	resolvedProgram := resolution.command
	if args := tmux.ConversationSelectorArgs(resolvedProgram); len(args) > 0 {
		return fmt.Errorf("cannot switch session %q to account %q because its resolved program pins an existing conversation with arguments %s; an account swap requires a fresh conversation, so remove those arguments and retry", i.Title, name, strings.Join(args, " "))
	}
	conversationID := newSessionID()
	if pending != nil && pending.To == name && pending.ConversationID != "" {
		conversationID = pending.ConversationID
	}
	launchProgram, conversation := planLaunchConversation(conversationID, resolvedProgram)
	launchProgram = injectSystemPrompt(launchProgram)
	proof := accountLaunchProof(resolvedProgram, launchProgram, resolution.trustBase)
	if err := tmux.ValidateAccountLaunchSupport(name); err != nil {
		return fmt.Errorf("cannot switch session %q to account %q: %w", i.Title, name, err)
	}
	accountScope, err := resolveAccountForProvision(path, program, name)
	if err != nil {
		return fmt.Errorf("cannot select account %q for session %q: %w", name, i.Title, err)
	}
	if err := refuseAccountAgentDrift(name, accountScope.Agent, launchProgram); err != nil {
		return fmt.Errorf("cannot switch session %q to account %q: %w", i.Title, name, err)
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
		if tab == nil || !tab.Kind.HasTmux() || tab.tmux == nil {
			continue
		}
		if args := tmux.ConversationSelectorArgs(tab.tmux.Program()); len(args) > 0 {
			return fmt.Errorf("cannot switch session %q to account %q because tab %q pins an existing conversation with arguments %s; an account swap restarts that tab under a separate conversation store", i.Title, name, tab.Name, strings.Join(args, " "))
		}
		if err := sessionenv.ValidateAccountEnvironmentCommand(tab.tmux.Program(), accountScope); err != nil {
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
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return fmt.Errorf("account swap for %q lost the limit-resume fence during launch preflight", i.Title)
	}
	i.accountSwapLaunch = &accountSwapLaunchPlan{
		account: name, base: resolvedProgram, program: launchProgram,
		proof: proof, conversation: conversation, conversationCapture: conversationCapture,
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

// StopRemainingPanesForAccountSwap rechecks every local sibling when the agent
// probe already established that tab zero is absent. A retry must not promote
// that one absence into proof that every credential-bearing pane is gone.
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
	i.mu.Lock()
	defer i.mu.Unlock()
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
	i.Account = name
	i.accountAutoSelected = true
	pending := &AccountSwapData{From: from, To: name}
	if plan := i.accountSwapLaunch; plan != nil && plan.account == name && plan.conversation.HasID() {
		pending.ConversationID = plan.conversation.ID
	}
	i.pendingAccountSwap = pending
	return previous, nil
}

// RestoreAccountSelectionUnderResumeFence rolls back an in-memory selection
// whose durable checkpoint failed. The stopped old runtime is not restarted;
// the next scheduler pass retries from the still-durable previous identity.
func (i *Instance) RestoreAccountSelectionUnderResumeFence(name string, auto bool, conversation AgentConversationData) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.inFlightOp != OpRespawning {
		return fmt.Errorf("restoring account for %q requires the limit-resume fence", i.Title)
	}
	i.Account = name
	i.accountAutoSelected = auto
	i.pendingAccountSwap = nil
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
	var failures []error
	for idx, tab := range tabs {
		if agentAlreadyAbsent && idx == 0 {
			continue
		}
		if tab == nil || !tab.Kind.HasTmux() || tab.tmux == nil {
			continue
		}
		if tab.tmux.ProvenNoPane() {
			continue
		}
		state, blind, err := tab.tmux.CloseAndWaitForPaneExitReportingBlindness()
		switch {
		case state == tmux.PaneStateKnown && blind:
			failures = append(failures, fmt.Errorf("tab %q vanished without its pane being observed; a detached child may still be writing the worktree", tab.Name))
		case state == tmux.PaneStateUnknown:
			failures = append(failures, fmt.Errorf("cannot confirm tab %q stopped: %w", tab.Name, err))
		case err != nil:
			failures = append(failures, fmt.Errorf("failed to stop tab %q: %w", tab.Name, err))
		}
	}
	if err := errors.Join(failures...); err != nil {
		return fmt.Errorf("account swap: cannot stop every credential-bearing pane for %q: %w", i.Title, err)
	}
	return nil
}

func (b *LocalBackend) respawnFresh(i *Instance) error {
	plan, err := i.accountSwapLaunchForRespawn()
	if err != nil {
		return err
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
	i.pendingAccountSwap.ReplacementPanesStarted = true
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
// id before the pending marker can be cleared.
func (i *Instance) SynchronizeAccountSwapRuntimeMetadata() error {
	i.mu.Lock()
	account := i.Account
	pending := cloneAccountSwapData(i.pendingAccountSwap)
	if pending == nil || account != pending.To {
		i.mu.Unlock()
		return fmt.Errorf("account swap for %q has no committed replacement to synchronize", i.Title)
	}
	agent := i.currentAgentNameLocked()
	if pending.ConversationID != "" {
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

func refreshTabSessionEnvironment(i *Instance, tab *Tab) error {
	if err := tab.tmux.SetEnvPassthrough(sessionEnvPassthroughForInstance(i)); err != nil {
		return fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	account, _ := i.AccountSelection()
	agent := sessionenv.AgentForCommand(i.AgentProgram())
	if err := setTabAccountEnvironment(tab, agent, account); err != nil {
		return fmt.Errorf("prepare account-scoped shell: %w", err)
	}
	return nil
}
