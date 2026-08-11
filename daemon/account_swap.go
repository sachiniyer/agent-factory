package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// autoAccountSwap is the identity decision frozen immediately before the
// limit-resume transaction. from is the account that produced the wall (empty
// means ambient); to is an explicit configured, registered, unblocked candidate.
type autoAccountSwap struct {
	from                 string
	previousAccount      string
	previousAuto         bool
	previousConversation session.AgentConversationData
	to                   string
	candidates           []string
	agent                string
	alreadySet           bool
	fallbackDue          bool
	fellBack             bool
}

// committedAccountSwap recognizes a replacement whose identity checkpoint
// landed but whose prompt/notice transaction did not finish. It is independent
// of current config: disabling future choices cannot make a completed move
// silent, and a manual retry must finish the same notice as the scheduler.
func committedAccountSwap(instance *session.Instance) *autoAccountSwap {
	if instance == nil || !instance.SupportsAutomaticAccountSwap() {
		return nil
	}
	from, to, pending := instance.PendingAccountSwap()
	current, currentAuto := instance.AccountSelection()
	if !pending || !currentAuto || strings.TrimSpace(to) == "" || current != to {
		return nil
	}
	return &autoAccountSwap{
		from:       from,
		to:         to,
		agent:      sessionenv.AgentForCommand(instance.AgentProgram()),
		alreadySet: true,
	}
}

// accountSwapForLimit answers only from facts af actually has. "Unblocked"
// means no live wall or retained, unexpired observation records a limit for
// that agent/account; it is never promoted into a provider quota claim.
func (m *Manager) accountSwapForLimit(instance *session.Instance, global *config.Config) (*autoAccountSwap, error) {
	if instance == nil || !instance.LimitReached() || !instance.SupportsAutomaticAccountSwap() {
		return nil, nil
	}
	if committed := committedAccountSwap(instance); committed != nil {
		return committed, nil
	}
	agent := sessionenv.AgentForCommand(instance.AgentProgram())
	if _, supported := sessionenv.SupportsAccounts(agent); !supported {
		return nil, nil
	}
	current, currentAuto := instance.AccountSelection()
	limitedAccount, _ := instance.LimitAccount()
	root := instance.GetRepoPath()
	if root == "" {
		root = instance.Path
	}
	resolved, err := config.ResolveConfigForIdentityDecisionFromGlobal(root, global)
	if err != nil {
		return nil, fmt.Errorf("resolve limit account candidates for %q: %w", instance.Title, err)
	}
	if len(resolved.LimitAccountCandidates) == 0 {
		return nil, nil
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("locate registered accounts for %q: %w", instance.Title, err)
	}
	registered, err := agentaccount.List(home, agent)
	if err != nil {
		return nil, fmt.Errorf("list registered %s accounts for %q: %w", agent, instance.Title, err)
	}

	m.mu.Lock()
	instances := make([]*session.Instance, 0, len(m.instances))
	for _, other := range m.instances {
		instances = append(instances, other)
	}
	m.mu.Unlock()
	limitedSet := make(map[string]struct{})
	now := nowFunc()
	retained, err := loadAccountLimitLedger()
	if err != nil {
		return nil, fmt.Errorf("load retained account-limit evidence for %q: %w", instance.Title, err)
	}
	persisted, err := loadPersistedAccountLimitObservations()
	if err != nil {
		return nil, fmt.Errorf("load persisted account-limit evidence for %q: %w", instance.Title, err)
	}
	retained = append(retained, persisted...)
	for _, observation := range retained {
		if observation.Agent != agent || strings.TrimSpace(observation.Account) == "" {
			continue
		}
		if !observation.ResetAt.IsZero() && !now.Before(observation.ResetAt.Add(limitResumeGrace)) {
			continue
		}
		limitedSet[observation.Account] = struct{}{}
	}
	for _, other := range instances {
		if other == nil {
			continue
		}
		if sessionenv.AgentForCommand(other.AgentProgram()) == agent {
			if account, limitedNow := other.LimitAccount(); limitedNow && strings.TrimSpace(account) != "" {
				limitedSet[account] = struct{}{}
			}
		}
		for _, observation := range other.AccountLimitObservations() {
			if observation.Agent != agent || strings.TrimSpace(observation.Account) == "" {
				continue
			}
			if !observation.ResetAt.IsZero() && !now.Before(observation.ResetAt.Add(limitResumeGrace)) {
				continue
			}
			limitedSet[observation.Account] = struct{}{}
		}
	}
	limited := make([]string, 0, len(limitedSet))
	for account := range limitedSet {
		limited = append(limited, account)
	}
	targets := quota.SelectAccountCandidates(quota.AccountSelection{
		CurrentAccount:      current,
		CurrentAutoSelected: currentAuto,
		Candidates:          resolved.LimitAccountCandidates,
		Registered:          registered,
		Limited:             limited,
	})
	if len(targets) == 0 {
		return nil, nil
	}
	target := targets[0]
	return &autoAccountSwap{
		from:            limitedAccount,
		previousAccount: current,
		previousAuto:    currentAuto,
		to:              target,
		candidates:      targets,
		agent:           agent,
		alreadySet:      currentAuto && current == target && current != limitedAccount,
	}, nil
}

