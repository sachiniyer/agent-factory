package daemon

import (
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The start-of-day root-agent snapshot (#2216 Phase 6) and its builders,
// split from rootagent.go along the snapshot concern (#1145 lint). The
// snapshot value is immutable; healRootAgentLayers republishes narrowed
// copies through Manager.rootAgentLayers (#3264), and the fail-closed
// granularity rules live at the reads they gate (#3241, #3247, #3297).

// rootAgentSnapshot is the start-of-day root-agent configuration the ensure
// loop resolves against (#2216 Phase 6): the global [root_agent] singleton,
// every registered project's personal [root_agent] layer (keyed by repo ID),
// the repos whose root-agent decision is unknowable (personalUnreadable,
// registryUnreadable — the #3241/#3247 fail-closed causes), the resolved roots
// the singleton sweep visits, and the repo IDs the legacy root_agents map
// already covers (so the singleton sweep can dedupe against it).
type rootAgentSnapshot struct {
	global   *config.RootAgentLayer
	personal map[string]*config.RootAgentLayer
	// personalUnreadable maps a fail-closed repo ID to its project ID, so
	// consumer-facing refusals can name the config file to fix (#3264).
	personalUnreadable map[string]string
	projectRoots       map[string]string
	// recordFailureIDs names the registry record directories the snapshot
	// could not read (#3297). Their repos are unattributable — the root path
	// lives inside the unreadable record — so no per-repo latch can carry
	// them; verdict consumers name them instead, because telling a repo with
	// no readable config to "add a root_agents entry" misdirects when one of
	// these records may be that repo's (#3316 review).
	recordFailureIDs []string
	// unresolvedRoots maps the derived repo ID of each registered project whose
	// recorded root did not resolve at snapshot time to that recorded path
	// (#3247 arm 2). The singleton sweep cannot visit these (projectRoots
	// deliberately excludes them), but they are still CONFIGURED projects —
	// their layers sit in personal/personalUnreadable — so consumer verdicts
	// must not call them unconfigured and advise adding config that already
	// exists (#3264 review).
	unresolvedRoots    map[string]string
	legacyRepoIDs      map[string]bool
	registryUnreadable bool
}

// buildRootAgentSnapshot reads the registry once at daemon start, matching the
// RootAgents map's restart-to-apply contract: registering a project or editing
// its personal root_agent takes effect on the next daemon start. It is
// best-effort for the parts a failure provably cannot hide a disable in, and
// fail-closed for the rest (#3241, #3247): a personal config that exists but
// cannot be LOADED, a project registry that cannot be LISTED, and a recorded
// project root that does not resolve may each conceal the highest-precedence
// enabled=false, so none of them may quietly become "no personal layer".
func buildRootAgentSnapshot(cfg *config.Config) rootAgentSnapshot {
	snap := rootAgentSnapshot{
		global:             config.GlobalRootAgentLayer(cfg),
		personal:           map[string]*config.RootAgentLayer{},
		personalUnreadable: map[string]string{},
		projectRoots:       map[string]string{},
		unresolvedRoots:    map[string]string{},
		legacyRepoIDs:      map[string]bool{},
	}

	snap.legacyRepoIDs = legacyRepoIDSet(cfg)

	projects, failures, strays, _, err := config.ListProjectsDetailed()
	if err != nil {
		// Fail CLOSED, registry-wide, for a failed ENUMERATION only (#3247,
		// granularity per #3297): with the record list itself unknown, NO repo
		// — including one named only by a legacy root_agents entry — can be
		// proven un-disabled, and none may start or heal. Per-record failures
		// take the per-record branch below instead; an absent registry lists
		// as zero projects — so this is always a real enumeration failure,
		// never plain absence (the home-gone vs unreadable distinction #3246
		// keeps).
		registry := config.ProjectRegistryDirName
		if dir, dirErr := config.ProjectRegistryDir(); dirErr == nil {
			registry = dir
		}
		log.ErrorLog.Printf("root agent snapshot: cannot enumerate the project registry (%s); failing closed — no root agents will be started or healed until the registry is readable again (re-checked on the ensure cadence): %v", registry, err)
		snap.registryUnreadable = true
		return snap
	}
	logRegistryRecordProblems(failures, strays)
	snap.recordFailureIDs = recordFailureDirectoryIDs(failures)
	snap.personal, snap.personalUnreadable, snap.projectRoots, snap.unresolvedRoots = projectRootAgentLayers(projects)
	return snap
}

// recordFailureDirectoryIDs projects the failed record directory names for
// the snapshot and its verdict consumers.
func recordFailureDirectoryIDs(failures []config.ProjectRecordFailure) []string {
	if len(failures) == 0 {
		return nil
	}
	ids := make([]string, 0, len(failures))
	for _, failure := range failures {
		ids = append(ids, failure.DirectoryID)
	}
	return ids
}

// legacyRepoIDSet resolves each root_agents path to its repo ID for the
// singleton sweep's dedup set. A not-yet-cloned legacy path is normal
// (#1122): the per-path ensure sweep retries it, and it is simply not part of
// the dedup set until it resolves — while it does not resolve it cannot
// collide with a registered project that did. Shared by the start-of-day
// builder and the registry heal, which must RECOMPUTE it (#3315 review): a
// legacy path that resolved only after boot would otherwise be missing from
// the healed snapshot's dedup set, letting the singleton sweep double-visit
// its repo behind a failing legacy attempt.
func legacyRepoIDSet(cfg *config.Config) map[string]bool {
	ids := map[string]bool{}
	for path := range cfg.RootAgents {
		repo, err := config.RepoFromPath(config.ExpandTilde(path))
		if err != nil {
			continue
		}
		ids[repo.ID] = true
	}
	return ids
}

// logRegistryRecordProblems names each registry record the snapshot had to
// suppress and any stray files it ignored. THE GRANULARITY RULE (#3297,
// stated at the read in config.ListProjectsDetailed): a record that cannot be
// read suppresses only ITS OWN project — its root path lives inside the
// unreadable record, so the suppression cannot be keyed to a repo, and a
// legacy root_agents entry for the same repo still applies as its own opt-in
// (the accepted residue). A stray file suppresses nothing: enumeration
// succeeded and every real record was read. Only a failed enumeration fails
// the machine closed — one bad record must not become a machine-wide
// root-agent outage.
func logRegistryRecordProblems(failures []config.ProjectRecordFailure, strays []string) {
	for _, failure := range failures {
		log.WarningLog.Printf("root agent snapshot: project registry record %s cannot be read; only that project is affected — its personal [root_agent] layer is unreachable and the singleton sweep cannot ensure it, though a legacy root_agents entry for the same repo still applies as its own opt-in — until the record is repaired (or removed) and the daemon restarts: %v", failure.DirectoryID, failure.Err)
	}
	if len(strays) > 0 {
		log.WarningLog.Printf("root agent snapshot: project registry contains %d non-record file(s) (%s); they affect nothing and can be removed", len(strays), strings.Join(strays, ", "))
	}
}

// projectRootAgentLayers derives the registry-dependent half of the snapshot
// from one successful ListProjects read: each project's personal layer and
// resolved root, and the fail-closed set for personal configs that exist but
// do not load. Shared by the start-of-day builder and the safe-direction heal
// (#3264), so a healed registry read produces exactly the snapshot a daemon
// start would have.
func projectRootAgentLayers(projects []config.Project) (personal map[string]*config.RootAgentLayer, personalUnreadable, projectRoots, unresolvedRoots map[string]string) {
	personal = map[string]*config.RootAgentLayer{}
	personalUnreadable = map[string]string{}
	projectRoots = map[string]string{}
	unresolvedRoots = map[string]string{}
	for _, p := range projects {
		var repoID, repoRoot string
		if repo, repoErr := config.RepoFromPath(p.Root); repoErr == nil {
			repoID, repoRoot = repo.ID, repo.Root
			projectRoots[repoID] = repo.Root
		} else {
			// The recorded root does not resolve right now — an absent mount, a
			// checkout deleted or no longer a git repository. The singleton sweep
			// cannot visit it (there is nothing to create a session at, so it
			// stays out of projectRoots), but the personal config still lives in
			// the AF home under p.ID, and repo identity is a pure function of the
			// main-root path (RepoIDFromRoot), so the layer is attributed to the
			// ID a checkout resolving at the recorded path gets. Skipping the
			// project instead is fail-open (#3247): the legacy sweep's per-tick
			// retry (#1122) resolves the repo the moment the path returns and
			// would ensure it with no personal layer, starting a root whose
			// enabled=false sat readable in the AF home the whole time. The
			// residue is a recorded root that is not the main worktree's
			// toplevel — a linked worktree of a bare clone, a subdirectory
			// registration, or a spelling that later re-resolves through a
			// symlink: there the derived ID cannot match what the sweep
			// resolves, and the layer applies only at a daemon start after the
			// path returns.
			repoID = config.RepoIDForRecordedRoot(p.Root)
			repoRoot = p.Root
			unresolvedRoots[repoID] = p.Root
			log.WarningLog.Printf("root agent snapshot: project %s root %s does not resolve to a git repository; the [root_agent] singleton alone starts nothing for it this run, but its personal layer still applies to that path — a legacy root_agents entry for the same repo picks the layer up the moment the path resolves: %v", p.ID, p.Root, repoErr)
		}
		pc, err := config.LoadProjectConfig(p.ID)
		if err != nil {
			// Fail CLOSED (#3241): this file may hold the highest-precedence
			// `enabled = false` — for a parse or read failure we provably cannot
			// know — so the failed load makes the project's root-agent decision
			// unknown. It must not become "absent", or a lower-precedence enable
			// (the ubiquitous empty legacy root_agents entry, or a global
			// enabled=true) starts a root the user explicitly disabled. Recording
			// the repo in personalUnreadable makes the snapshot's resolve — the
			// one resolution choke point both ensure sweeps and
			// rootAgentMaterializeVerdictFor share — resolve it to disabled. An
			// already-live root is left alone (adopt-first); only creation and
			// healing stop, until the config loads again (the ensure cadence
			// re-attempts the read, #3264 — a still-failing read stays closed).
			log.WarningLog.Printf("root agent snapshot: project %s (%s) personal config cannot be loaded; failing closed — no root agent will be started or healed for this repo until its config loads again (re-checked on the ensure cadence): %v", p.ID, repoRoot, err)
			personalUnreadable[repoID] = p.ID
			continue
		}
		if layer := pc.RootAgentLayer(); layer != nil {
			personal[repoID] = layer
		}
	}
	return personal, personalUnreadable, projectRoots, unresolvedRoots
}
