package daemon

import (
	"context"
	"errors"
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
// after RefreshStatuses, so a root whose tmux died is marked Dead and its heal
// begins in the same tick — begins, not completes: since #3721 the reap and
// (re-)create run on their own goroutine (rootagent_create.go), because their
// unbounded filesystem work must never own the poll goroutine.
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
// ensure loop escalates to an ERROR log: the cause now looks persistent (a
// deleted repo path, an unparseable persisted root record), not transient.
// Once per streak, and a second time only for the one transition that changes
// what the ERROR may claim — see rootEnsureShouldEscalate (#3500). The loop never stops retrying — it settles at the
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
	// A transcript creation or removal can lag one poll without affecting the
	// live root. Bound the directory scan instead of statting every historical
	// transcript on the daemon's default one-second ensure cadence.
	rootClaudeTranscriptInspectionInterval = 30 * time.Second
)

// rootEnsureBackoffFor is the shared ensure-cadence backoff curve: base
// doubling per consecutive failure, capped at max. Used by the per-candidate
// retry state and by the snapshot heal pass (#3264), so both pace on one rule.
func rootEnsureBackoffFor(consecutiveFailures int) time.Duration {
	backoff := rootEnsureBackoffMax
	// Guard the shift: past ~16 doublings the exponential form has no meaning
	// and would overflow.
	if shift := consecutiveFailures - 1; shift < 16 {
		if b := rootEnsureBackoffBase << shift; b < backoff {
			backoff = b
		}
	}
	return backoff
}

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
// loop. Guarded by Manager.mu, and since #3721 written from two goroutines
// rather than one: the sweep on the poll goroutine records what it decides
// without a create, and each create goroutine records its own outcome
// (rootEnsureSucceeded/rootEnsureFailed). Tests drive EnsureRootAgents directly.
type rootEnsureState struct {
	consecutiveFailures int
	// unansweredFailures counts how many of those consecutive failures never
	// got an answer out of git (#3500), so the escalation ERROR can only claim
	// a persistent cause when something actually established one.
	unansweredFailures int
	// escalated and escalatedPersistent record whether this streak has already
	// logged its escalation ERROR and whether that ERROR was allowed to claim a
	// persistent cause, so a streak escalated without that evidence can escalate
	// once more if the evidence arrives (#3500 review).
	escalated           bool
	escalatedPersistent bool
	nextAttempt         time.Time
	// suppressLogged dedupes the "not re-creating a user-killed root" log
	// line to once per suppression.
	suppressLogged bool
	// Claude transcript verification is advisory while the root is live. Keep
	// its filesystem work and any persistent inspection warning off the hot
	// one-second ensure path.
	nextClaudeTranscriptInspection time.Time
	claudeTranscriptWarning        string
}

// resolvedRootAgentFor is the single resolution choke point for one repo: every
// caller — both ensure sweeps and rootAgentMaterializeVerdictFor — resolves
// through it (or through the same snapshot's resolve), so they can never
// disagree on whether a root should run or what it runs.
func (m *Manager) resolvedRootAgentFor(repoID string, legacy *config.RootAgentConfig) config.RootAgentResolution {
	if m.rootAttributionPendingFor(repoID) {
		// Identity verification for a recorded root that resolves to this
		// repo is still in flight: fail closed exactly like the snapshot's
		// own unknowable states, or the legacy sweep could start the root
		// seconds before a personal enabled=false is re-attributed onto it
		// (#3299 review round 8).
		return config.RootAgentResolution{}
	}
	return m.rootAgentLayers.Load().resolve(repoID, legacy)
}

