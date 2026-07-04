package daemon

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Root-agent always-ensure (#1106): for every repo opted in via the
// root_agents config key, the daemon guarantees a reserved session titled
// "root" attached in-place at the repo root (the `af sessions create --here`
// shape from #1107 — worktree_path == repo_path, current branch, cleanup
// never touches the user's tree). The poll loop calls EnsureRootAgents right
// after RefreshStatuses, so a root whose tmux died is marked Dead and healed
// in the same tick.
//
// The loop is adopt-first: an existing root instance in any state other than
// Dead — whatever program it runs and however it was created — is left
// completely alone. Only a Dead root (tmux vanished) or a missing one
// triggers a (re-)create, and an explicit KillSession of the root suppresses
// re-creation until the daemon restarts (see Manager.KillSession).

// rootDangerouslySkipPermissionsFlag is ensured on the default root-agent
// program: the root agent exists to act autonomously (issue #1106's
// root-agent profile).
const rootDangerouslySkipPermissionsFlag = "--dangerously-skip-permissions"

// rootEnsureEscalationThreshold is the consecutive-failure count at which the
// ensure loop escalates to a one-time ERROR log: the cause now looks
// persistent (a deleted repo path, an unparseable persisted root record), not
// transient. The loop never stops retrying — it settles at the
// rootEnsureBackoffMax cadence instead. A permanent give-up here is what kept
// root agents down for hours after the 2026-07-03 tmux-server outage: the
// outage outlasted the six fast attempts, and recovery then depended on a
// daemon restart. Any outage that ends must heal on the next retry, whatever
// the failure looked like while it lasted (#1122).
const rootEnsureEscalationThreshold = 6

// Backoff between failed ensure attempts for one repo: base doubles per
// consecutive failure, capped at max. Package vars so tests can shorten them.
var (
	rootEnsureBackoffBase = 10 * time.Second
	rootEnsureBackoffMax  = 5 * time.Minute
)

// rootEnsureState is the per-configured-repo retry state for the ensure
// loop. Guarded by Manager.mu (the loop runs on the daemon poll goroutine,
// but tests drive EnsureRootAgents directly).
type rootEnsureState struct {
	consecutiveFailures int
	nextAttempt         time.Time
	// suppressLogged dedupes the "not re-creating a user-killed root" log
	// line to once per suppression.
	suppressLogged bool
}

// EnsureRootAgents runs one ensure pass over every repo configured in
// root_agents. Called from the daemon poll loop after RefreshStatuses; a
// no-op when nothing is configured or the initial restore has not finished.
func (m *Manager) EnsureRootAgents() {
	if len(m.cfg.RootAgents) == 0 || !m.Ready() {
		return
	}
	paths := make([]string, 0, len(m.cfg.RootAgents))
	for path := range m.cfg.RootAgents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		m.ensureRootAgent(path, m.cfg.RootAgents[path])
	}
}

// ensureRootAgent guarantees the root agent for one configured repo path:
// adopt a live root untouched, heal a Dead/Lost one in place, create a missing
// one, and respect an explicit user kill. All outcomes are logged; failures
// back off exponentially and settle at the rootEnsureBackoffMax cadence, so
// the loop always heals eventually once the cause clears.
func (m *Manager) ensureRootAgent(path string, rc config.RootAgentConfig) {
	m.mu.Lock()
	st := m.rootEnsureStates[path]
	if st == nil {
		st = &rootEnsureState{}
		m.rootEnsureStates[path] = st
	}
	skip := time.Now().Before(st.nextAttempt)
	m.mu.Unlock()
	if skip {
		return
	}

	repo, err := config.RepoFromPath(config.ExpandTilde(path))
	if err != nil {
		m.rootEnsureFailed(path, st, fmt.Errorf("root_agents entry %q does not resolve to a git repository: %w", path, err))
		return
	}

	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	m.mu.Lock()
	_, killed := m.rootKilledByUser[repo.ID]
	inst := m.instances[key]
	m.mu.Unlock()

	if killed {
		m.mu.Lock()
		logSuppression := !st.suppressLogged
		st.suppressLogged = true
		m.mu.Unlock()
		if logSuppression {
			log.InfoLog.Printf("root agent for %s was explicitly killed; not re-creating until the daemon restarts", repo.Root)
		}
		return
	}

	if inst != nil {
		if status := inst.GetStatus(); status != session.Dead && status != session.Lost {
			// Adopt, never clobber: a live root — whatever program it runs
			// and whoever created it — is the root agent. Nothing to do.
			m.rootEnsureSucceeded(st)
			return
		}
		// The root's tmux vanished (crash, tmux server death — the #1104
		// outage class; recorded as Lost since #1108, Dead by older builds).
		// Reap the dead record and fall through to re-create in place — the
		// root keeps its stronger always-ensure semantics rather than waiting
		// for the general Lost-restore loop. Kill is best-effort teardown of
		// already-dead tmux, and an in-place worktree's Cleanup never touches
		// the user's tree (#1107), so this can only remove daemon-owned state.
		log.WarningLog.Printf("root agent for %s is gone (tmux vanished); re-creating it in place", repo.Root)
		if err := m.reapDeadRoot(repo.ID, inst); err != nil {
			m.rootEnsureFailed(path, st, fmt.Errorf("failed to remove dead root record: %w", err))
			return
		}
	}

	program := rootAgentProgram(repo.Root, rc)
	if _, err := m.CreateSession(CreateSessionRequest{
		Title:         session.RootSessionTitle,
		RepoPath:      repo.Root,
		Program:       program,
		AutoYes:       rc.AutoYesEnabled(),
		InPlace:       true,
		allowReserved: true,
	}); err != nil {
		m.rootEnsureFailed(path, st, fmt.Errorf("failed to create root session: %w", err))
		return
	}
	log.InfoLog.Printf("ensured root agent for %s (in-place, program %q, auto_yes %t)", repo.Root, program, rc.AutoYesEnabled())
	m.rootEnsureSucceeded(st)
}

