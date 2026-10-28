package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/sachiniyer/agent-factory/config"
)

// healRootAgentLayers is the safe-direction self-heal for the root-agent
// snapshot's fail-closed latches (#3264): while the snapshot carries unknowns
// — a registry that could not be listed (#3247), personal configs that could
// not be loaded (#3241) — the ensure cadence re-attempts exactly those READS,
// and a success replaces "unknown" with the config's true answer. It can only
// narrow the fail-closed set, never widen or reinterpret it: a still-failing
// read leaves the latch closed, and a now-readable enabled=false resolves to
// a provenanced disable rather than a start.
//
// This is not an exception to restart-to-apply. A config that was READ at
// daemon start stays applied as read, edits and all, until the next start;
// the reads retried here FAILED at start, so the first success IS their
// snapshot read — the one the boot intended to take. The registry likewise
// freezes on its first successful list (rebuilt through the same
// projectRootAgentLayers the boot uses); it is not re-read after that.
//
// Runs on the poll goroutine only (called from EnsureRootAgents), so a plain
// Store publishes each narrowed snapshot; the backoff state keeps a broken
// registry or config from being re-read every poll tick, pacing on the shared
// ensure curve and the injectable clock.
func (m *Manager) healRootAgentLayers() {
	layers := m.rootAgentLayers.Load()
	if !layers.registryUnreadable && len(layers.personalUnreadable) == 0 &&
		len(layers.unresolvedRoots) == 0 && len(layers.reconcileOwed) == 0 &&
		// A root_agents path whose probe never answered is an unknown like any
		// other, and this pass is the only thing that re-attempts it (#3782
		// item 2). Without it a boot-time timeout stands for the life of the
		// daemon: the ensure sweep keeps retrying the CREATE, but nothing
		// revisits the dedup set, so the repo is covered by a provisional
		// identity that may never be replaced by the real one.
		len(layers.legacy.unknownPaths) == 0 {
		return
	}
	// Two independent cadences (#3299 review round 8). The personal-config and
	// reconcile retries pace on rootHealNextAttempt's failure backoff, and the
	// legacy root_agents recompute rides that same clock when it retries on
	// its own. The registry arm paces on a SEPARATE clock,
	// rootHealRegistryNextAttempt: the two-strike absence discipline (#3315)
	// needs its two observations SPACED by that backoff, and a legacy path
	// that narrows in the same pass as the streak's first observation resets
	// rootHealNextAttempt to now — so the registry's second read keeps its
	// OWN spacing, or the two-strike mount-flap protection collapses to the
	// per-tick cadence. Re-attribution runs on every pass unconditionally:
	// its pacing is per entry (a settled negative rests until its own
	// retryAt; an in-flight probe is checked without blocking), so an idle
	// visit costs a map walk, not filesystem work, and a probe landing
	// between ticks is consumed on the next tick.
	m.mu.Lock()
	due := !nowFunc().Before(m.rootHealNextAttempt)
	registryDue := !nowFunc().Before(m.rootHealRegistryNextAttempt)
	m.mu.Unlock()

	healed := *layers
	changed := false
	// rpAttempted/rpChanged track only the backoff-paced reads on the SHARED
	// clock (personal-config, reconcile, legacy root_agents); the clock below
	// must not move when this pass did none of them. The registry arm carries
	// its OWN pair (registryAttempted/registryCommitted) below — a legacy
	// narrowing that resets the shared clock must not collapse the registry
	// streak's two-strike spacing.
	rpAttempted := false
	rpChanged := false
	registryAttempted := false
	registryCommitted := false

	if layers.registryUnreadable {
		// The registry arm paces on its OWN backoff clock
		// (rootHealRegistryNextAttempt), not the shared one — see the
		// two-independent-cadences note above. When it is not due the read is
		// skipped, but the pass does NOT early-return: a legacy root_agents
		// path that is due on the shared clock may still narrow below, on its
		// own cadence, and that narrowing must not wait on the registry's.
		if registryDue {
			registryAttempted = true
			// A latched registry PROVABLY existed at daemon start — plain absence
			// never sets the latch — so during recovery an ABSENT directory is a
			// transition, not proof of zero projects: a repair mv in flight, a
			// mount blip. ListProjectsDetailed makes that distinction explicit
			// and binds an empty result to a present registry. On top of that,
			// recovery publishes only on the SECOND consecutive MATCHING present-
			// and-listable snapshot, one backoff cadence apart — paced on THIS
			// arm's OWN clock, rootHealRegistryNextAttempt, so a legacy path that
			// narrows in the same pass cannot drag the second read onto the
			// per-tick cadence — and re-verifies presence after the dependent
			// personal-config reads — the applyHomeCheck two-strike discipline
			// (only a definite observation, twice consecutively), because a mount
			// flap inside a single pass can make removed-looking reads out of
			// files that are about to return (#3315 review, rounds 2-3). A flap
			// now has to defeat two spaced passes plus the post-read binding;
			// that residue is indistinguishable without filesystem transactions
			// and is accepted, in writing, here. Per-record failures and strays
			// take the #3297 granularity treatment exactly as at boot.
			if projects, failures, strays, present, err := config.ListProjectsDetailed(); err == nil && present {
				streak := m.observeRootHealRegistrySnapshot(projects)
				if streak >= 2 {
					logRegistryRecordProblems(m.warn(), failures, strays)
					// The RUNTIME rebuild, so it carries the fence (#3530 review id
					// 3920258554): this path proves legacy rows while the daemon is
					// serving, and a delete can hold the identity it would write.
					personal, personalUnreadable, projectRoots, unresolvedRoots, reconcileOwed := projectRootAgentLayers(m.warn(), projects, m.identityTransitionUnfenced)
					verifiedProjects, _, _, stillPresent, perr := config.ListProjectsDetailed()
					if perr == nil && stillPresent && sameRootHealRegistryProjects(projects, verifiedProjects) {
						healed.personal, healed.personalUnreadable, healed.projectRoots, healed.unresolvedRoots = personal, personalUnreadable, projectRoots, unresolvedRoots
						healed.reconcileOwed = reconcileOwed
						healed.recordFailureIDs = recordFailureDirectoryIDs(failures)
						healed.registryUnreadable = false
						changed = true
						registryCommitted = true
						m.resetRootHealRegistryObservation()
						m.info().Printf("root agent snapshot: project registry is readable again; resuming root-agent resolution with %d personal layer(s), %d project(s) still failing closed", len(healed.personal), len(healed.personalUnreadable))
					} else if perr == nil && stillPresent {
						// The post-read check is another valid observation. Retain it as
						// the new candidate, but require the next cadence to agree before
						// any latch is released.
						m.observeRootHealRegistrySnapshot(verifiedProjects)
					} else {
						m.resetRootHealRegistryObservation()
					}
				}
			} else {
				m.resetRootHealRegistryObservation()
			}
		}
	} else {
		if due && len(layers.personalUnreadable) > 0 {
			rpAttempted = true
			personal, personalUnreadable, healedCount := m.retryUnreadablePersonalConfigs(layers)
			if healedCount > 0 {
				healed.personal = personal
				healed.personalUnreadable = personalUnreadable
				changed = true
				rpChanged = true
			}
		}
		// Paced on the SAME backoff as the other retried reads (#3530 review id
		// 3918379041). A reconciliation that keeps failing — an unrelated
		// corrupt record makes the registry's strict load fail — would
		// otherwise reacquire the registry lock and reread every record on
		// every poll tick, which is exactly the contention this healer's
		// backoff contract exists to prevent. Re-attribution keeps its
		// unconditional cadence because its pacing is per entry; this is a
		// whole-registry read, so it belongs on the clock.
		if due && len(layers.reconcileOwed) > 0 {
			rpAttempted = true
			if m.retryReconcileOwed(&healed) {
				changed = true
				rpChanged = true
			}
		}
		reattrChanged, settles := m.reattributeUnresolvedRoots(&healed)
		if reattrChanged {
			changed = true
		}
		// Applied AFTER the publish below: a settle releases the
		// attribution-pending gate, which must not happen before the
		// snapshot carrying the corresponding bridge/flags is visible.
		defer func() {
			for _, settle := range settles {
				settle()
			}
		}()
	}

	// The recompute rides EVERY published heal, and is ALSO the standalone
	// retry for a root_agents path whose probe has not answered yet (#3782
	// item 2) — paced on the same backoff as the other retried reads, so a
	// permanently dead mount costs one bounded probe per cadence rather than
	// one per tick.
	legacyRetryDue := due && len(layers.legacy.unknownPaths) > 0
	if changed || legacyRetryDue {
		// Recompute the legacy dedup set on EVERY heal (#3315 review, both
		// rounds): a legacy path that resolved only after boot must dedup its
		// repo out of the singleton sweep in the published snapshot — whether
		// the heal was the registry or a personal config — or a failing
		// legacy attempt lets the singleton create the root without the
		// legacy layer.
		//
		// It runs on the POLL goroutine, so it takes the BOUNDED resolver
		// (#3782 item 1): the recompute is a scan, it admits nothing, and one
		// configured path on a stalled mount must not stop every pass below
		// EnsureRootAgents for every session on the box. Per path rather than
		// one shared budget across the loop, matching the sweep that follows
		// it, so a stalled path cannot starve the resolution of a path that
		// answers.
		//
		// The previous snapshot's per-path resolutions carry forward for
		// exactly the same reason this recompute exists: a probe that never
		// answered is UNKNOWN, and an unknown that dropped out of the set
		// would be #3315's double-visit re-entered through a deadline.
		healed.legacy = legacyRepoIDSet(m.cfg, resolveLegacyRootRepo, healed.legacy)
		if legacyRetryDue {
			rpAttempted = true
			// Only NARROWING is progress. A retry that leaves the same paths
			// unknown must charge the backoff, or a dead mount drags the
			// whole heal onto the per-tick cadence.
			if len(healed.legacy.unknownPaths) < len(layers.legacy.unknownPaths) {
				changed = true
				rpChanged = true
			}
		}
	}
	if changed {
		if rootHealPrePublishHookForTest != nil {
			rootHealPrePublishHookForTest()
		}
		m.rootAgentLayers.Store(&healed)
	}
	// The shared clock serves personal-config, reconcile, and legacy
	// root_agents only. The registry arm no longer touches it: a legacy
	// narrowing resets this clock to now, and pacing the registry's
	// two-strike streak on it would let the second read land one poll tick
	// later instead of one cadence later.
	if rpAttempted {
		m.mu.Lock()
		if rpChanged {
			m.rootHealFailures = 0
			m.rootHealNextAttempt = nowFunc()
		} else {
			m.rootHealFailures++
			m.rootHealNextAttempt = nowFunc().Add(rootEnsureBackoffFor(m.rootHealFailures))
		}
		m.mu.Unlock()
	}
	// The registry arm's OWN clock. A non-committing read (a first-observation
	// streak, a differing post-read, a failed or absent enumeration) charges a
	// full backoff; a commit resets it. Independent of rootHealNextAttempt, so
	// the legacy reset above cannot collapse the streak's two-strike spacing.
	if registryAttempted {
		m.mu.Lock()
		if registryCommitted {
			m.rootHealRegistryFailures = 0
			m.rootHealRegistryNextAttempt = nowFunc()
		} else {
			m.rootHealRegistryFailures++
			m.rootHealRegistryNextAttempt = nowFunc().Add(rootEnsureBackoffFor(m.rootHealRegistryFailures))
		}
		m.mu.Unlock()
	}
}

