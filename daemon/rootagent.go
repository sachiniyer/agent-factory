package daemon

import (
	"context"
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
// triggers a (re-)create. An explicit KillSession suppresses re-creation in
// this daemon until rootKillHealDelay; restart re-asserts configuration
// immediately, and an elapsed delay takes effect on the next ensure pass (#1223).

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

// rootKillHealDelay is the grace window the ensure loop honors after an
// explicit KillSession of the root before it self-heals a still-configured
// root (#1223). Long enough to cover a deliberate manual restart or a brief
// stop, short enough that an always-on root is never left dead for long — the
// #1223 outage kept it dead 23 minutes (until a daemon restart). Package var so
// tests can shorten it; the only permanent stop is removing the repo from
// root_agents. The ensure loop reads the injectable package clock nowFunc for
// the grace comparison, so tests advance time instead of sleeping.
var rootKillHealDelay = 2 * time.Minute

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

// buildRootAgentSnapshot captures the start-of-day root-agent configuration the
// ensure loop resolves against (#2216 Phase 6): the global [root_agent]
// singleton, every registered project's personal [root_agent] layer and resolved
// root (both keyed by repo ID), and the repo IDs the legacy root_agents map
// already covers (so the singleton sweep can dedupe against it). It is
// best-effort — a project whose checkout no longer resolves, or whose personal
// config cannot be read, is logged and skipped, never fatal, so one bad project
// never keeps the daemon from starting. Reading the registry once at start
// matches the RootAgents map's restart-to-apply contract: registering a project
// or editing its personal root_agent takes effect on the next daemon start.
func buildRootAgentSnapshot(cfg *config.Config) (global *config.RootAgentLayer, personal map[string]*config.RootAgentLayer, projectRoots map[string]string, legacyRepoIDs map[string]bool) {
	global = config.GlobalRootAgentLayer(cfg)
	personal = map[string]*config.RootAgentLayer{}
	projectRoots = map[string]string{}
	legacyRepoIDs = map[string]bool{}

	for path := range cfg.RootAgents {
		repo, err := config.RepoFromPath(config.ExpandTilde(path))
		if err != nil {
			// A not-yet-cloned legacy path is normal (#1122): the per-path ensure
			// sweep retries it. It is simply not part of the dedup set until it
			// resolves — and while it does not resolve it cannot collide with a
			// registered project (which did resolve at start).
			continue
		}
		legacyRepoIDs[repo.ID] = true
	}

	projects, err := config.ListProjects()
	if err != nil {
		log.WarningLog.Printf("root agent snapshot: could not list registered projects; singleton root agents are disabled until the next daemon start: %v", err)
		return global, personal, projectRoots, legacyRepoIDs
	}
	for _, p := range projects {
		repo, err := config.RepoFromPath(p.Root)
		if err != nil {
			log.WarningLog.Printf("root agent snapshot: project %s root %s does not resolve to a git repository; skipping it until the next daemon start: %v", p.ID, p.Root, err)
			continue
		}
		projectRoots[repo.ID] = repo.Root
		pc, err := config.LoadProjectConfig(p.ID)
		if err != nil {
			log.WarningLog.Printf("root agent snapshot: project %s personal config is unreadable; ignoring its root_agent override until the next daemon start: %v", p.ID, err)
			continue
		}
		if layer := pc.RootAgentLayer(); layer != nil {
			personal[repo.ID] = layer
		}
	}
	return global, personal, projectRoots, legacyRepoIDs
}

// rootAgentInputsFor assembles the resolver inputs for one repo from the
// start-of-day snapshot plus its (optional) legacy entry, so every caller — the
// ensure sweep and repoRootAgentWillMaterialize — layers the same sources and
// can never disagree on whether a root should run or what it runs.
func (m *Manager) rootAgentInputsFor(repoID string, legacy *config.RootAgentConfig) config.RootAgentInputs {
	return config.RootAgentInputs{
		Global:   m.rootAgentGlobal,
		Legacy:   legacy,
		Personal: m.rootAgentPersonal[repoID],
	}
}

// legacyRootAgentForRepo returns a copy of the root_agents entry whose path
// resolves to repoID, or nil if none does. Resolved per call (not from the
// snapshot dedup set) to preserve the legacy contract that a path pointing at a
// not-yet-cloned repo starts applying the moment the repo appears.
func (m *Manager) legacyRootAgentForRepo(repoID string) *config.RootAgentConfig {
	entry, _ := config.LegacyRootAgentForRepo(m.cfg, repoID)
	return entry
}

// rootAgentResolutionForRepo resolves the effective root-agent profile for one
// repo ID and reports whether the repo is a candidate at all (named by a legacy
// entry or a registered project). It is the single authority
// repoRootAgentWillMaterialize shares with the ensure loop, so a delivery waits
// for a root exactly when the loop will create one. Layers are merged, so a
// personal enabled=false disables a legacy-enabled root here too.
func (m *Manager) rootAgentResolutionForRepo(repoID string) (config.RootAgentResolution, bool) {
	legacy := m.legacyRootAgentForRepo(repoID)
	_, isProject := m.rootAgentProjectRoots[repoID]
	if legacy == nil && !isProject {
		return config.RootAgentResolution{}, false
	}
	return config.ResolveRootAgent(m.rootAgentInputsFor(repoID, legacy)), true
}

// EnsureRootAgents runs one ensure pass over every repo that resolves to an
// enabled root agent: the legacy root_agents entries and the registered projects
// a global/personal [root_agent] singleton turns on (#2216 Phase 6). Called from
// the daemon poll loop after RefreshStatuses; a no-op until the initial restore
// finishes.
func (m *Manager) EnsureRootAgents() {
	if !m.Ready() {
		return
	}

	// Legacy root_agents paths first: this path keeps the per-path RepoFromPath
	// retry and backoff a not-yet-cloned repo relies on (#1122). Sorted for a
	// deterministic order.
	if len(m.cfg.RootAgents) > 0 {
		paths := make([]string, 0, len(m.cfg.RootAgents))
		for path := range m.cfg.RootAgents {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			m.ensureLegacyRootAgent(path, m.cfg.RootAgents[path])
		}
	}

	// Registered projects a legacy entry did NOT already cover, enabled purely by
	// the global or personal [root_agent] singleton. Skipping legacy-covered repo
	// IDs is what keeps a repo named by both from being ensured twice.
	if len(m.rootAgentProjectRoots) > 0 {
		repoIDs := make([]string, 0, len(m.rootAgentProjectRoots))
		for repoID := range m.rootAgentProjectRoots {
			if m.rootAgentLegacyRepoIDs[repoID] {
				continue
			}
			repoIDs = append(repoIDs, repoID)
		}
		sort.Strings(repoIDs)
		for _, repoID := range repoIDs {
			m.ensureSingletonRootAgent(repoID)
		}
	}
}

// ensureLegacyRootAgent ensures the root for one root_agents path, resolving its
// full layer stack (global + this legacy entry + the project's personal layer)
// so a personal enabled=false can disable it. It owns the per-path RepoFromPath
// retry/backoff (#1122); resolution and the create/adopt/heal tail are shared
// with the singleton path via ensureResolvedRoot.
func (m *Manager) ensureLegacyRootAgent(path string, rc config.RootAgentConfig) {
	m.mu.Lock()
	st := m.rootEnsureStateForLocked(path)
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
	resolution := config.ResolveRootAgent(m.rootAgentInputsFor(repo.ID, &rc))
	m.ensureResolvedRoot(path, st, repo, resolution)
}

// ensureSingletonRootAgent ensures the root for a registered project enabled by
// the global/personal singleton with no legacy root_agents entry. The repo
// identity and root come from the start-of-day snapshot (no per-tick git
// resolution — a registered project resolved at daemon start), and the state is
// keyed by that resolved root path.
func (m *Manager) ensureSingletonRootAgent(repoID string) {
	root := m.rootAgentProjectRoots[repoID]
	m.mu.Lock()
	st := m.rootEnsureStateForLocked(root)
	skip := time.Now().Before(st.nextAttempt)
	m.mu.Unlock()
	if skip {
		return
	}
	repo := &config.RepoContext{Root: root, ID: repoID}
	resolution := config.ResolveRootAgent(m.rootAgentInputsFor(repoID, nil))
	m.ensureResolvedRoot(root, st, repo, resolution)
}

// rootEnsureStateForLocked returns the retry state for a candidate key, creating
// it on first use. Caller must hold m.mu.
func (m *Manager) rootEnsureStateForLocked(key string) *rootEnsureState {
	st := m.rootEnsureStates[key]
	if st == nil {
		st = &rootEnsureState{}
		m.rootEnsureStates[key] = st
	}
	return st
}

// ensureResolvedRoot is the shared create/adopt/heal tail for a resolved
// candidate: adopt a live root untouched, heal a Dead/Lost one in place, create
// a missing one, and respect an explicit user kill. stateKey names the retry
// state (a legacy config path or a project root); resolution is the merged
// profile. A candidate the config resolves to disabled is a no-op that resets
// the retry state and leaves any existing root alone — removing an opt-in never
// tears a live root down, it just stops re-ensuring it. All outcomes are logged;
// failures back off exponentially and settle at rootEnsureBackoffMax, so the
// loop always heals once the cause clears.
func (m *Manager) ensureResolvedRoot(stateKey string, st *rootEnsureState, repo *config.RepoContext, resolution config.RootAgentResolution) {
	if !resolution.Enabled {
		m.rootEnsureSucceeded(st)
		return
	}

	// A project deleted at runtime (#1735) is suppressed for the rest of this
	// daemon's life: DeleteProject already stopped its root and removed it from
	// root_agents on disk, so respawning it here from the still-immutable
	// in-memory config would resurrect the project the user just deleted.
	m.mu.Lock()
	_, deleted := m.deletedRootRepos[repo.ID]
	m.mu.Unlock()
	if deleted {
		m.rootEnsureSucceeded(st)
		return
	}

	key := daemonInstanceKey(repo.ID, session.RootSessionTitle)
	m.mu.Lock()
	killedAt, killed := m.rootKilledAt[repo.ID]
	inst := m.instances[key]
	m.mu.Unlock()

	if killed {
		if nowFunc().Before(killedAt.Add(rootKillHealDelay)) {
			// Still inside the grace window: honor the user's stop, but only
			// briefly. Logged once per suppression so a killed root does not
			// spam the log every poll tick.
			m.mu.Lock()
			logSuppression := !st.suppressLogged
			st.suppressLogged = true
			m.mu.Unlock()
			if logSuppression {
				log.InfoLog.Printf("root agent for %s was explicitly killed; honoring the stop, will re-create it in ~%s (config is the source of truth for an always-on root — remove it from root_agents to keep it down)", repo.Root, rootKillHealDelay)
			}
			return
		}
		// Grace window elapsed and the repo is still configured: config wins,
		// so clear the kill and fall through to re-create. This is the #1223
		// self-heal — a killed (or outage-downed) root comes back without a
		// daemon restart.
		m.mu.Lock()
		delete(m.rootKilledAt, repo.ID)
		st.suppressLogged = false
		m.mu.Unlock()
		log.InfoLog.Printf("root agent for %s: kill grace window elapsed; re-creating (always-on self-heal, #1223)", repo.Root)
	}

	// What the vanished root was, snapshotted before the reap deletes the record
	// that holds it: the conversation it was in (#2616) and the tabs it had open
	// (#2628). reapedRoot distinguishes "the reaped root had none of these" from
	// "there was no prior root at all" — only the first is worth reporting, and
	// they are different answers to the question an operator asks after an outage.
	var (
		carried    reapedRootState
		reapedRoot bool
	)

	if inst != nil {
		if status := inst.GetStatus(); status != session.Dead && status != session.Lost && status != session.Archived {
			// Adopt, never clobber: a live root — whatever program it runs
			// and whoever created it — is the root agent. Nothing to do.
			m.rootEnsureSucceeded(st)
			return
		}
		// An Archived root (#1028) is inert — no tmux — so it must NOT be
		// adopted as live; fall through to reap-and-recreate like Dead/Lost so
		// the always-ensured root comes back. In practice ArchiveSession
		// rejects archiving the reserved root title, so this is defensive; the
		// in-place root worktree is external, so reapDeadRoot's Cleanup is a
		// no-op that only removes daemon-owned state.
		// The root's tmux vanished (crash, tmux server death — the #1104
		// outage class; recorded as Lost since #1108, Dead by older builds).
		// Reap the dead record and fall through to re-create in place — the
		// root keeps its stronger always-ensure semantics rather than waiting
		// for the general Lost-restore loop. Kill is best-effort teardown of
		// already-dead tmux, and an in-place worktree's Cleanup never touches
		// the user's tree (#1107), so this can only remove daemon-owned state.
		//
		// Carrying agent_conversation across that reap is what keeps the two
		// halves of the heal from disagreeing (#2616): the record about to be
		// deleted holds the only pointer to the conversation the root was in, and
		// CreateSession would otherwise mint a fresh id — leaving the one session
		// every watch/monitor delivery targets healthy, Ready, and amnesiac. The
		// tab roster rides across for the same reason and from the same record
		// (#2628): a fresh create comes up with only its agent tab, so everything
		// else the user had open — a terminal, a process tab, a dev-server web
		// tab, an editor — would vanish with the record that listed it.
		log.WarningLog.Printf("root agent for %s is gone (tmux vanished); attempting to reap and re-create it in place", repo.Root)
		var err error
		carried, reapedRoot, err = m.reapDeadRoot(repo.ID, inst)
		if err != nil {
			m.rootEnsureFailed(stateKey, st, fmt.Errorf("failed to remove dead root record: %w", err))
			return
		}
		if !reapedRoot {
			return
		}
	}

	program := rootAgentProgramForProfile(repo.Root, resolution.RootAgent)
	req := CreateSessionRequest{
		Title:         session.RootSessionTitle,
		RepoPath:      repo.Root,
		Program:       program,
		InPlace:       true,
		allowReserved: true,
		// Both zero on every path that did not just reap a root — a first-ever
		// create, or a kill whose grace window elapsed (KillSession already deleted
		// that record, so there is nothing to continue or rebuild).
		resumeConversation: carried.conversation,
		restoreTabs:        carried.tabs,
	}
	data, err := m.CreateSession(context.Background(), req)
	if err != nil && req.resumeConversation.HasID() {
		// The always-on guarantee outranks continuity. A conversation the provider
		// can no longer resume (cleared history, a transcript store the agent no
		// longer has) makes the resumed command exit at startup — and since the
		// reaped record is already gone, retrying it here rather than next tick is
		// what keeps an unresumable id from costing the root a backoff interval of
		// downtime. Losing the history is the bug this carry fixes; losing the ROOT
		// would be worse than the bug.
		log.WarningLog.Printf("root agent for %s could not be re-created on its prior %s conversation %s (%v); retrying with a fresh agent",
			repo.Root, carried.conversation.Agent, carried.conversation.ID, err)
		req.resumeConversation = session.AgentConversationData{}
		data, err = m.CreateSession(context.Background(), req)
	}
	if err != nil {
		m.rootEnsureFailed(stateKey, st, fmt.Errorf("failed to create root session: %w", err))
		return
	}
	log.InfoLog.Printf("ensured root agent for %s (in-place, program %q)", repo.Root, program)
	if reapedRoot {
		reportRootConversationCarry(repo.Root, carried.conversation, data.AgentConversation)
		reportRootTabCarry(repo.Root, carried.tabs, data.Tabs)
	}
	m.rootEnsureSucceeded(st)
}

// reportRootTabCarry records what became of the reaped root's non-agent tabs
// (#2628), on the same principle as reportRootConversationCarry: a heal that
// silently drops part of what the user had open is the bug, so the outcome has
// to be legible even when it is the good one.
//
// It counts what the NEW record actually holds rather than what the create was
// asked to rebuild — a tab whose tmux could not be re-spawned is dropped
// best-effort by setupTabs, and the record is what the tab strip renders. Silent
// on a root that had no extra tabs, which is the overwhelmingly common case.
func reportRootTabCarry(repoRoot string, carried, created []session.TabData) {
	want := countNonAgentTabs(carried)
	if want == 0 {
		return
	}
	got := countNonAgentTabs(created)
	if got < want {
		log.WarningLog.Printf("re-created root agent for %s brought back %d of its %d non-agent tabs; the rest could not be restored", repoRoot, got, want)
		return
	}
	log.InfoLog.Printf("re-created root agent for %s restored its %d non-agent tabs", repoRoot, got)
}

// countNonAgentTabs counts the tabs of a persisted roster that a re-create has
// to rebuild: everything but the agent tab, which every launch spawns itself.
func countNonAgentTabs(tabs []session.TabData) int {
	n := 0
	for idx, td := range tabs {
		if idx == 0 || td.Kind == session.TabKindAgent {
			continue
		}
		n++
	}
	return n
}

// reportRootConversationCarry records what became of the reaped root's
// conversation, as three outcomes a log reader can tell apart (#2616).
//
// Falling back to a fresh agent is legitimate — the root must exist — but it
// must not READ like a carry-over. That is the whole shape of this bug: eight
// silent re-creates over three and a half weeks, each one a session that came
// back Ready with an identical rail row, found only because someone went
// looking in the log. A fix that leaves "resumed" and "started over"
// indistinguishable just relocates the invisibility.
//
// It reports the conversation the new record actually CARRIES rather than what
// the create was asked to do: a create can be handed a conversation and still
// come up on a different one (the resolved program runs another agent, or pins
// its own resume flag), and the record is the thing that will be resumed from
// next time.
func reportRootConversationCarry(repoRoot string, carried session.AgentConversationData, created *session.AgentConversationData) {
	switch {
	case !carried.HasID():
		log.WarningLog.Printf("re-created root agent for %s had no recorded conversation to carry; it starts with a fresh context", repoRoot)
	case created != nil && created.Agent == carried.Agent && created.ID == carried.ID:
		log.InfoLog.Printf("re-created root agent for %s resumed its prior %s conversation %s", repoRoot, carried.Agent, carried.ID)
	case created == nil:
		log.WarningLog.Printf("re-created root agent for %s did not record its prior %s conversation %s; the resolved command may select its own conversation, so context continuity is unknown",
			repoRoot, carried.Agent, carried.ID)
	default:
		log.WarningLog.Printf("re-created root agent for %s did not come up on its prior %s conversation %s; it starts with a fresh context",
			repoRoot, carried.Agent, carried.ID)
	}
}

// deliverToReemergingRoot handles a DeliverPrompt whose absent target is this
// repo's daemon-managed root agent, momentarily gone while the ensure loop
// re-materializes it in place after a tmux outage (#1223). It waits for the
// ensure loop to bring root back (bounded by targetDeliverWait, mirroring the
// concurrent-create retry) and then sends the prompt into it, so a watch/
// monitor event is delivered once root returns instead of being dropped by the
// reserved-name guard the auto-create path would hit. Returns handled=false
// when the target is not a re-emerging root, so DeliverPrompt falls through to
// its normal create path; on a wait timeout it returns handled=true with an
// accurate "being recreated" error rather than the misleading reserved-name one.
func (m *Manager) deliverToReemergingRoot(repo *config.RepoContext, req DeliverPromptRequest) (string, bool, error) {
	if !session.IsReservedTitle(req.Title) || !m.repoRootAgentWillMaterialize(repo.ID) {
		return "", false, nil
	}
	if err := m.waitForTargetSession(repo.ID, req.Title); err != nil {
		// Pre-flight: the root never reappeared within the wait, so nothing was
		// sent — refund the rate slot (#2501). This is the reserved-root outage
		// path a monitor task targeting `root` hits during a tmux blip.
		return "", true, notAttempted(fmt.Errorf("root agent for %q is being recreated (tmux momentarily absent); event not delivered this attempt: %w", repo.Root, err))
	}
	// A TUI can attach to root during the wait above, so re-check the defer lease
	// before sending — otherwise this path pastes into an attached pane the
	// "exists" path would have deferred (#1638).
	if m.deferWhileAttached(repo.ID, req) {
		return StatusDeferredAttached, true, nil
	}
	if err := m.SendPrompt(SendPromptRequest{Title: req.Title, RepoID: repo.ID, Prompt: req.Prompt}); err != nil {
		return "", true, err
	}
	return "sent", true, nil
}

// repoRootAgentWillMaterialize reports whether the daemon's ensure loop is
// responsible for (re-)creating the reserved "root" session for this repo: the
// repo's layered root_agent config resolves to enabled and its project has not
// been deleted at runtime. Config is the single source of truth for "root should
// be running" — a root that is Dead, Lost, or even explicitly killed self-heals
// (the kill only delays re-creation by rootKillHealDelay, #1223), so an enabled
// root always materializes eventually and a delivery to a momentarily-absent one
// should wait for the ensure loop rather than auto-create it (which the
// reserved-name guard would reject).
//
// It routes through rootAgentResolutionForRepo, the SAME resolution the ensure
// loop uses, so the two never disagree: a repo the loop will not create (a
// personal enabled=false over a legacy entry, or a project no singleton turned
// on) reports false, and a singleton-only project the loop WILL create reports
// true even with no legacy root_agents entry. Otherwise a send-prompt to such a
// root is wrongly rejected at the reserved-name gate (#1835).
//
// A deleted project (#1735) is the one case where the still-immutable snapshot
// outlives the truth: DeleteProject removed the opt-in from disk but this
// in-memory copy keeps listing the repo, and ensureResolvedRoot skips it for the
// rest of the daemon's life. Answering from config alone would make callers wait
// out targetDeliverWait for a root that can never come back, then blame a
// recreation that is not happening, so deletion is checked with the same
// lock+lookup the ensure loop uses, before the resolution.
func (m *Manager) repoRootAgentWillMaterialize(repoID string) bool {
	m.mu.Lock()
	_, deleted := m.deletedRootRepos[repoID]
	m.mu.Unlock()
	if deleted {
		return false
	}
	resolution, ok := m.rootAgentResolutionForRepo(repoID)
	return ok && resolution.Enabled
}

// reapedRootState is what a reaped root record hands to its replacement: the
// state a fresh CreateSession cannot reconstruct on its own, snapshotted from
// the exact record the reap is about to delete. It is one value rather than a
// growing return list so a future field cannot be added to the snapshot and
// forgotten at the create — the shape of both #2616 and #2628.
type reapedRootState struct {
	// conversation is the provider conversation the vanished root was in (#2616).
	conversation session.AgentConversationData
	// tabs is its full persisted roster, agent tab included (#2628). The create
	// ignores index 0 and rebuilds the rest; keeping the roster whole means the
	// snapshot is exactly what the record held, not a pre-filtered view of it.
	tabs []session.TabData
}

// reapDeadRoot removes a Dead root instance so ensureRootAgent can re-create
// the title. On success it returns the state snapshotted under the operation
// lock from the exact record it deleted. The boolean reports whether
// the root was actually reaped; false means a concurrent operation owns or
// changed the title, or provider conversation discovery is still polling, so
// ensure should wait for a later tick instead of falling through to
// CreateSession. Mirrors KillSession's teardown but deliberately does NOT
// record rootKilledAt: this is the daemon healing itself, not a user decision.
func (m *Manager) reapDeadRoot(repoID string, inst *session.Instance) (reapedRootState, bool, error) {
	key := daemonInstanceKey(repoID, session.RootSessionTitle)
	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		// A user kill (or its finish pass) owns this title right now. Let that
		// operation decide whether the root is removed or left for the next tick.
		return reapedRootState{}, false, nil
	}
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	capturePending := m.pendingConversationCaptures[inst] > 0
	m.mu.Unlock()
	if killing || current != inst || capturePending {
		return reapedRootState{}, false, nil
	}

	// Snapshot only after taking the same operation lock used by async
	// conversation capture and re-confirming that this is still the tracked
	// instance. Reading before the lock leaves a narrow loss window: capture can
	// commit a newly discovered ID after the read but before the reap acquires
	// the lock, and the reap would then delete the updated record while carrying
	// the stale snapshot.
	//
	// Both fields come out of ONE projection — the same one the record on disk is
	// written from — rather than two reads of the instance. Two reads could land
	// on either side of a capture commit and hand the replacement a conversation
	// and a roster that never coexisted; there is no reason to leave that open
	// when the whole record is available atomically.
	snapshot := inst.ToInstanceData()
	carried := reapedRootState{tabs: snapshot.Tabs}
	if snapshot.AgentConversation != nil {
		carried.conversation = *snapshot.AgentConversation
	}

	// The reaped root's per-session editor is bound to the record this pass is
	// deleting, so it must not outlive it — a carried vscode tab re-resolves
	// against the REPLACEMENT record and lazily spawns a fresh editor on the next
	// proxy request (WebTabTarget), which is what makes the carry safe here. Stop
	// before runtime teardown and confirm again before record deletion, mirroring
	// KillSession/finishUserKill and closing a proxy spawn that resolved the dead
	// root immediately before this pass took ownership. Either unknown result
	// retains the record for the next ensure pass.
	if err := m.stopVSCodeForInstance(key, inst.ID); err != nil {
		return reapedRootState{}, false, fmt.Errorf("reaping dead root for repo %s: VS Code editor teardown is not confirmed, retaining its record for a retry: %w", repoID, err)
	}

	// Best-effort by design (#478): tmux is already gone and an in-place
	// worktree's Cleanup is a no-op, so failures Kill can ANSWER for only log
	// inside Kill and never surface here.
	//
	// An error that does reach us therefore means the teardown could not complete
	// SAFELY — tmux never confirmed the pane dead, or a worktree removal was cut off
	// mid-delete — so the workspace is still there. Deleting the record would orphan
	// it and leave nothing pointing at it. Keep the record; this loop runs every
	// tick, so it IS the retry (#1917: found by auditing every record delete against
	// the invariant, not reported).
	teardownErr := inst.Kill()
	if err := m.stopVSCodeForInstance(key, inst.ID); err != nil {
		return reapedRootState{}, false, fmt.Errorf("reaping dead root for repo %s: VS Code editor teardown is not confirmed after runtime teardown, retaining its record for a retry: %w", repoID, err)
	}
	// Through the one choke point (#1917): it refuses while the teardown's outcome
	// is unknown. This site was still log-and-delete after two audits I called
	// exhaustive — which is the argument for there being exactly one place to call.
	deleted, err := m.deleteSessionRecord(repoID, session.RootSessionTitle, inst.ID, teardownErr)
	if err != nil {
		// Return the ERROR, not (false, nil) (#1917 round 8). "No, but fine" is
		// absence-of-error wearing a different hat: the caller reads it as "nothing to
		// reap" and skips rootEnsureFailed, so a persistent tmux/file-lock timeout
		// re-runs this whole bounded teardown on EVERY tick — occupying the single
		// status/restore poll loop and spamming warnings — instead of backing off.
		// A failure has to look like one for the retry cadence to see it.
		return reapedRootState{}, false, fmt.Errorf("reaping dead root for repo %s: %w", repoID, err)
	}
	if !deleted {
		log.InfoLog.Printf("dead root reap for repo %s skipped storage delete: current root record has a different instance identity", repoID)
		return reapedRootState{}, false, nil
	}
	m.mu.Lock()
	if m.instances[key] == inst {
		delete(m.instances, key)
	}
	m.mu.Unlock()
	return carried, true, nil
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

// rootAgentProgram resolves the command the root agent runs from a legacy
// per-repo entry. Retained as the thin adapter the direct-map program test and
// any legacy-only caller use; it delegates to rootAgentProgramForProfile so the
// resolution rule lives in exactly one place.
func rootAgentProgram(repoRoot string, rc config.RootAgentConfig) string {
	return rootAgentProgramForProfile(repoRoot, config.RootAgent{Program: rc.Program})
}

// rootAgentProgramForProfile resolves the command the root agent runs from a
// resolved root-agent profile. An explicit program wins verbatim (an agent enum
// name still resolves through program_overrides downstream, exactly like any
// session program). The default profile — an empty program — is the repo's
// resolved claude command with --dangerously-skip-permissions ensured, the root
// agent's whole purpose being autonomous operation (#1106).
func rootAgentProgramForProfile(repoRoot string, ra config.RootAgent) string {
	if strings.TrimSpace(ra.Program) != "" {
		return ra.Program
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