// resolve applies the daemon's fail-closed policy (#3241, #3247) before
// layering: a repo whose decision decisionUnknown reports unknowable resolves
// to disabled without consulting lower layers — absence of proof is not
// permission to start an agent. The returned zero resolution carries no
// provenance on purpose: no config source decided this, the daemon's read
// failure did, and the cause stays available in the snapshot's unreadable
// fields, which rootAgentMaterializeVerdictFor names to consumers (#3264).
// Callers resolve through this method, never through config.ResolveRootAgent
// directly, so the gate cannot be bypassed.
func (s *rootAgentSnapshot) resolve(repoID string, legacy *config.RootAgentConfig) config.RootAgentResolution {
	if s.decisionUnknown(repoID) {
		return config.RootAgentResolution{}
	}
	personal := s.personal[repoID]
	if record, ok := s.unresolvedRoots[repoID]; ok && record.identityMismatch {
		// The layer under this ID belongs to a project whose claim on the
		// recorded path is DISPROVEN — a different clone occupies it (#3299
		// review round 13; same-path shape, where the derived hash IS the
		// occupant's real ID). Neither the dead claim's enable (its program
		// in a stranger's checkout) nor its disable (vetoing the occupant's
		// own legacy entry) may govern the occupant. Unknowable shapes keep
		// the layer: while undisproven, fail-closed applies it.
		personal = nil
	}
	return config.ResolveRootAgent(config.RootAgentInputs{
		Global:   s.global,
		Legacy:   legacy,
		Personal: personal,
	})
}

// decisionUnknown is the daemon's one fail-closed predicate for root agents:
// it reports that no layered resolution for this repo can be trusted, because
// a config source that may hold the highest-precedence enabled=false could
// not be read — the project registry itself (#3247, which hides every
// personal layer at once), or this repo's personal config (#3241, including
// one attributed by recorded path while its project root does not resolve).
// The answer is stable for the snapshot value it is asked of; the unknowns
// only ever NARROW, when healRootAgentLayers replaces the snapshot after a
// retried read succeeds (#3264).
func (s *rootAgentSnapshot) decisionUnknown(repoID string) bool {
	if s.registryUnreadable {
		return true
	}
	if _, unreadable := s.personalUnreadable[repoID]; unreadable {
		// A PROVEN mismatch releases this latch (#3299 review round 14): for
		// a main-root recording the derived hash is the occupant's real ID,
		// and the unreadable config belongs to a project whose claim on the
		// path is disproven — it cannot govern the occupant, whichever value
		// it holds.
		if record, ok := s.unresolvedRoots[repoID]; !ok || !record.identityMismatch {
			return true
		}
	}
	// A checkout at some project's recorded root resolves to this repo, but
	// its marker could not be READ (#3299 review round 6): the checkout may
	// BE that project — whose personal layer (possibly enabled=false, or
	// itself unreadable) sits where this resolution cannot see or trust it.
	// Identity unknowable means the decision is unknowable; a legacy entry
	// for the same repo must not start the root off global layers alone. A
	// PROVEN mismatch deliberately does not gate here: a different project's
	// layers do not govern this repo. The DIRECT lookup covers main-root
	// recordings, where the derived hash IS the occupant's real ID and no
	// bridge is ever recorded (#3299 review round 15).
	if record, ok := s.unresolvedRoots[repoID]; ok && record.markerUnreadable {
		return true
	}
	return false
}

