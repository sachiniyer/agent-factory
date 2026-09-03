package daemon

import (
	"context"
	"fmt"
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
	projectRoots       map[string]resolvedProjectRoot
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
	unresolvedRoots map[string]unresolvedProjectRecord
	// reconcileOwed maps a project ID to the durable identity work its startup
	// pass could not finish (#3530 review ids 3916912922, 3918535472). It is a
	// latch, not bookkeeping: a project whose path resolves never enters
	// unresolvedRoots, so without it the heal pass has no reason to run at all
	// and the backfill is never retried for the life of the daemon. If the
	// checkout disappears before the next restart, that restart addresses the
	// project by a provisional identity and loses the state stored under its
	// real one.
	//
	// Two shapes, and the difference decides what the retry may skip: the WRITE
	// failed after the proof was established, or the PROOF itself could not be
	// established (a marker read that timed out or was unreadable).
	reconcileOwed map[string]reconcileOwedEntry
	legacyRepoIDs map[string]bool
	// legacyRepoIDByPath is the per-root_agents-path provenance of
	// legacyRepoIDs: the repo ID each configured path last RESOLVED to. It
	// exists so a later recompute can tell an UNKNOWN resolution from an
	// ABSENT one (#3782 item 1) — the set alone cannot, because a path whose
	// probe never answered has no repo ID to look itself up by. Rebuilt in
	// the same statement as legacyRepoIDs, by the one function that derives
	// both, so the two cannot drift.
	legacyRepoIDByPath map[string]string
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
// reconcileOwedEntry is one project's unfinished durable identity work.
//
// proven says the exact-workspace-plus-marker proof already succeeded, so the
// retry must NOT re-derive it — a replacement checkout that took the path since
// would otherwise inherit the write. When it is false the proof is what failed,
// and the retry has to establish it before writing anything.
type reconcileOwedEntry struct {
	repoID string
	proven bool
}

func buildRootAgentSnapshot(cfg *config.Config) rootAgentSnapshot {
	snap := rootAgentSnapshot{
		global:             config.GlobalRootAgentLayer(cfg),
		personal:           map[string]*config.RootAgentLayer{},
		personalUnreadable: map[string]string{},
		projectRoots:       map[string]resolvedProjectRoot{},
		unresolvedRoots:    map[string]unresolvedProjectRecord{},
		legacyRepoIDs:      map[string]bool{},
	}

	// The UNBOUNDED entry point, deliberately, and only here. Bounding a
	// boot-time probe is a decision about what a daemon START owes an operator
	// whose configured checkout sits on a dead mount — refusing to start takes
	// every other session on the box with it — and that is #3782 item 2, not
	// this change. The heal recompute below it is on the poll goroutine and
	// takes the bounded one.
	snap.legacyRepoIDs, snap.legacyRepoIDByPath = legacyRepoIDSet(cfg, unboundedLegacyRootRepo, nil)

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
	// Boot: nothing is serving yet, so no delete can hold an identity.
	snap.personal, snap.personalUnreadable, snap.projectRoots, snap.unresolvedRoots, snap.reconcileOwed = projectRootAgentLayers(projects, nil)
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

// legacyRepoResolver resolves one root_agents key to its repository. The two
// callers of legacyRepoIDSet sit on different lifecycles — daemon start and the
// instance poll goroutine — so the entry point is the caller's to choose rather
// than the function's to assume.
type legacyRepoResolver func(path string) (*config.RepoContext, error)

// unboundedLegacyRootRepo is config.RepoFromPath behind the legacy sweep's tilde
// expansion: context.Background(), which is literally what config.RepoFromPath
// passes its context-taking twin. Routed through the same legacyRootRepoFromPath
// seam as the bounded resolver so a test can stall one configured path at either
// call site without a process-global `git` shim on PATH.
func unboundedLegacyRootRepo(path string) (*config.RepoContext, error) {
	return legacyRootRepoFromPath(context.Background(), config.ExpandTilde(path))
}

// legacyRepoIDSet resolves each root_agents path to its repo ID for the
// singleton sweep's dedup set, returning that set and the per-path resolutions
// it was derived from. A not-yet-cloned legacy path is normal (#1122): the
// per-path ensure sweep retries it, and it is simply not part of the dedup set
// until it resolves — while it does not resolve it cannot collide with a
// registered project that did. Shared by the start-of-day builder and the
// registry heal, which must RECOMPUTE it (#3315 review): a legacy path that
// resolved only after boot would otherwise be missing from the healed
// snapshot's dedup set, letting the singleton sweep double-visit its repo
// behind a failing legacy attempt.
//
// previous is the last recompute's per-path resolutions, and it is what keeps
// the bound from re-entering that same #3315 double-visit through a timeout. An
// unanswered probe is UNKNOWN, and AN UNKNOWN MUST NEVER READ AS ABSENT: git
// answering "not a repository" is a verdict about the path and may drop the
// entry, but a probe that never answered establishes nothing, so the repo ID the
// path last resolved to stands until a probe answers. Dropping it instead would
// mean one stalled mount silently un-dedups a repo whose root_agents opt-in has
// been sitting there all along — #3500's rule (never convert "could not
// establish" into a verdict), applied to a set membership rather than a log
// line.
func legacyRepoIDSet(cfg *config.Config, resolve legacyRepoResolver, previous map[string]string) (map[string]bool, map[string]string) {
	ids := map[string]bool{}
	byPath := map[string]string{}
	for path := range cfg.RootAgents {
		repo, err := resolve(path)
		switch {
		case err == nil:
			ids[repo.ID] = true
			byPath[path] = repo.ID
		case !config.RepoProbeUnanswered(err):
			// A VERDICT. git ran and answered that the path is not a
			// repository right now — #1122's not-yet-cloned entry, or a
			// checkout that went away — and a verdict about the path may drop
			// the entry, exactly as it always has.
		case previous[path] != "":
			// UNKNOWN. Nothing was established, so the last resolution stands.
			ids[previous[path]] = true
			byPath[path] = previous[path]
		default:
			// UNKNOWN, and nothing to stand: this path has never resolved, so
			// it was never in the set to begin with.
		}
	}
	return ids, byPath
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
// unresolvedProjectRecord retains what re-attribution needs from a project
// whose recorded root did not resolve: the recorded path, and the identity
// evidence (#3299 review) — the project ID and the checkout marker id that
// proves a returning path holds the SAME clone, not a different checkout
// reusing it.
// resolvedProjectRoot is the singleton sweep's binding for one registered
// project whose recorded root RESOLVED: the path an in-place root agent runs
// at, plus the identity evidence that path was accepted on. Both halves are
// kept because the sweep needs them at different moments — the root at every
// visit, the identity only at a create (#3366).
//
// A path alone was the defect. The snapshot binds a repo ID to a create path
// once (at boot, or on re-attribution) and every later create, heal and
// kill-grace expiry trusted that binding for the rest of the daemon run: the
// checkout could be removed and a different clone put at the path, and the
// autonomous root would start there — under the registered project's personal
// layer — having never been re-proven to be the project's own checkout.
// Carrying the marker id here is what lets the create boundary re-prove it.
type resolvedProjectRoot struct {
	// root is the RECORDED root, exactly as projectRootAgentLayers and the
	// re-attribution acceptance publish it: identity comes from the repo ID,
	// but an in-place root agent runs at the checkout the user registered
	// (#3361's identity/workspace boundary).
	root string
	// projectID names the registry record, so a refusal can tell the user
	// which project to rebind.
	projectID string
	// checkoutID is the registry's identity proof for that record — the marker
	// a checkout at root must carry to be the SAME clone the project was
	// registered from (#3299/#3334).
	checkoutID string
}

type unresolvedProjectRecord struct {
	root       string
	projectID  string
	checkoutID string
	// identityMismatch records that the recorded path RESOLVES, the marker
	// READ SUCCEEDED, and the checkout there does not carry the project's
	// marker — a different clone occupying the path. Consumers word the
	// remedy differently: an absent path needs bringing back, a mismatched
	// one needs a rebind and a daemon restart (#3299 review round 4).
	identityMismatch bool
	// markerUnreadable records that the recorded path RESOLVES but the
	// marker could not be READ (permissions, I/O): identity is unknowable,
	// which is neither absence nor a proven mismatch — prescribing a rebind
	// there could destroy a transiently unreadable original (#3299 review
	// round 5).
	markerUnreadable bool
	// pathVanished narrows markerUnreadable further: the path itself
	// disappeared mid-verification, so renderers prescribe restoring the
	// path rather than fixing marker readability (#3299 review round 12).
	// The fail-closed gating is markerUnreadable's; this only words it.
	pathVanished bool
	// identityPass is the heal pass whose probe result last ESTABLISHED the
	// three flags above, and it is what makes them datable (#3611). Zero means
	// no probe result has ever established them — the boot builder files a
	// record with no verdict at all, and a registry-recovery rebuild replaces
	// every record with a fresh one.
	//
	// It exists because one field is asked for two different things and both
	// asks are right. Preservation wants the last verdict kept UNTIL
	// SUPERSEDED: an evidence-free retry may not clear a proven mismatch,
	// because that mismatch is the only thing keeping a dead project's personal
	// layer off the different clone now at its path (#3299 review id
	// 3910519842). Release wants the same verdict only while it is CURRENT: a
	// tombstone released on a mismatch nobody has re-proved acts on whatever
	// checkout is at the path now, which may be the deleted project's own one
	// come back. Neither consumer can be fixed by another conditional on the
	// flag; what was missing is freshness on the evidence, so each consumer can
	// state its own tolerance against this mark.
	//
	// A PASS NUMBER rather than a timestamp, deliberately: it is monotonic, it
	// needs no clock, and it lets a consumer say "verified in the current or
	// previous pass" without inventing a duration constant that would then have
	// to track the probe cadence.
	identityPass uint64
}

// identityWriteFence supplies the predicate ReconcileProjectRepoID re-asks
// under the registry lock. Nil means "nothing can be holding an identity",
// which is true only of the boot snapshot: the daemon is not serving yet, so no
// delete exists to fence one (#3530 review id 3920258554). Every RUNTIME caller
// — the registry-recovery rebuild in healRootAgentLayers is one — must pass the
// manager's, or a write can land while a delete holds the identity.
type identityWriteFence func(from, to string) func() bool

func projectRootAgentLayers(projects []config.Project, fence identityWriteFence) (personal map[string]*config.RootAgentLayer, personalUnreadable map[string]string, projectRoots map[string]resolvedProjectRoot, unresolvedRoots map[string]unresolvedProjectRecord, reconcileOwed map[string]reconcileOwedEntry) {
	personal = map[string]*config.RootAgentLayer{}
	personalUnreadable = map[string]string{}
	projectRoots = map[string]resolvedProjectRoot{}
	unresolvedRoots = map[string]unresolvedProjectRecord{}
	reconcileOwed = map[string]reconcileOwedEntry{}
	for _, p := range projects {
		var repoID, repoRoot string
		if repo, repoErr := config.RepoFromPath(p.Root); repoErr == nil {
			repoID, repoRoot = repo.ID, repo.Root
			// Repo identity comes from repo.ID, but an in-place root agent runs
			// at the registered checkout. Keep that recorded root explicit: the
			// pre-#3358 resolver substituted the non-repository parent of a bare
			// common directory here (#3361). The ensure-cadence re-attribution
			// path publishes the same recorded root on acceptance, so a project
			// that resolves mid-run gets the create a boot resolution would
			// have (#3299) — reattributeUnresolvedRoots states that parity.
			// Write the identity down the FIRST time this record is seen to
			// resolve, not only when re-attribution runs (#3530 review id
			// 3914971739). A record written before RepoID existed whose path
			// resolves at daemon start never enters unresolvedRoots, so the
			// re-attribution path never reaches it — and it would switch to a
			// provisional identity the moment its path went away, losing
			// sessions and policy keyed under the real one. Idempotent: the
			// writer is a no-op once the identity is recorded.
			if p.RepoID == "" {
				// AVAILABILITY IS NOT IDENTITY, and this write is permanent —
				// the one-way writer will never replace it (#3530 review id
				// 3915518804). RepoFromPath succeeding proves a repository is
				// reachable at the recorded path, not that it is the
				// registered checkout: a replacement clone answers, and a
				// vanished nested root resolves UPWARD into whatever encloses
				// it. Either would bind this project to a stranger forever,
				// and every later missing-path delete and personal-policy
				// decision would target that stranger.
				//
				// ResolveRegisteredProjectRepoID is the proof that exists for
				// this: exact-workspace match plus the record's own checkout
				// marker. Unproven simply means not yet — the project stays
				// provisional and the next pass tries again.
				proven, ok := config.ResolveRegisteredProjectRepoID(context.Background(), p)
				switch {
				case ok && proven == repoID:
					if _, err := config.ReconcileProjectRepoID(p.ID, repoID, identityWriteWanted(fence, repoID)); err != nil {
						// Keep the work, or there is none left to retry: this
						// project resolves, so it never joins unresolvedRoots
						// and the heal pass would return before reaching
						// anything (#3530 review id 3916912922). The proof has
						// already been established here — the latch carries the
						// identity it established, and the retry only re-does
						// the WRITE.
						reconcileOwed[p.ID] = reconcileOwedEntry{repoID: repoID, proven: true}
						log.WarningLog.Printf("root agent snapshot: project %s resolves to repo %s but its identity could not be recorded; retrying on the ensure cadence — until it succeeds, a path that goes away falls back to a provisional identity: %v", p.ID, repoID, err)
					}
				case ok:
					// The proof named a DIFFERENT identity than the resolution
					// a moment earlier did — the same marked checkout, its
					// repository's identity root moved in between (#3530 review
					// id 3919604357). Handling only equality published the
					// project and its personal policy under the stale one with
					// NOTHING latched, and a resolved project never enters
					// unresolvedRoots, so no pass would ever revisit it: a
					// legacy opt-in resolving the proven identity could then
					// start without the project's disable. The proof wins,
					// because it is the evidence about which checkout this is.
					repoID, repoRoot = proven, p.Root
					if _, err := config.ReconcileProjectRepoID(p.ID, repoID, identityWriteWanted(fence, repoID)); err != nil {
						reconcileOwed[p.ID] = reconcileOwedEntry{repoID: repoID, proven: true}
						log.WarningLog.Printf("root agent snapshot: project %s's checkout is verified under %s rather than the identity its path resolved to, but that could not be recorded; retrying on the ensure cadence: %v", p.ID, repoID, err)
					}
				case !ok:
					// The PROOF is what failed — a marker read that timed out
					// or could not be read — and that is just as unfinished as
					// a failed write (#3530 review id 3918535472). Latched
					// UNPROVEN, so the retry re-establishes it rather than
					// inheriting a claim about a checkout it never verified.
					reconcileOwed[p.ID] = reconcileOwedEntry{repoID: repoID}
					log.WarningLog.Printf("root agent snapshot: project %s resolves to repo %s but its checkout could not be verified as that project's own, so its identity is not recorded yet; re-checking on the ensure cadence", p.ID, repoID)
				}
			}
			//
			// The record's checkout id rides along so the create boundary can
			// re-prove the checkout at that path is still this project's own,
			// rather than trusting a binding made once at boot (#3366).
			projectRoots[repoID] = resolvedProjectRoot{root: p.Root, projectID: p.ID, checkoutID: p.CheckoutID}
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
			// enabled=false sat readable in the AF home the whole time.
			//
			// The identity comes from the RECORD, not from hashing the path
			// (#3530). A project writes down the repository it resolved to
			// (Project.RepoID), so an absent path is still attributed to its
			// own repository — including when the recorded root is not the
			// repository's identity root, which is the residue #3334 had to
			// defer. Hashing the path instead would attribute the layer to
			// whatever repository turns up there, which is the collision this
			// namespace split removes.
			//
			// Only a record written before RepoID existed falls back, and it
			// falls back to a value NO repository can hold, so it reaches
			// nothing until the ensure cadence resolves it and writes the real
			// identity down.
			repoID = config.ReconciledRepoIDForProject(p)
			repoRoot = p.Root
			unresolvedRoots[repoID] = unresolvedProjectRecord{root: p.Root, projectID: p.ID, checkoutID: p.CheckoutID}
			// The claim is split at the resolution boundary (#3500): a probe
			// git never answered is a subprocess outcome, not a verdict on the
			// recorded root. The HANDLING is the same either way — the path is
			// unusable for this pass whichever it was — and what settles it is
			// the ensure-cadence re-check below (#3299), so only the wording
			// comes from the classifier.
			log.WarningLog.Printf("root agent snapshot: %s; its personal layer still applies to that path, a legacy root_agents entry for the same repo keeps its per-tick retry, and the daemon re-checks the recorded path on its ensure cadence — the project resumes fully, under its real repo identity, once the path resolves: %v", repoResolveClaim(fmt.Sprintf("project %s root", p.ID), p.Root, repoErr), repoErr)
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
	return personal, personalUnreadable, projectRoots, unresolvedRoots, reconcileOwed
}

// identityWriteWanted adapts an identityWriteFence for one identity, and treats
// a nil fence as "wanted" — the boot case, where nothing can hold one.
func identityWriteWanted(fence identityWriteFence, repoID string) func() bool {
	if fence == nil {
		return nil
	}
	return fence(repoID, repoID)
}