// observeRootHealRegistrySnapshot records one successful registry read. A
// changed project set starts a new streak: two individually successful reads
// cannot prove recovery when a mount transition made them observe different
// registries. config.ListProjects returns projects in registry-entry order.
// Agreement deliberately excludes Project.PathExists: it is recomputed from a
// live stat on every read, not stored in the registry, and projectRootAgentLayers
// resolves availability for itself when building the candidate snapshot.
func (m *Manager) observeRootHealRegistrySnapshot(projects []config.Project) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rootHealRegistryStreak == 0 || !sameRootHealRegistryProjects(m.rootHealRegistryProjects, projects) {
		m.rootHealRegistryProjects = slices.Clone(projects)
		m.rootHealRegistryStreak = 1
		return m.rootHealRegistryStreak
	}
	m.rootHealRegistryStreak++
	return m.rootHealRegistryStreak
}

func sameRootHealRegistryProjects(left, right []config.Project) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].ID != right[i].ID ||
			left[i].CheckoutID != right[i].CheckoutID ||
			left[i].Root != right[i].Root ||
			left[i].RelativeRoot != right[i].RelativeRoot ||
			// The recorded identity too (#3530 review id 3914971866): a
			// reconciliation landing between the two reads changes which id a
			// project is attributed under, and calling those snapshots
			// identical would freeze layers built from the provisional one
			// until restart.
			left[i].RepoID != right[i].RepoID {
			return false
		}
	}
	return true
}

