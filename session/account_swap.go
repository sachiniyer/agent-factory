package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// accountSwapStopper is implemented by runtimes that can conclusively stop the
// current agent while retaining the session record and workspace. The daemon
// commits the new account only after this returns, so an unanswered teardown can
// never leave a live old identity recorded as the new one.
type accountSwapStopper interface {
	stopForAccountSwap(*Instance) error
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
	if backend == nil {
		return false
	}
	typ := backend.Type()
	return typ == "local" || typ == "docker"
}

// ValidateAccountSwap checks the complete identity boundary before the current
// runtime is touched. Only local and Docker sessions can carry registered
// accounts; other off-box runtimes retain their existing refusal.
func (i *Instance) ValidateAccountSwap(name string) error {
	backend := i.currentBackend()
	i.mu.RLock()
	program := i.Program
	path := i.Path
	current := i.Account
	auto := i.accountAutoSelected
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
	if typ := backend.Type(); typ != "local" && typ != "docker" {
		return fmt.Errorf("account swapping is not supported by the %s backend", typ)
	}
	if pendingCleanup > 0 {
		return fmt.Errorf("cannot switch accounts for session %q while %d prior tab teardown(s) remain unconfirmed; restart af to retry that cleanup, then retry the account swap", i.Title, pendingCleanup)
	}
	resolvedProgram := resolveLaunchProgramForInstance(i).command
	if args := tmux.ConversationSelectorArgs(resolvedProgram); len(args) > 0 {
		return fmt.Errorf("cannot switch session %q to account %q because its resolved program pins an existing conversation with arguments %s; an account swap requires a fresh conversation, so remove those arguments and retry", i.Title, name, strings.Join(args, " "))
	}
	accountScope := sessionenv.Account{Agent: sessionenv.AgentForCommand(resolvedProgram), Name: name}
	for idx, tab := range tabs {
		if idx == 0 {
			continue
		}
		if tab == nil || !tab.Kind.HasTmux() || tab.tmux == nil {
			continue
		}
		if err := sessionenv.ValidateAccountEnvironmentCommand(tab.tmux.Program(), accountScope); err != nil {
			return fmt.Errorf("cannot switch session %q to account %q while tab %q pins another identity: %w", i.Title, name, tab.Name, err)
		}
	}
	if _, err := resolveAccountForProvision(path, program, name); err != nil {
		return fmt.Errorf("cannot select account %q for session %q: %w", name, i.Title, err)
	}
	return nil
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
	return stopper.stopForAccountSwap(i)
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
	i.pendingAccountSwap = &AccountSwapData{From: from, To: name}
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
	return true
}

// AccountSwapReprovisionsSandbox reports whether the current account swap must
// push the sandbox branch before stopping it. Docker is the only off-box runtime
// that can carry an account today.
func (i *Instance) AccountSwapReprovisionsSandbox() bool {
	backend := i.currentBackend()
	return backend != nil && backend.Type() == "docker"
}

func (b *LocalBackend) stopForAccountSwap(i *Instance) error {
	i.mu.RLock()
	gw := i.gitWorktree
	tabs := append([]*Tab(nil), i.Tabs...)
	i.mu.RUnlock()
	if len(tabs) == 0 || tabs[0].tmux == nil || gw == nil || gw.GetWorktreePath() == "" {
		return fmt.Errorf("account swap: session %q has no local agent runtime", i.Title)
	}
	var failures []error
	for _, tab := range tabs {
		if tab == nil || !tab.Kind.HasTmux() || tab.tmux == nil {
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
	return b.respawnWithConversation(i, false)
}

func refreshTabSessionEnvironment(i *Instance, tab *Tab) error {
	if err := tab.tmux.SetEnvPassthrough(sessionEnvPassthroughForInstance(i)); err != nil {
		return fmt.Errorf("invalid session environment pass-through: %w", err)
	}
	account, _ := i.AccountSelection()
	tab.tmux.SetAccountEnvironmentForAgent(sessionenv.AgentForCommand(i.AgentProgram()), account)
	return nil
}

func (b *remoteAgentBackend) stopForAccountSwap(i *Instance) error {
	return i.reapRemoteRuntimeForReplacement()
}
