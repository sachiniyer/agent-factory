package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"

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
// Three cases keep the old answer exactly: an explicit --account, an explicit
// AccountAmbient (a picker that offers "the ambient identity" must mean it —
// routing that request onto the pool silently re-identifies a session the user
// chose to keep OFF it, and an ambient pick outranks a configured default for
// the same reason an explicit --account does), and a create whose backend
// cannot carry an account (ssh/hook/sandbox — NewInstance refuses an account
// there, so routing one would break a previously-working create). For the
// first and last, the configured default still applies and refuses by name as
// before.
func (m *Manager) routeCreateAccount(cfg *config.Config, req *CreateSessionRequest) error {
	if strings.TrimSpace(req.Account) != "" || req.allowReserved || req.AccountAmbient {
		return nil
	}
	agent := sessionenv.AgentForCommand(req.Program)
	if agent == "" {
		return nil
	}
	// Selection and opt-out come from ONE resolved configuration generation:
	// resolving them separately could pair a default read from one config save
	// with an ambient refusal read from the next (#4404 review).
	selection, ambientOptOut := config.DefaultAccountPolicyFor(cfg, req.RepoPath, agent)

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
	if sessionenv.AgentForCommand(resolvedCreateProgram(cfg, req.RepoPath, req.Program)) != agent {
		// program_overrides redirects this label to a different agent's command —
		// or to no agent's command at all, as a fixture shim does. The launch
		// boundary refuses ANY account whose validation namespace disagrees with
		// the resolved command, so there is no account this create could carry.
		// Route nothing; the configured default still applies and refuses by
		// name, which is the same outcome a pinned --account gets.
		return applyResolvedDefaultAccount(req, selection)
	}
	if ambientOptOut {
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
	// An automatic pick may only land on an account the agent can actually
	// launch as — one with a completed login. Registration alone is a directory
	// name: routing onto an account with no credential in it would put the new
	// session on an identity that cannot authenticate, silently, in place of
	// the ambient login that always worked. A configured default is the one
	// exception: it is the user's own pin (the same contract --account
	// carries), and its picker rows already say "runs as it until you log in",
	// so it stays a candidate the way an explicit choice would.
	pool := make([]string, 0, len(registered))
	for _, name := range registered {
		loggedIn, lerr := agentaccount.LoggedIn(home, agent, name)
		switch {
		case lerr != nil:
			m.warn().Printf("cannot verify the %s credential for account %q — leaving it out of automatic routing: %v",
				agent, name, lerr)
		case loggedIn:
			pool = append(pool, name)
		}
	}
	if selection.Name != "" && !slices.Contains(pool, selection.Name) {
		pool = append(pool, selection.Name)
	}
	if len(pool) == 0 {
		// Every registered account is missing its credential, so no automatic
		// pick can produce a working session — and a configured default would
		// have been refused above or sits in the pool, so ambient is the only
		// identity left.
		return nil
	}
	// The evidence scan and the claim must publish as ONE observation against
	// account-limit updates: a status tick's refute or a startup's retained
	// load may be writing fresh wall evidence under accountLimitMu while this
	// route reads it, and a claim laid down between an evidence snapshot and
	// its use routes onto a wall the swap scheduler would never pick (#4404
	// review). accountLimitMu is the same fence swap admission and the refute
	// take; taking it first keeps the established accountLimitMu → m.mu order
	// — both accountLimitEvidenceForSwap and claimCreateAccount acquire m.mu
	// inside.
	m.accountLimitMu.Lock()
	limited, err := m.accountLimitEvidenceForSwap(agent, loadAccountLimitEvidenceForSwap)
	if err != nil {
		m.accountLimitMu.Unlock()
		return fmt.Errorf("cannot route %q to a healthy %s account: %w", req.Title, agent, err)
	}
	chosen, claim, err := m.claimCreateAccount(agent, pool, limited, selection.Name)
	m.accountLimitMu.Unlock()
	if err != nil {
		return err
	}
	if selection.Name != "" && chosen != selection.Name {
		m.info().Printf("create %q: default_accounts.%s = %q has unexpired limit evidence; routing to %q",
			req.Title, agent, selection.Name, chosen)
	}
	req.Account = chosen
	req.accountAutoSelected = true
	req.routedAccountClaim = claim
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

// resolvedCreateProgram answers the command a create's program label will
// actually launch, resolved through the same repo-over-global program_overrides
// layering the launch boundary reads (session.refuseUnsupportedAccountAgent).
// An unresolvable repo falls back to the op-entry global snapshot, which
// resolves to the label unchanged when it names no override either — so the
// router and the boundary cannot disagree about which agent the command runs.
func resolvedCreateProgram(cfg *config.Config, repoPath, program string) string {
	if repo, err := config.RepoFromPath(repoPath); err == nil {
		if resolved, rerr := config.ResolveConfigForRepoInspectionWithGlobal(repo, cfg); rerr == nil {
			return config.ResolveProgram(&resolved.Config, program)
		}
	}
	return config.ResolveProgram(cfg, program)
}

// claimCreateAccount picks this create's account and reserves the pick in the
// SAME m.mu critical section that computed the loads — the fix for the
// read-then-pick gap the router opened (#4404 review). Before this, loads were
// a snapshot: two near-simultaneous creates could both read "least loaded" for
// the same account and both take it, because the pending-create projection that
// would have counted the first pick was not published until long after. The
// claim stands in for that row until CreateSession publishes it — the row then
// carries the account itself, and the claim hands the count off under one lock
// — or until the create fails before publishing and releases it.
func (m *Manager) claimCreateAccount(agent string, pool []string, limited map[string]time.Time, preferred string) (chosen, claim string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	loads := m.accountSessionLoadsLocked(agent, pool)
	chosen, err = quota.RouteAccountPool(agent, pool, limited, preferred, loads)
	if err != nil {
		return "", "", err
	}
	claim = agent + "\x00" + chosen
	if m.createAccountClaims == nil {
		m.createAccountClaims = make(map[string]int)
	}
	m.createAccountClaims[claim]++
	return chosen, claim, nil
}

// releaseCreateAccountClaim drops one outstanding router claim — called by
// CreateSession when the create exits before its pending row could take over
// the count.
func (m *Manager) releaseCreateAccountClaim(claim string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseCreateAccountClaimLocked(claim)
}

// releaseCreateAccountClaimLocked retires one claim. The pending-row handoff
// calls it inside the SAME m.mu section that publishes the row, so no load
// snapshot can observe the assignment counted twice or not at all. Caller holds
// m.mu.
func (m *Manager) releaseCreateAccountClaimLocked(claim string) {
	if n := m.createAccountClaims[claim]; n > 1 {
		m.createAccountClaims[claim] = n - 1
		return
	}
	delete(m.createAccountClaims, claim)
}

// accountSessionLoadsLocked counts the sessions currently holding each
// candidate account, so RouteAccountPool can spread new work across the pool.
// Live instances, in-flight creates AND outstanding router claims all count —
// a burst of near-simultaneous creates must not all pile onto the same
// least-loaded answer. Caller holds m.mu.
func (m *Manager) accountSessionLoadsLocked(agent string, registered []string) map[string]int {
	candidate := make(map[string]bool, len(registered))
	for _, name := range registered {
		candidate[name] = true
	}
	loads := make(map[string]int, len(registered))
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
	for claim, n := range m.createAccountClaims {
		claimAgent, account, ok := strings.Cut(claim, "\x00")
		if !ok || claimAgent != agent || !candidate[account] {
			continue
		}
		loads[account] += n
	}
	return loads
}