// legacyRootAgentForRepo returns a copy of the root_agents entry whose path
// resolves to repoID, or nil if none does. Resolved per call (not from the
// snapshot dedup set) to preserve the legacy contract that a path pointing at a
// not-yet-cloned repo starts applying the moment the repo appears.
func (m *Manager) legacyRootAgentForRepo(repoID string) *config.RootAgentConfig {
	entry, _ := config.LegacyRootAgentForRepo(m.cfg, repoID)
	return entry
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

	// Safe-direction self-heal first (#3264): while the snapshot carries
	// unknowns, re-attempt exactly the reads that failed, so a boot-time
	// transient does not pin root agents off until a human restarts the daemon.
	m.healRootAgentLayers()

	layers := m.rootAgentLayers.Load()
	// With the registry (still) unlistable (#3247) every candidate resolves to
	// disabled through decisionUnknown, so the sweeps below could only fork git
	// per legacy path per tick to re-derive a constant refusal — and the legacy
	// path's retry/escalation logging would keep promising heals this state
	// cannot deliver. The snapshot's boot-time ERROR plus the heal pass above
	// are the messages for this state; skip the sweeps entirely.
	if layers.registryUnreadable {
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
	if len(layers.projectRoots) > 0 {
		repoIDs := make([]string, 0, len(layers.projectRoots))
		for repoID := range layers.projectRoots {
			if layers.legacyRepoIDs[repoID] {
				continue
			}
			repoIDs = append(repoIDs, repoID)
		}
		sort.Strings(repoIDs)
		for _, repoID := range repoIDs {
			m.ensureSingletonRootAgent(repoID, layers.projectRoots[repoID])
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

	repo, err := resolveLegacyRootRepo(path)
	if err != nil {
		m.rootEnsureFailed(path, st, fmt.Errorf("%s: %w", repoResolveClaim("root_agents entry", path, err), err))
		return
	}
	resolution := m.resolvedRootAgentFor(repo.ID, &rc)
	// No identity to re-prove: a root_agents entry is an opt-in the user wrote
	// against a PATH, not a registry record, so there is no recorded checkout id
	// a checkout there must match. #3334 already settled that shape — a proven
	// mismatch releases the repo so its legacy opt-in still applies — and #3366
	// deliberately does not change it (see verifyRootCreateCheckout).
	m.ensureResolvedRoot(path, st, repo, resolution, nil)
}

// rootLegacyRepoProbeTimeout bounds every repository resolution the legacy
// root_agents machinery performs on the instance poll goroutine: the sweep's
// own, on every non-backed-off tick (#3757), and the dedup-set recompute the
// heal pass runs on every published heal (#3782 item 1). One budget rather
// than two, because it is one goroutine, one purpose, and one asymmetry — and
// a second knob would be a second thing to get wrong.
//
// 2s, matching rootHealProbeGrace — its sibling on the same goroutine for the
// same purpose — and the value repoGitWaitDelay documents as "what every other
// WaitDelay in this repo uses for this same mechanism".
//
// GENEROUS ON PURPOSE, and the asymmetry is the reason. A budget that expires
// on a checkout which was merely slow costs a HEALTHY repo a backoff interval
// of delayed healing, and #3503 is the standing lesson about stingy probe
// budgets on a box whose load baseline is 60-95. Being late here is cheap;
// being wrong about a live checkout is not. The bound exists to convert
// "indefinitely" into "bounded", not to make the sweep prompt.
//
// The worst case is ~2x this, not this: repoProbeWaitDelay grants each probe a
// WaitDelay allowance equal to the caller's remaining time, and that timer
// STARTS WHEN THE CONTEXT IS DONE, so it is added to the deadline rather than
// carved out of it. Commands attempted after the deadline get the 50ms floor,
// so the total settles around 4s. Paid at most once per backoff interval — 10s
// doubling to rootEnsureBackoffMax — rather than once per tick, and never
// forever, which was the whole defect.
//
// A package var so tests can drive both sides of the bound; production never
// reassigns it.
var rootLegacyRepoProbeTimeout = 2 * time.Second

// legacyRootRepoFromPath resolves one root_agents path for the ensure sweep,
// under the caller's context. A package var so a test can drive the stalled-path
// case this bound exists for; production assigns it once.
var legacyRootRepoFromPath = config.RepoFromPathContext

// resolveLegacyRootRepo resolves one root_agents path under a bound, owning the
// context's lifetime so it cannot outlive the resolution it bounds.
//
// THE BOUNDED ENTRY POINT, NOT THE UNBOUNDED ONE, and the distinction is the
// whole of #3757. config.RepoFromPathContext's contract states the split: it is
// what "polling and registry scans use ... so one unreachable checkout cannot
// indefinitely block an unrelated live project", while "admission paths retain
// RepoFromPath's full error contract and unbounded caller lifetime". This sweep
// is polling. It admits nothing — a refusal here returns before
// ensureResolvedRoot, so nothing is adopted, reaped, created or released — so
// there is no half-created object a deadline could strand. That is exactly the
// property #3721's create does NOT have, which is why that one was moved off
// this goroutine instead of bounded.
//
// A timed-out resolution is therefore UNKNOWN, never a verdict: the error
// carries config.ErrRepoProbeUnanswered, rootEnsureFailed counts it as a
// failure that established nothing, repoResolveClaim words it in #3500's form,
// and the candidate settles onto the ensure backoff and keeps retrying forever
// (#1122). A live root on that repo is untouched throughout — the sweep never
// reaches the code that could touch it.
func resolveLegacyRootRepo(path string) (*config.RepoContext, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rootLegacyRepoProbeTimeout)
	defer cancel()
	return legacyRootRepoFromPath(ctx, config.ExpandTilde(path))
}

// ensureSingletonRootAgent ensures the root for a registered project enabled by
// the global/personal singleton with no legacy root_agents entry. The repo
// identity and root come from the snapshot the caller enumerated (no per-tick
// git resolution — a registered project resolved when its snapshot was read),
// and the state is keyed by that resolved root path.
//
// The binding's identity evidence rides down to ensureResolvedRoot, which
// re-proves it at the create boundary only (#3366): a binding made once, at
// boot or at re-attribution, is not evidence about the checkout that is at the
// path now.
func (m *Manager) ensureSingletonRootAgent(repoID string, binding resolvedProjectRoot) {
	m.mu.Lock()
	st := m.rootEnsureStateForLocked(binding.root)
	skip := time.Now().Before(st.nextAttempt)
	m.mu.Unlock()
	if skip {
		return
	}
	repo := &config.RepoContext{Root: binding.root, ID: repoID}
	resolution := m.resolvedRootAgentFor(repoID, nil)
	m.ensureResolvedRoot(binding.root, st, repo, resolution, &binding)
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
// loop always heals once the cause clears. identity is the registry binding a
// registry-backed candidate was enumerated from, re-proven at the create
// boundary (#3366), and nil for a legacy root_agents path.
func (m *Manager) ensureResolvedRoot(stateKey string, st *rootEnsureState, repo *config.RepoContext, resolution config.RootAgentResolution, identity *resolvedProjectRoot) {
	if !resolution.Enabled {
		m.rootEnsureSucceeded(st)
		return
	}
	workspace := repo.WorkspacePath()

	// A project deleted at runtime (#1735) is suppressed for the rest of this
	// daemon's life: DeleteProject already stopped its root and removed it from
	// root_agents on disk, so respawning it here from the still-immutable
	// in-memory config would resurrect the project the user just deleted.
	// Deletion state may be keyed by EITHER identity: a project deleted while
	// its recorded root was unavailable keys the fence and tombstone by the
	// derived recorded-path ID, and the snapshot's reattributedFrom alias is
	// the durable bridge — checked here, at admission time, so a derived-ID
	// delete suppresses no matter when it landed relative to the identity
	// transition (#3299 review round 4). An ACTIVE derived-ID delete
	// (projectDeletes) skips this tick the same way; when it finishes, its
	// tombstone holds through this alias.
	sweepLayers := m.rootAgentLayers.Load()
	m.mu.Lock()
	deleted := m.rootDeletionTombstoneApplies(sweepLayers, repo.ID)
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
				log.InfoLog.Printf("root agent for %s was explicitly killed; honoring the stop, will re-create it in ~%s (config is the source of truth for an always-on root — remove it from root_agents to keep it down)", workspace, rootKillHealDelay)
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
		log.InfoLog.Printf("root agent for %s: kill grace window elapsed; re-creating (always-on self-heal, #1223)", workspace)
	}

	if inst != nil {
		if status := inst.GetStatus(); status != session.Dead && status != session.Lost && status != session.Archived {
			// Adopt, never clobber: a live root — whatever program it runs
			// and whoever created it — is the root agent. The one mutation is
			// refreshing a recorded Claude conversation from durable transcript
			// evidence, so a later outage does not carry a rotated-away id (#3306).
			m.refreshRootClaudeConversation(repo.ID, key, workspace, inst, st)
			m.rootEnsureSucceeded(st)
			return
		}
	}

	// THE CREATE BOUNDARY (#3721). Everything above this line reads state; nothing
	// below it can be relied on to return. A (re-)create is an admission path whose
	// first act is an unbounded `git rev-parse`, so running it here made one
	// unreachable checkout stop the poll goroutine — and with it RefreshStatuses,
	// RestoreLostSessions and the settlement retries — for every session on the box.
	// It gets its own goroutine instead; see rootagent_create.go for why the create
	// is moved rather than bounded, and for what keeps the next tick from starting a
	// second one.
	m.launchRootCreate(rootCreateJob{
		stateKey:   stateKey,
		st:         st,
		repo:       repo,
		resolution: resolution,
		identity:   identity,
		inst:       inst,
	})
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
//
// The judgment itself is session.ClassifyRootRecreateContext — the SAME call
// that decides the note on the row (#2629), not a second copy of the rule. The
// log adds one distinction the note does not need (nothing recorded to carry vs
// recorded and not resumed); it cannot add a different verdict.
func reportRootConversationCarry(repoRoot string, carried session.AgentConversationData, created *session.AgentConversationData, launchedAgent string) {
	switch session.ClassifyRootRecreateContext(carried, created, launchedAgent) {
	case session.RootRecreateContextNone:
		log.InfoLog.Printf("re-created root agent for %s resumed its prior %s conversation %s", repoRoot, carried.Agent, carried.ID)
	case session.RootRecreateContextUnknown:
		log.WarningLog.Printf("re-created root agent for %s did not record its prior %s conversation %s; the resolved command may select its own conversation, so context continuity is unknown",
			repoRoot, carried.Agent, carried.ID)
	default:
		switch {
		case !carried.HasID():
			log.WarningLog.Printf("re-created root agent for %s had no recorded conversation to carry; it starts with a fresh context", repoRoot)
		case created == nil:
			// The provable agent-change fallback: the root now runs a different
			// agent, so its prior conversation cannot be resumed at all. Naming both
			// agents is what makes a repointed root_agents program diagnosable.
			log.WarningLog.Printf("re-created root agent for %s now runs %s, so its prior %s conversation %s cannot be resumed; it starts with a fresh context",
				repoRoot, launchedAgent, carried.Agent, carried.ID)
		default:
			log.WarningLog.Printf("re-created root agent for %s did not come up on its prior %s conversation %s; it starts with a fresh context",
				repoRoot, carried.Agent, carried.ID)
		}
	}
}

// deliverToReemergingRoot handles a DeliverPrompt whose absent target is this
// repo's daemon-managed root agent, momentarily gone while the ensure loop
// re-materializes it in place after a tmux outage (#1223). It waits for the
// ensure loop to bring root back (bounded by targetDeliverWait, mirroring the
// concurrent-create retry) and then sends the prompt into it, so a watch/
// monitor event is delivered once root returns instead of being dropped by the
// reserved-name guard the auto-create path would hit. Returns handled=false
// when the target is not a reserved title at all, or when the repo is simply
// unconfigured — there the reserved-name guard's "add it to root_agents"
// advice is the correct answer. Every other refusing verdict is answered HERE
// with its cause (#3264): falling through would tell a user whose repo IS in
// root_agents to add it to root_agents, while the actual blocker — a disable,
// an unloadable personal config, an unlistable registry, a deleted project —
// went unnamed. On a wait timeout it returns handled=true with an accurate
// "being recreated" error rather than the misleading reserved-name one.
func (m *Manager) deliverToReemergingRoot(repo *config.RepoContext, req DeliverPromptRequest) (string, session.PromptDeliveryStatus, bool, error) {
	if !session.IsReservedTitle(req.Title) {
		return "", session.PromptCouldNotConfirm, false, nil
	}
	if req.Title != session.RootSessionTitle {
		// A reserved-title VARIANT ("Root", " root ") can never be delivered
		// to: the ensure loop creates only the exact title, so no root-agent
		// policy fix makes this spelling deliverable. Fall through to the
		// reserved-name guard, whose "pick another name" is the right advice —
		// answering with a policy cause here would promise a remedy that
		// cannot work (#3264 review). This also stops the wait path from
		// waiting out targetDeliverWait for a title that will never appear.
		return "", session.PromptCouldNotConfirm, false, nil
	}
	verdict := m.rootAgentMaterializeVerdictFor(repo.ID)
	switch verdict.reason {
	case rootAgentWillMaterialize:
		// Fall through to the wait-and-send path below.
	case rootAgentNotConfigured:
		return "", session.PromptCouldNotConfirm, false, nil
	default:
		// Pre-flight refusal: nothing was sent, so the rate slot is refunded
		// (#2501), and the error names what actually stops the root.
		return "", session.PromptCouldNotConfirm, true, notAttempted(fmt.Errorf("root agent for %q will not materialize: %s; %s", repo.Root, rootAgentUnavailableDetail(verdict), notDeliveredMarker))
	}
	if err := m.waitForTargetSession(repo.ID, req.Title); err != nil {
		// Pre-flight: the root never reappeared within the wait, so nothing was
		// sent — refund the rate slot (#2501). This is the reserved-root outage
		// path a monitor task targeting `root` hits during a tmux blip.
		return "", session.PromptCouldNotConfirm, true, notAttempted(fmt.Errorf("root agent for %q is being recreated (tmux momentarily absent); %s this attempt: %w", repo.Root, notDeliveredMarker, err))
	}
	// A TUI can attach to root during the wait above, so re-check the defer lease
	// before sending — otherwise this path pastes into an attached pane the
	// "exists" path would have deferred (#1638).
	if m.deferWhileAttached(repo.ID, req) {
		return StatusDeferredAttached, session.PromptNotDelivered, true, nil
	}
	status, err := m.SendPromptWithStatus(SendPromptRequest{Title: req.Title, RepoID: repo.ID, Prompt: req.Prompt})
	if err != nil {
		return "", session.PromptCouldNotConfirm, true, err
	}
	return "sent", status, true, nil
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
	// notice is an unacknowledged re-create warning the reaped record still
	// carried (#2629) — a root healed twice before anyone looked at it. It floors
	// the replacement'"'"'s own verdict so the older, unseen loss is not erased by a
	// cleaner second heal.
	notice session.RootRecreateContext
}

// rootEnsureSucceeded resets a repo's retry state after a pass that left a
// healthy root in place (freshly created or adopted).
func (m *Manager) rootEnsureSucceeded(st *rootEnsureState) {
	m.mu.Lock()
	st.consecutiveFailures = 0
	st.unansweredFailures = 0
	st.escalated = false
	st.escalatedPersistent = false
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
// logged. Crossing rootEnsureEscalationThreshold logs an ERROR so a
// persistent cause is visible without waiting for a user to notice the
// missing root — worded for what the attempts actually established, and
// re-logged if that changes (#3500).
func (m *Manager) rootEnsureFailed(path string, st *rootEnsureState, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st.consecutiveFailures++
	if errors.Is(err, config.ErrRepoProbeUnanswered) {
		st.unansweredFailures++
	}
	backoff := rootEnsureBackoffFor(st.consecutiveFailures)
	st.nextAttempt = time.Now().Add(backoff)
	if rootEnsureShouldEscalate(st) {
		st.escalated = true
		st.escalatedPersistent = rootEnsureCauseIsEstablished(st)
		log.ErrorLog.Printf("root agent ensure for %q failed %d consecutive times; %s — will keep retrying every %s: %v", path, st.consecutiveFailures, rootEnsureEscalationCause(st), rootEnsureBackoffMax, err)
		return
	}
	log.WarningLog.Printf("root agent ensure for %q failed (attempt %d), retrying in %s: %v", path, st.consecutiveFailures, backoff, err)
}

// rootEnsureAnsweredFailures counts the failures in the current streak that
// produced a real error rather than ending before git could answer. Caller
// holds m.mu.
func rootEnsureAnsweredFailures(st *rootEnsureState) int {
	return st.consecutiveFailures - st.unansweredFailures
}

// rootEnsureCauseIsEstablished reports whether the streak has the evidence a
// persistence claim needs: a full threshold of failures that actually reported
// something. It is the one predicate both the escalation decision and its
// wording read, so the two cannot drift apart. Caller holds m.mu.
func rootEnsureCauseIsEstablished(st *rootEnsureState) bool {
	return rootEnsureAnsweredFailures(st) >= rootEnsureEscalationThreshold
}

// rootEnsureShouldEscalate decides whether a streak gets an escalation ERROR
// now. Once when it crosses the threshold, plus at most once more for the one
// transition that changes what the ERROR may claim: a streak escalated as
// "cause unknown" whose failures LATER start answering has established a real
// persistent cause, and the old strict equality on the threshold could never
// report it — the count is already past the threshold, so the genuine cause
// would be logged as warnings forever while the root stayed down (#3500
// review).
//
// The trigger and the CLAIM are deliberately separate. Visibility is owed after
// a threshold of consecutive failures whatever they were — the root has been
// down that long either way — but "the cause looks persistent" is owed only
// once a threshold of failures has actually reported something. So the first
// ERROR always fires on the count, worded for the evidence, and the upgrade
// fires once that evidence arrives.
//
// The bar is a full threshold rather than a single answered failure because
// this path does not see only repo probes: rootEnsureFailed also records a
// failed session create and a failed dead-root reap, neither of which carries
// the unanswered sentinel. One transient tmux failure must not turn "cause
// unknown" into "looks persistent" (#3500 review round 2) — and a MIXED first
// streak must not either, nor lock itself out of the upgrade by having claimed
// persistence on that one failure (round 3).
//
// Bounded at two ERRORs per streak: escalatedPersistent only ever goes false to
// true, since an answered failure is never un-answered later in the same
// streak. Caller holds m.mu.
func rootEnsureShouldEscalate(st *rootEnsureState) bool {
	if !st.escalated {
		return st.consecutiveFailures >= rootEnsureEscalationThreshold
	}
	return !st.escalatedPersistent && rootEnsureCauseIsEstablished(st)
}

// rootEnsureEscalationCause words what the escalation ERROR is entitled to
// claim about the cause. Attempts whose repo probe went unanswered (#3500)
// still count toward the backoff — they must, since the retry cadence is what
// keeps a loaded box from forking git every tick, and #1122's retry-forever
// contract is unchanged — but they are not evidence of anything: an attempt
// that never got an answer out of git has established nothing about the
// repository or the configuration. Caller holds m.mu.
func rootEnsureEscalationCause(st *rootEnsureState) string {
	answered := rootEnsureAnsweredFailures(st)
	switch {
	case answered == 0:
		return "no attempt got an answer out of git, so the cause is unknown; a repo probe that keeps dying says nothing about the repository or its configuration"
	case !rootEnsureCauseIsEstablished(st):
		return fmt.Sprintf("only %d of those attempts reported a real error and the rest ended before git could answer, so the cause is not established", answered)
	case st.unansweredFailures > 0:
		return fmt.Sprintf("the cause looks persistent, though %d of those attempts ended before git could answer", st.unansweredFailures)
	default:
		return "the cause looks persistent"
	}
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
	repo, err := config.RepoFromPath(repoRoot)
	if err == nil {
		var resolved *config.ResolvedConfig
		resolved, err = config.ResolveConfigForRepo(repo)
		if err == nil {
			program = config.ResolveProgram(&resolved.Config, "claude")
		}
	}
	if err != nil {
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