// reapDeadRoot removes a Dead root instance so ensureRootAgent can re-create
// the title. Mirrors KillSession's teardown but deliberately does NOT mark
// rootKilledByUser: this is the daemon healing itself, not a user decision.
func (m *Manager) reapDeadRoot(repoID string, inst *session.Instance) error {
	// Best-effort by design (#478): tmux is already gone and an in-place
	// worktree's Cleanup is a no-op, so failures here only log inside Kill.
	if err := inst.Kill(); err != nil {
		log.WarningLog.Printf("reaping dead root for repo %s: kill reported: %v", repoID, err)
	}
	storage, err := session.NewStorage(config.LoadState(), repoID)
	if err != nil {
		return err
	}
	if err := storage.DeleteInstance(session.RootSessionTitle); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.instances, daemonInstanceKey(repoID, session.RootSessionTitle))
	m.mu.Unlock()
	return nil
}

// rootEnsureSucceeded resets a repo's retry state after a pass that left a
// healthy root in place (freshly created or adopted).
func (m *Manager) rootEnsureSucceeded(st *rootEnsureState) {
	m.mu.Lock()
	st.consecutiveFailures = 0
	st.nextAttempt = time.Time{}
	st.suppressLogged = false
	m.mu.Unlock()
}

// rootEnsureFailed records a failed ensure attempt: exponential backoff up to
// rootEnsureBackoffMax, where the retry cadence stays for as long as the
// failures do. Retrying forever (instead of giving up until restart) is what
// guarantees a root heals after a tmux-server outage of any length — an
// outage is indistinguishable from a broken config while it lasts, and only
// a later retry can tell the difference (#1122). The cost for a genuinely
// broken config is one cheap failed attempt per cadence interval, each
// logged. Crossing rootEnsureEscalationThreshold logs one ERROR so a
// persistent cause is visible without waiting for a user to notice the
// missing root.
func (m *Manager) rootEnsureFailed(path string, st *rootEnsureState, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st.consecutiveFailures++
	backoff := rootEnsureBackoffMax
	// Guard the shift: past ~16 doublings the exponential form has no
	// meaning and would overflow.
	if shift := st.consecutiveFailures - 1; shift < 16 {
		if b := rootEnsureBackoffBase << shift; b < backoff {
			backoff = b
		}
	}
	st.nextAttempt = time.Now().Add(backoff)
	if st.consecutiveFailures == rootEnsureEscalationThreshold {
		log.ErrorLog.Printf("root agent ensure for %q failed %d consecutive times; the cause looks persistent — will keep retrying every %s: %v", path, st.consecutiveFailures, rootEnsureBackoffMax, err)
		return
	}
	log.WarningLog.Printf("root agent ensure for %q failed (attempt %d), retrying in %s: %v", path, st.consecutiveFailures, backoff, err)
}

// rootAgentProgram resolves the command the root agent runs. An explicit
// per-repo program wins verbatim (an agent enum name still resolves through
// program_overrides downstream, exactly like any session program). The
// default profile is the repo's resolved claude command with
// --dangerously-skip-permissions ensured — the root agent's whole purpose is
// autonomous operation (#1106).
func rootAgentProgram(repoRoot string, rc config.RootAgentConfig) string {
	if strings.TrimSpace(rc.Program) != "" {
		return rc.Program
	}
	program := "claude"
	if resolved, err := config.ResolveConfig(repoRoot); err == nil {
		program = config.ResolveProgram(&resolved.Config, "claude")
	} else {
		log.WarningLog.Printf("root agent for %s: failed to resolve repo config, using bare claude: %v", repoRoot, err)
	}
	// Only ensure the claude-only flag when the resolved command actually
	// runs claude: a program_overrides entry may point "claude" at another
	// program that exits on the unknown flag (#1116 defect class — e.g. the
	// play-test sandbox's "claude": "bash" override).
	if tmux.DetectAgentFromCommand(program) == tmux.ProgramClaude &&
		!strings.Contains(program, rootDangerouslySkipPermissionsFlag) {
		program += " " + rootDangerouslySkipPermissionsFlag
	}
	return program
}
