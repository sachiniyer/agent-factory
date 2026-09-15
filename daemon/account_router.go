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

// The create-time account router (#4404).
//
// Before this, a session with no --account launched under the ambient identity
// (or a default_accounts preference), and several codex accounts hitting their
// usage limits stalled sessions one pinned identity at a time. Now a create on
// a backend that carries accounts launches under the pool: the registered
// accounts minus every one with unexpired limit evidence, preferring the
// configured default while it is healthy and otherwise the least-loaded healthy
// account. When the whole pool is walled the create refuses loudly with the
// earliest known reset — launching into a known wall is worse than refusing.
//
// The pieces are shared, deliberately. "Which accounts are currently walled" is
// accountLimitEvidenceForSwap, the same map the limit-swap scheduler consults,
// so a session can never be routed onto an account the swapper would avoid.
// The pick itself is quota.RouteAccountPool, a pure function the tests drive.
// And an explicit --account still means exactly what it meant: the router
// returns before reading anything, so a pin is never second-guessed.

// routeCreateAccount decides the identity a session launches under when the
// request named none. It runs where applyDefaultAccount ran — after the program
// is resolved and before reserveCreate — so a refusal still costs no worktree,
// branch or tmux session, and every surface (TUI, web, CLI, task deliveries)
// gets the same routing with no re-implementation.
//
// Two cases keep the old answer exactly: an explicit --account, and a create
// whose backend cannot carry an account (ssh/hook/sandbox — NewInstance refuses
// an account there, so routing one would break a previously-working create).
// For both, the configured default still applies and refuses by name as before.
func (m *Manager) routeCreateAccount(cfg *config.Config, req *CreateSessionRequest) error {
	if strings.TrimSpace(req.Account) != "" || req.allowReserved {
		return nil
	}
	agent := sessionenv.AgentForCommand(req.Program)
	if agent == "" {
		return nil
	}
	selection := defaultAccountSelectionFor(cfg, req.RepoPath, agent)

	kind, kindErr := session.BackendKindFor(session.InstanceOptions{
		Backend:     session.BackendKind(req.Backend),
		ForceRemote: req.ForceRemote,
		InPlace:     req.InPlace,
	}, req.RepoPath)
	if _, supported := sessionenv.SupportsAccounts(agent); !supported ||
		kindErr != nil || (kind != session.BackendLocal && !kind.CarriesAccount()) {
		// Not routable: either the agent has no account registry at all, or the
		// backend an account would land on cannot carry it — including a backend
		// value that will not resolve, whose refusal NewInstance owns. The
		// configured default still applies and refuses exactly as before.
		return applyResolvedDefaultAccount(req, selection)
	}
	if config.DefaultAccountAmbientOptOut(cfg, req.RepoPath, agent) {
		return nil
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("cannot route %q to a %s account: af cannot resolve its agent-factory home: %w",
			req.Title, agent, err)
	}
	// A configured default that names nothing registered refuses by name even
	// when healthy siblings exist — the user asked for a specific identity, and
	// silently routing around the typo would hide it.
	if err := config.CheckDefaultAccount(home, req.RepoPath, selection); err != nil {
		return err
	}
	registered, err := agentaccount.List(home, agent)
	if err != nil {
		return fmt.Errorf("cannot route %q: the %s account registry could not be read: %w",
			req.Title, agent, err)
	}
	if len(registered) == 0 {
		// No pool to route across — and a configured default would have been
		// refused above, so ambient is the only identity left.
		return nil
	}
	limited, err := m.accountLimitEvidenceForSwap(agent, loadAccountLimitEvidenceForSwap)
	if err != nil {
		return fmt.Errorf("cannot route %q to a healthy %s account: %w", req.Title, agent, err)
	}
	loads := m.accountSessionLoads(agent, registered)
	chosen, err := quota.RouteAccountPool(agent, registered, limited, selection.Name, loads)
	if err != nil {
		return err
	}
	if selection.Name != "" && chosen != selection.Name {
		m.info().Printf("create %q: default_accounts.%s = %q has unexpired limit evidence; routing to %q",
			req.Title, agent, selection.Name, chosen)
	}
	req.Account = chosen
	req.accountAutoSelected = true
	if chosen == selection.Name {
		req.AccountSource = defaultAccountProvenance(selection, req.RepoPath)
	} else {
		req.AccountSource = fmt.Sprintf(
			"this session's account was not requested — af's account router picked the least-used %s account "+
				"with no current usage-limit evidence; pass --account to pin a specific identity",
			agent)
	}
	return nil
}

// accountSessionLoads counts the sessions currently holding each candidate
// account, so RouteAccountPool can spread new work across the pool. Live
// instances and in-flight creates both count — a burst of near-simultaneous
// creates must not all pile onto the same least-loaded answer.
func (m *Manager) accountSessionLoads(agent string, registered []string) map[string]int {
	candidate := make(map[string]bool, len(registered))
	for _, name := range registered {
		candidate[name] = true
	}
	loads := make(map[string]int, len(registered))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		if inst == nil || inst.IsArchived() {
			continue
		}
		account, _ := inst.AccountSelection()
		if account == "" || !candidate[account] {
			continue
		}
		if sessionenv.AgentForCommand(inst.AgentProgram()) != agent {
			continue
		}
		loads[account]++
	}
	for _, pending := range m.pendingCreates {
		if pending.Account == "" || !candidate[pending.Account] {
			continue
		}
		if sessionenv.AgentForCommand(pending.Program) != agent {
			continue
		}
		loads[pending.Account]++
	}
	return loads
}
