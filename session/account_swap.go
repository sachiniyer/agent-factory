package session

import (
	"errors"
	"fmt"
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
	account string
	base    string
	// runtimeBase is the clean base command this plan launches, frozen at
	// preflight, for RuntimeProgram: the base without af's injected flags
	// (base can be the pane's frozen program, which carries them), and never
	// re-resolved from configuration at the respawn boundary — a
	// program_overrides edit between preflight and respawn must not make
	// RuntimeProgram claim a command that never ran (#6049 review). It is the
	// launched base itself, or that base minus af's declared flags; empty only
	// when the removal cannot be proven, and nothing is then recorded.
	runtimeBase         string
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
	// passthrough is the session-environment list admission resolved. The
	// respawn applies it after the outgoing panes stopped instead of re-reading
	// config there (#7077); passthroughFrozen marks a plan admission built.
	passthrough       []string
	passthroughFrozen bool
}

func cloneAccountSwapLaunchPlan(plan *accountSwapLaunchPlan) *accountSwapLaunchPlan {
	if plan == nil {
		return nil
	}
	copy := *plan
	copy.proof.GeneratedArgs = append([]string(nil), plan.proof.GeneratedArgs...)
	copy.conversationCapture = cloneConversationCaptureSnapshot(plan.conversationCapture)
	copy.carry = plan.carry.clone()
	copy.passthrough = append([]string(nil), plan.passthrough...)
	return &copy
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
// credential boundary without changing backend kind: a local session, or an ssh or
// sandbox session on a listed host whose agent-server rotates its account scope in
// place (#5571 W7.6).
func (i *Instance) SupportsAutomaticAccountSwap() bool {
	backend := i.currentBackend()
	if backend == nil {
		return false
	}
	if backend.Type() == "local" {
		return true
	}
	return handoffInPlaceBackend(backend.Type()) && i.offBoxAccountSwapAvailable()
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
	// The ids held outside the tab belong to the account being left too (#6189).
	i.retireConversationsLocked()
	// Invalidate asynchronous discovery attached to the stopped process before it
	// can put that process's account-local conversation back into the live slot.
	i.agentRuntimeGeneration++
	i.retireLimitWallShadowOnRuntimeReplaceLocked()
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
		// The committed incoming command is a matter of record too: a restart
		// between this checkpoint and the replacement's first launch leaves
		// pane/runtime evidence describing the OUTGOING agent, and a retry
		// that froze that command would fail the drift check against the
		// committed namespace forever (#4430 review round 7).
		pending.Program = plan.base
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
	i.pendingAccountSwapUnwritten = false
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
		i.pendingAccountSwapUnwritten = false
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
	i.stampHandoffConversationLocked()
	i.pendingAccountSwap = nil
	i.pendingAccountSwapUnwritten = false
	// The notice was just delivered, so the agent's new work ends the run on
	// its own idle edge — see ClearPendingHandoffMission for why a held edge
	// must not outlive a resend.
	i.taskRunIdleEdgeHeld = false
	i.taskRunTurnGateHeld = false
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

// durableAccountSwapAgentLocked is the account namespace a durable record
// names, most durable first: the selection record (i.accountAgent) is the
// commit-time answer no config flip or restart can move; a committed manual
// swap's AccountAgent predates it on records written between the two fields;
// the pane's frozen program only survives until an attach rewrites its metadata
// from CURRENT config (#4430 review round 4). false: only a re-resolution, the
// legacy fallback for records older than all of them, can answer.
func (i *Instance) durableAccountSwapAgentLocked() (string, bool) {
	if i.accountAgent != "" {
		return i.accountAgent, true
	}
	if p := i.pendingAccountSwap; p != nil && p.AccountAgent != "" {
		return p.AccountAgent, true
	}
	if ts := i.tmuxLocked(); ts != nil {
		if frozen := sessionenv.AgentForCommand(ts.Program()); frozen != "" {
			return frozen, true
		}
	}
	return "", false
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
	//
	// The re-resolution is the LAST resort, so it runs only when no durable
	// record names the namespace: such a record needs no config read, and must
	// neither wait on nor be refused by one that cannot complete (#7077).
	i.mu.RLock()
	_, durable := i.durableAccountSwapAgentLocked()
	i.mu.RUnlock()
	var resolvedAgent string
	var resolveErr error
	if !durable {
		resolvedAgent, resolveErr = HandoffEffectiveAgentForPath(i.Path, i.AgentProgram())
	}
	i.mu.Lock()
	account := i.Account
	pending := cloneAccountSwapData(i.pendingAccountSwap)
	agent, durable := i.durableAccountSwapAgentLocked()
	if !durable {
		if resolveErr != nil {
			i.mu.Unlock()
			return fmt.Errorf("account swap for %q: %w", i.Title, resolveErr)
		}
		agent = resolvedAgent
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
	passthrough, err := SessionEnvPassthroughForInstance(i)
	if err != nil {
		return fmt.Errorf("restore account swap environment for %q: %w", i.Title, err)
	}
	for _, tab := range tabs {
		if tab == nil || tab.tmux == nil || !tab.Kind.HasTmux() {
			continue
		}
		if err := tab.tmux.SetEnvPassthrough(passthrough); err != nil {
			return fmt.Errorf("restore account swap environment for tab %q: %w", tab.Name, err)
		}
		if err := tab.tmux.SetEnvEmpty(sessionEnvEmptyForInstance(i)); err != nil {
			return fmt.Errorf("restore account swap empty-pin names for tab %q: %w", tab.Name, err)
		}
		if err := setTabAccountEnvironment(tab, agent, account); err != nil {
			return fmt.Errorf("restore account swap environment for tab %q: %w", tab.Name, err)
		}
	}
	i.markSwapEnvironmentResolved(tabs, passthrough)
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