// preflightAccountSwapCandidates keeps candidate order but refuses to let one
// unusable account starve later configured identities. Validation also freezes
// the exact launch plan, so the selected candidate is written back to the swap
// only after that preflight succeeds.
func preflightAccountSwapCandidates(swap *autoAccountSwap, validate func(string) error) error {
	if swap == nil || swap.alreadySet {
		return nil
	}
	candidates := swap.candidates
	if len(candidates) == 0 {
		candidates = []string{swap.to}
	}
	var failures []error
	for _, candidate := range candidates {
		if err := validate(candidate); err != nil {
			failures = append(failures, fmt.Errorf("account %q: %w", candidate, err))
			continue
		}
		swap.to = candidate
		return nil
	}
	return errors.Join(failures...)
}

func accountSwapIdentity(agent, account string) string {
	if strings.TrimSpace(account) == "" {
		return "the ambient " + agent + " identity"
	}
	return fmt.Sprintf("%s account %q", agent, account)
}

func accountSwapPrompt(swap *autoAccountSwap, prompt string) string {
	notice := fmt.Sprintf(
		"[Agent Factory] This session switched from %s to %s after the previous identity reached its usage limit. "+
			"The replacement was explicitly allowed by limit_account_candidates and had no current limit observation. "+
			"Continue the same task under the new identity.",
		accountSwapIdentity(swap.agent, swap.from), accountSwapIdentity(swap.agent, swap.to))
	if strings.TrimSpace(prompt) == "" {
		return notice + "\n\ncontinue"
	}
	return notice + "\n\n" + strings.TrimSpace(prompt)
}

// captureAccountSwapConversation binds Codex discovery to the replacement
// runtime while the limit-resume operation still owns its fence. Account swaps
// cannot use the ordinary asynchronous capture: that goroutine serializes its
// write through the same per-session operation lock held by the caller, so the
// pending recovery marker could otherwise be cleared and checkpointed before
// the conversation id became durable.
func captureAccountSwapConversation(instance *session.Instance, snap session.ConversationCaptureSnapshot) error {
	token := instance.AgentRuntimeToken()
	if token.Agent() != tmux.ProgramCodex {
		return nil
	}
	conversation, err := session.CaptureAgentConversation(token.Agent(), snap, conversationCaptureTimeout)
	if err != nil {
		return fmt.Errorf("capture replacement Codex conversation: %w", err)
	}
	if !conversation.HasID() {
		return errors.New("replacement Codex runtime did not expose a conversation id")
	}
	if !instance.SetAgentConversationForRuntime(token, conversation) {
		return errors.New("replacement Codex runtime changed before its conversation id could be recorded")
	}
	return nil
}

// prepareRuntimeForAccountSwap establishes that every old local pane is gone
// before the replacement is recorded. An unanswered probe refuses, while an
// absent agent still triggers a sibling-pane recheck for retry safety.
func (m *Manager) prepareRuntimeForAccountSwap(repoID, key string, instance *session.Instance) error {
	probe := probeLiveness(instance, instance.AgentServer())
	if probe == probeUnknown {
		return fmt.Errorf("cannot switch accounts for %q: its current runtime did not answer the liveness probe; not starting another identity while the old one may still be running", instance.Title)
	}
	if err := m.stopVSCodeForAccountSwap(key, instance); err != nil {
		return err
	}
	switch probe {
	case probeAlive, probeAnsweredDead:
		return instance.StopForAccountSwap()
	case probeAbsent:
		return instance.StopRemainingPanesForAccountSwap()
	default:
		return fmt.Errorf("cannot switch accounts for %q: unrecognized runtime state", instance.Title)
	}
}

// stopVSCodeForAccountSwap brings the daemon-owned editor into the same
// credential boundary as every tmux pane. It must be confirmed gone before the
// account commit; a later render relaunches it from the selected account env.
func (m *Manager) stopVSCodeForAccountSwap(key string, instance *session.Instance) error {
	if !instanceHasVSCodeTab(instance) {
		return nil
	}
	if m.vscode == nil {
		return fmt.Errorf("cannot switch accounts for %q: daemon has no VS Code supervisor", instance.Title)
	}
	if err := m.stopVSCodeForInstance(key, instance.ID); err != nil {
		return fmt.Errorf("cannot switch accounts for %q: cannot confirm its VS Code editor stopped: %w", instance.Title, err)
	}
	return nil
}