func (m *Manager) resetRootHealRegistryObservation() {
	m.mu.Lock()
	m.rootHealRegistryStreak = 0
	m.rootHealRegistryProjects = nil
	m.mu.Unlock()
}

// retryUnreadablePersonalConfigs re-attempts LoadProjectConfig for every repo
// the snapshot holds fail-closed, returning rebuilt personal maps and how many
// healed. Failures stay in the set silently — the boot-time warning already
// named them, and re-warning on every retry would turn a broken file into log
// spam; only the heal is news.
//
// Two kinds of success, two rules. A CONTENT-BEARING read (the file parsed)
// heals immediately: a mount flap cannot fabricate parsed content. An
// ABSENCE-classified read (ENOENT, meaning "deliberately removed") is
// content-free and a vanished mount can spoof it, so it heals only on the
// second consecutive spaced observation of record-dir-present with the file
// absent — the applyHomeCheck two-strike discipline (#3315 review, rounds
// 2-3). Any other outcome resets that project's streak.
func (m *Manager) retryUnreadablePersonalConfigs(layers *rootAgentSnapshot) (map[string]*config.RootAgentLayer, map[string]string, int) {
	personal := make(map[string]*config.RootAgentLayer, len(layers.personal))
	for repoID, layer := range layers.personal {
		personal[repoID] = layer
	}
	personalUnreadable := make(map[string]string, len(layers.personalUnreadable))
	healedCount := 0
	for repoID, projectID := range layers.personalUnreadable {
		if !projectRecordDirPresent(projectID) {
			personalUnreadable[repoID] = projectID
			m.resetPersonalAbsenceStreak(projectID)
			continue
		}
		pc, err := config.LoadProjectConfig(projectID)
		if err != nil {
			personalUnreadable[repoID] = projectID
			m.resetPersonalAbsenceStreak(projectID)
			continue
		}
		if pc == nil {
			// ENOENT: absent only if config.toml has no directory entry and
			// the record directory is STILL present after the read (a dangling
			// config symlink or vanished registry makes the load ENOENT too),
			// and only on the second spaced strike.
			if !projectConfigEntryAbsent(projectID) || !projectRecordDirPresent(projectID) {
				personalUnreadable[repoID] = projectID
				m.resetPersonalAbsenceStreak(projectID)
				continue
			}
			m.mu.Lock()
			// Per-project spacing, independent of the shared retry clock: an
			// observation landing within one backoff base of the previous
			// strike is IGNORED (not counted, not a reset) — two ticks one
			// second apart must never satisfy a discipline that exists to
			// survive mount flaps (#3299 review round 9).
			now := nowFunc()
			if last, ok := m.rootHealAbsenceLastStrike[projectID]; ok && now.Sub(last) < rootEnsureBackoffBase {
				m.mu.Unlock()
				personalUnreadable[repoID] = projectID
				continue
			}
			m.rootHealAbsenceLastStrike[projectID] = now
			m.rootHealAbsenceStreaks[projectID]++
			strikes := m.rootHealAbsenceStreaks[projectID]
			m.mu.Unlock()
			if strikes < 2 {
				personalUnreadable[repoID] = projectID
				continue
			}
			healedCount++
			m.resetPersonalAbsenceStreak(projectID)
			m.info().Printf("root agent snapshot: project %s personal config was removed; root-agent resolution for repo %s resumes without a personal layer", projectID, repoID)
			continue
		}
		healedCount++
		m.resetPersonalAbsenceStreak(projectID)
		if layer := pc.RootAgentLayer(); layer != nil {
			personal[repoID] = layer
		}
		m.info().Printf("root agent snapshot: project %s personal config loads again; root-agent resolution for repo %s resumes from config", projectID, repoID)
	}
	return personal, personalUnreadable, healedCount
}

// resetPersonalAbsenceStreak clears a project's ENOENT two-strike counter.
func (m *Manager) resetPersonalAbsenceStreak(projectID string) {
	m.mu.Lock()
	delete(m.rootHealAbsenceLastStrike, projectID)
	delete(m.rootHealAbsenceStreaks, projectID)
	m.mu.Unlock()
}

// projectConfigEntryAbsent distinguishes a removed config.toml from an
// existing entry whose target cannot be read. Lstat deliberately does not
// follow symlinks, so a dangling symlink remains an unreadable config state.
func projectConfigEntryAbsent(projectID string) bool {
	path, err := config.ProjectConfigTomlPath(projectID)
	if err != nil {
		return false
	}
	_, err = os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

// projectRecordDirPresent reports whether a project's registry record
// directory currently exists. The personal-config retry gates its
// ENOENT-means-removed reading on it: only a present record directory proves
// a missing config.toml was deliberately removed rather than momentarily
// absent with its whole registry.
func projectRecordDirPresent(projectID string) bool {
	path, err := config.ProjectConfigTomlPath(projectID)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Dir(path))
	return err == nil && info.IsDir()
}
