package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
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
	agent                string
	alreadySet           bool
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
// means no live session currently records a limit observation for that
// agent/account; it is never promoted into a provider quota claim.
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
	resolved, err := config.ResolveConfigForInspectionFromGlobal(root, global)
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
	limited := make([]string, 0)
	for _, other := range instances {
		if other == nil || sessionenv.AgentForCommand(other.AgentProgram()) != agent {
			continue
		}
		if account, limitedNow := other.LimitAccount(); limitedNow && strings.TrimSpace(account) != "" {
			limited = append(limited, account)
		}
	}
	target, ok := quota.SelectAccountCandidate(quota.AccountSelection{
		CurrentAccount:      current,
		CurrentAutoSelected: currentAuto,
		Candidates:          resolved.LimitAccountCandidates,
		Registered:          registered,
		Limited:             limited,
	})
	if !ok {
		return nil, nil
	}
	return &autoAccountSwap{
		from:            limitedAccount,
		previousAccount: current,
		previousAuto:    currentAuto,
		to:              target,
		agent:           agent,
		alreadySet:      currentAuto && current == target && current != limitedAccount,
	}, nil
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

// prepareRuntimeForAccountSwap establishes that the old identity is gone before
// the replacement is recorded. A reachable Docker sandbox is pushed first; an
// unanswered probe refuses, while af's typed not-provisioned result makes a
// retry after an already-completed stop idempotent.
func (m *Manager) prepareRuntimeForAccountSwap(repoID, key string, instance *session.Instance) error {
	probe := probeLiveness(instance, instance.AgentServer())
	switch probe {
	case probeUnknown:
		return fmt.Errorf("cannot switch accounts for %q: its current runtime did not answer the liveness probe; not starting another identity while the old one may still be running", instance.Title)
	case probeAlive, probeAnsweredDead:
		if instance.AccountSwapReprovisionsSandbox() {
			if err := m.preserveSandboxBeforeReap(repoID, key, instance, killSuggestionFor(instance)); err != nil {
				return err
			}
			if err := requireDurableSandboxBranch(repoID, instance); err != nil {
				return err
			}
		}
		return instance.StopForAccountSwap()
	case probeAbsent:
		return instance.StopRemainingPanesForAccountSwap()
	default:
		return fmt.Errorf("cannot switch accounts for %q: unrecognized runtime state", instance.Title)
	}
}
