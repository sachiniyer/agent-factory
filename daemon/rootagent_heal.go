package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
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
	if !layers.registryUnreadable && len(layers.personalUnreadable) == 0 && len(layers.unresolvedRoots) == 0 {
		return
	}
	// Two independent cadences (#3299 review round 8). The registry and
	// personal-config retries pace on rootHealNextAttempt's failure backoff —
	// and ONLY on it: the two-strike absence discipline (#3315) needs its
	// observations SPACED by that backoff, so nothing else may drag these
	// reads onto the per-tick cadence. Re-attribution runs on every pass
	// unconditionally: its pacing is per entry (a settled negative rests
	// until its own retryAt; an in-flight probe is checked without blocking),
	// so an idle visit costs a map walk, not filesystem work, and a probe
	// landing between ticks is consumed on the next tick.
	m.mu.Lock()
	due := !nowFunc().Before(m.rootHealNextAttempt)
	m.mu.Unlock()

	healed := *layers
	changed := false
	// rpAttempted/rpChanged track only the backoff-paced reads; the clock
	// below must not move when this pass did none of them.
	rpAttempted := false
	rpChanged := false

	if layers.registryUnreadable {
		if !due {
			return
		}
		rpAttempted = true
		// A latched registry PROVABLY existed at daemon start — plain absence
		// never sets the latch — so during recovery an ABSENT directory is a
		// transition, not proof of zero projects: a repair mv in flight, a
		// mount blip. ListProjectsDetailed makes that distinction explicit
		// and binds an empty result to a present registry. On top of that,
		// recovery publishes only on the SECOND consecutive MATCHING present-
		// and-listable snapshot, one backoff cadence apart, and re-verifies
		// presence after the dependent personal-config reads — the
		// applyHomeCheck two-strike discipline (only a definite observation,
		// twice consecutively), because a mount flap inside a single pass can
		// make removed-looking reads out of files that are about to return
		// (#3315 review, rounds 2-3). A flap now has to defeat two spaced
		// passes plus the post-read binding; that residue is indistinguishable
		// without filesystem transactions and is accepted, in writing, here.
		// Per-record failures and strays take the #3297 granularity treatment
		// exactly as at boot.
		if projects, failures, strays, present, err := config.ListProjectsDetailed(); err == nil && present {
			streak := m.observeRootHealRegistrySnapshot(projects)
			if streak >= 2 {
				logRegistryRecordProblems(failures, strays)
				personal, personalUnreadable, projectRoots, unresolvedRoots := projectRootAgentLayers(projects)
				verifiedProjects, _, _, stillPresent, perr := config.ListProjectsDetailed()
				if perr == nil && stillPresent && sameRootHealRegistryProjects(projects, verifiedProjects) {
					healed.personal, healed.personalUnreadable, healed.projectRoots, healed.unresolvedRoots = personal, personalUnreadable, projectRoots, unresolvedRoots
					healed.recordFailureIDs = recordFailureDirectoryIDs(failures)
					healed.registryUnreadable = false
					changed = true
					rpChanged = true
					m.resetRootHealRegistryObservation()
					log.InfoLog.Printf("root agent snapshot: project registry is readable again; resuming root-agent resolution with %d personal layer(s), %d project(s) still failing closed", len(healed.personal), len(healed.personalUnreadable))
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

	if changed {
		// Recompute the legacy dedup set on EVERY heal (#3315 review, both
		// rounds): a legacy path that resolved only after boot must dedup its
		// repo out of the singleton sweep in the published snapshot — whether
		// the heal was the registry or a personal config — or a failing
		// legacy attempt lets the singleton create the root without the
		// legacy layer.
		healed.legacyRepoIDs = legacyRepoIDSet(m.cfg)
		m.rootAgentLayers.Store(&healed)
	}
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
			left[i].RelativeRoot != right[i].RelativeRoot {
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
			log.InfoLog.Printf("root agent snapshot: project %s personal config was removed; root-agent resolution for repo %s resumes without a personal layer", projectID, repoID)
			continue
		}
		healedCount++
		m.resetPersonalAbsenceStreak(projectID)
		if layer := pc.RootAgentLayer(); layer != nil {
			personal[repoID] = layer
		}
		log.InfoLog.Printf("root agent snapshot: project %s personal config loads again; root-agent resolution for repo %s resumes from config", projectID, repoID)
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

// rootReattributionProbe is one asynchronous git/marker check for an
// unresolved recorded root. Its result fields are written only by the probe
// goroutine, strictly before done is closed; the poll goroutine reads them
// only after observing that close, so no lock guards them.
type rootReattributionProbe struct {
	done chan struct{}
	// pass is the heal pass that spawned this probe. A pass gives its own
	// probes a bounded blocking wait; probes inherited from earlier passes
	// already had theirs, so they are only ever checked non-blockingly.
	pass uint64
	// completedAt stamps the probe's finish. A completed result is consumed
	// only while fresh (rootHealProbeResultTTL): freshness by completion
	// time, not by pass identity, is what lets a responsive-but-slow mount
	// heal on the next tick instead of being discarded forever, while still
	// bounding how stale an accepted identity check can be (#3299 review
	// rounds 4-5).
	completedAt time.Time
	// repo is set whenever git resolution succeeded, even for rejected
	// checkouts: the resolved identity keys the verdict bridge.
	repo    *config.RepoContext
	matches bool
	// candidate publishes the resolved identity the moment git resolution
	// succeeds, BEFORE the marker verdict: while this probe is unfinished or
	// unconsumed, that repo's root-agent decision is unknowable — the
	// project's personal layer (possibly enabled=false) still sits under the
	// derived ID — so the resolution choke points fail it closed rather than
	// let a legacy entry start the root mid-verification (#3299 review
	// round 8).
	candidate atomic.Pointer[config.RepoContext]
	// mismatch means the marker READ SUCCEEDED and the marker differs or is
	// absent — a proven different checkout. markerUnreadable means the read
	// itself failed (EACCES, EIO): identity unknowable, no rebind advice.
	mismatch         bool
	markerUnreadable bool
	// settled marks a consumed NEGATIVE result held in place until retryAt:
	// per-entry pacing, so a stalled sibling's hot pass cadence cannot make
	// this entry respawn a git probe every poll tick (#3299 review round 7).
	// Written only by the poll goroutine, after done is closed.
	settled bool
	retryAt time.Time
}

// rootHealProbeGrace bounds how long one heal pass waits for its probes: long
// enough that a healthy local filesystem completes within the same pass (so
// recovery still lands the tick the mount returns), short enough that a
// stalled network mount delays the poll loop by at most this much per tick
// rather than wedging it (#3299 review).
const rootHealProbeGrace = 2 * time.Second

// rootHealProbeResultTTL bounds how stale a completed probe result may be
// when consumed. It must comfortably cover the gap between a probe finishing
// just after its pass's grace and the next poll tick's pass — the
// responsive-but-slow-mount case — while keeping the window in which a
// checkout could be swapped after verification to seconds. The residual
// (a swap landing inside the TTL after a completed probe) is accepted, in
// writing, here: no probe-then-publish design closes it entirely, and the
// alternative — synchronous re-verification at consume time — is exactly the
// poll-goroutine filesystem stall the probes exist to avoid.
const rootHealProbeResultTTL = 30 * time.Second

// reattributeUnresolvedRoots re-attempts git resolution for recorded project
// roots that did not resolve at snapshot time (#3247 arm 2, residue closed by
// #3299). A successful RepoFromPath is content-bearing evidence — a mount
// flap cannot fabricate a resolved toplevel — but AVAILABILITY IS NOT
// IDENTITY (#3299 review): the resolved checkout must also carry the
// project's checkout marker (config.ProjectCheckoutMatches), or a different
// clone reusing the recorded path would inherit the project's personal layer
// and an autonomous root the user never opted that clone into; without the
// marker the project stays unresolved with its mismatch recorded, and the
// log and verdict name the rebind-then-restart remedy. The marker is probed
// at the RECORDED path, never at repo.Root (#3299 review round 2): for a
// linked worktree of a bare clone, RepoFromPath resolves repo.Root to the
// parent of the bare common directory — not a worktree at all — while the
// recorded path binds to the same common dir the marker lives in, exactly
// how Register/Rebind probe it.
//
// Both checks touch the recorded path's filesystem, so they run in per-entry
// probe goroutines and this pass consumes only completed results, waiting at
// most rootHealProbeGrace in total (#3299 review rounds 3-4): a healthy
// mount recovers within its own tick, a stalled one costs a bounded wait and
// stays fail-closed unresolved. Results are pass-scoped — a probe finishing
// after its pass's grace describes a filesystem from a previous cadence and
// is discarded for a fresh re-check rather than trusted late.
//
// Deletion state may be keyed by EITHER identity. An entry whose derived ID
// sits behind an ACTIVE delete fence (projectDeletes) is skipped outright,
// and on acceptance the snapshot records realID→derivedID in
// reattributedFrom: DeleteProject normalizes an unavailable path to the
// derived ID at whatever moment it runs — including after this pass — so
// every deletion-state consumer checks both identities through the published
// alias instead of relying on a carry-at-transition a concurrent delete
// could miss (#3299 review round 4).
//
// On acceptance the project's layers move from the recorded-path derived ID
// to the repo's REAL identity — which also differs when the recorded root is
// a linked worktree of a bare clone or a subdirectory registration, the
// residue the derived key cannot cover — and the recorded root joins
// projectRoots, so the singleton sweep can create the root this run instead
// of waiting for a daemon start. Mutates the candidate snapshot in place; the
// caller publishes.
// reattributeUnresolvedRoots returns the snapshot mutations' companion
// actions in settles: every probe-state transition that RELEASES a fail-closed
// gate (a negative settling, a successful probe retiring) runs only after the
// caller publishes the healed snapshot, so no concurrent reader can observe
// the release before the state that justifies it (#3299 review rounds 9-10).
func (m *Manager) reattributeUnresolvedRoots(healed *rootAgentSnapshot) (changed bool, settles []func()) {
	if len(healed.unresolvedRoots) == 0 {
		return false, nil
	}
	m.rootHealPassSeq++
	pass := m.rootHealPassSeq
	cloneForWrite := func() {
		if changed {
			return
		}
		// Copy-on-first-write: the maps in the candidate snapshot are shared
		// with the published value until replaced.
		healed.personal = cloneLayerMap(healed.personal)
		healed.personalUnreadable = cloneStringMap(healed.personalUnreadable)
		healed.projectRoots = cloneStringMap(healed.projectRoots)
		healed.unresolvedRoots = cloneUnresolvedMap(healed.unresolvedRoots)
		healed.reattributedFrom = cloneStringMap(healed.reattributedFrom)
		healed.unresolvedResolvedIDs = cloneStringMap(healed.unresolvedResolvedIDs)
		changed = true
	}

	// Spawn phase: start (or refresh) every missing probe BEFORE waiting on
	// any result, so one stalled mount cannot starve its siblings of the
	// shared grace — probes run concurrently while the consume phase waits
	// (#3299 review round 5). A completed-but-expired result is replaced by a
	// fresh probe here; a still-running probe is left alone (one outstanding
	// goroutine per entry, however long its mount stalls).
	m.mu.Lock()
	for derivedID, record := range healed.unresolvedRoots {
		probe := m.rootHealProbes[derivedID]
		if probe != nil {
			select {
			case <-probe.done:
				if probe.settled {
					// A consumed negative rests until its own backoff
					// expires; only then does a fresh probe re-check.
					if !nowFunc().Before(probe.retryAt) {
						probe = nil
					}
				} else if nowFunc().Sub(probe.completedAt) > rootHealProbeResultTTL {
					probe = nil
				}
			default:
				// Still running; keep it.
			}
		}
		if probe == nil {
			probe = &rootReattributionProbe{done: make(chan struct{}), pass: pass}
			m.rootHealProbes[derivedID] = probe
			go runRootReattributionProbe(probe, record)
		}
	}
	m.mu.Unlock()

	// Consume phase: only completed, fresh results mutate the snapshot. A
	// probe spawned by THIS pass gets a bounded blocking wait against the
	// shared once-only deadline; a probe inherited from an earlier pass
	// already had its grace and is checked non-blockingly — its result is
	// consumed the tick after it lands (the pending return keeps that tick
	// close instead of on the failure backoff curve).
	deadline := time.After(rootHealProbeGrace)
	expired := false
	for derivedID, record := range healed.unresolvedRoots {
		m.mu.Lock()
		probe := m.rootHealProbes[derivedID]
		settled := probe != nil && probe.settled
		m.mu.Unlock()
		if probe == nil || settled {
			// A settled negative neither pends nor re-consumes: it waits out
			// its own retryAt, invisible to the pass.
			continue
		}
		ready := false
		select {
		case <-probe.done:
			ready = true
		default:
		}
		if !ready && probe.pass == pass && !expired {
			select {
			case <-probe.done:
				ready = true
			case <-deadline:
				// time.After delivers exactly one value; never receive from
				// it twice (#3299 review round 4). Later entries fall through
				// to non-blocking checks only.
				expired = true
			}
		}
		if !ready {
			// Genuinely in flight: the next unconditional pass (every poll
			// tick) consumes the result the tick after it lands.
			continue
		}
		if nowFunc().Sub(probe.completedAt) > rootHealProbeResultTTL {
			// Completed, but stale — the filesystem it verified is from a
			// previous cadence. The spawn phase next pass replaces it.
			continue
		}
		if !probe.matches {
			// Hold the settled negative in place under its own backoff
			// (#3299 review round 7). The settle itself is DEFERRED to after
			// the caller publishes the healed snapshot, and applied under
			// m.mu (#3299 review round 9): settled releases the
			// attribution-pending gate, and a concurrent verdict observing
			// that release before the unreadable-marker bridge is published
			// would resolve fail-open for an instant; the unsynchronized
			// write also races rootAttributionPendingFor's locked read.
			deferredProbe := probe
			deferredID := derivedID
			settles = append(settles, func() {
				m.mu.Lock()
				m.rootHealProbeFailures[deferredID]++
				deferredProbe.retryAt = nowFunc().Add(rootEnsureBackoffFor(m.rootHealProbeFailures[deferredID]))
				deferredProbe.settled = true
				m.mu.Unlock()
			})
		} else {
			// Deferred like the negative settle (#3299 review round 10): the
			// probe's presence is what keeps the candidate repo attribution-
			// pending, and releasing it before the healed snapshot publishes
			// would let a concurrent verdict resolve against the OLD snapshot
			// — personal layers still under the derived ID — for an instant.
			retiredID := derivedID
			settles = append(settles, func() {
				m.mu.Lock()
				delete(m.rootHealProbes, retiredID)
				delete(m.rootHealProbeFailures, retiredID)
				m.mu.Unlock()
			})
		}
		if !probe.matches {
			// The probe logged the specifics; a fresh probe next pass keeps
			// re-checking. Record the failure shape for verdict consumers —
			// a proven mismatch prescribes a rebind, an unreadable marker
			// does not — and bridge the rejected checkout's resolved identity
			// to this record so consumers keyed by the real repo ID see the
			// remedy (#3299 review round 5). No layer moves: the checkout
			// stays unverified.
			resolvedID := ""
			if probe.repo != nil && probe.repo.ID != derivedID {
				resolvedID = probe.repo.ID
			}
			staleBridge := false
			for rid, derived := range healed.unresolvedResolvedIDs {
				if derived == derivedID && rid != resolvedID {
					staleBridge = true
				}
			}
			if record.identityMismatch != probe.mismatch || record.markerUnreadable != probe.markerUnreadable ||
				staleBridge || (resolvedID != "" && healed.unresolvedResolvedIDs[resolvedID] != derivedID) {
				cloneForWrite()
				record.identityMismatch = probe.mismatch
				record.markerUnreadable = probe.markerUnreadable
				healed.unresolvedRoots[derivedID] = record
				// Retire every bridge this record held before recording the
				// current one (if any): a path that went absent again, or that
				// now resolves to a DIFFERENT repository, must not leave the
				// previously observed repo answering for this project forever
				// (#3299 review round 6).
				for rid, derived := range healed.unresolvedResolvedIDs {
					if derived == derivedID && rid != resolvedID {
						delete(healed.unresolvedResolvedIDs, rid)
					}
				}
				if resolvedID != "" {
					healed.unresolvedResolvedIDs[resolvedID] = derivedID
				}
			}
			// A COMPLETED negative outcome is a normal failed read: it feeds
			// the failure backoff rather than the hot pending cadence, or an
			// unavailable root would fork git on every poll tick forever
			// (#3299 review round 6).
			continue
		}
		repo := probe.repo
		m.mu.Lock()
		_, fenced := m.projectDeletes[derivedID]
		m.mu.Unlock()
		if fenced {
			// The next unconditional pass re-checks once the delete settles;
			// the completed match stays consumable while fresh.
			log.InfoLog.Printf("root agent snapshot: recorded project root %s resolves again, but project %s is mid-delete; leaving it unresolved so the delete keeps its derived-ID target", record.root, record.projectID)
			continue
		}
		cloneForWrite()
		if layer, ok := healed.personal[derivedID]; ok && derivedID != repo.ID {
			healed.personal[repo.ID] = layer
			delete(healed.personal, derivedID)
		}
		if projectID, ok := healed.personalUnreadable[derivedID]; ok && derivedID != repo.ID {
			healed.personalUnreadable[repo.ID] = projectID
			delete(healed.personalUnreadable, derivedID)
		}
		if derivedID != repo.ID {
			// The alias is the deletion bridge: DeleteProject keys its fence
			// and tombstone by the derived ID whenever the path is unavailable
			// at delete time — including a delete that starts after this very
			// pass — so every deletion-state consumer checks both identities
			// through the published snapshot rather than relying on a
			// carry-at-transition that a concurrent delete could miss.
			healed.reattributedFrom[repo.ID] = derivedID
		}
		// A verified match retires any rejected-checkout bridge that pointed
		// at this record.
		for resolvedID, derived := range healed.unresolvedResolvedIDs {
			if derived == derivedID {
				delete(healed.unresolvedResolvedIDs, resolvedID)
			}
		}
		// The RECORDED root is the create path (see projectRootAgentLayers):
		// repo.Root for a bare-clone linked worktree is not a repository.
		healed.projectRoots[repo.ID] = record.root
		delete(healed.unresolvedRoots, derivedID)
		log.InfoLog.Printf("root agent snapshot: recorded project root %s resolves again (repo %s, checkout marker verified); its personal layer applies under the repo's real identity and the singleton sweep can ensure it this run", record.root, repo.ID)
	}
	return changed, settles
}

// runRootReattributionProbe performs the blocking half of one re-attribution
// check — git resolution and the checkout-marker probe, both of which touch
// the recorded path's filesystem — so the poll goroutine never blocks on a
// stalled mount. It logs its own negative outcomes because the consuming pass
// only learns pass/fail.
func runRootReattributionProbe(probe *rootReattributionProbe, record unresolvedProjectRecord) {
	defer func() {
		probe.completedAt = nowFunc()
		close(probe.done)
	}()
	repo, err := config.RepoFromPath(record.root)
	if err != nil {
		return
	}
	probe.repo = repo
	probe.candidate.Store(repo)
	matches, err := config.ProjectCheckoutMatches(record.root, record.checkoutID)
	if err != nil {
		// The marker could not be READ — identity is unknowable, which is
		// neither absence nor a proven mismatch. No rebind advice: the
		// original checkout may merely be transiently unreadable, and
		// rebinding over it would be destructive (#3299 review round 5).
		probe.markerUnreadable = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s resolves, but its checkout marker could not be read; leaving project %s unresolved until the marker is readable again (re-checked on the ensure cadence): %v", record.root, record.projectID, err)
		return
	}
	if !matches {
		probe.mismatch = true
		log.WarningLog.Printf("root agent snapshot: recorded project root %s resolves, but the checkout there does not carry project %s's marker %s — a different clone may be reusing the path; leaving it unresolved (run `af projects rebind %s <path>` if this checkout replaces it, then restart the daemon: the running snapshot keeps the marker id it captured at start)", record.root, record.projectID, record.checkoutID, record.projectID)
		return
	}
	probe.matches = true
}

// rootAttributionPendingFor reports that an unconsumed re-attribution probe
// has RESOLVED repoID as some unresolved project's real identity but not yet
// delivered its marker verdict into the published snapshot (#3299 review
// round 8). Until it does, the repo's decision is unknowable: the project's
// personal layer — possibly enabled=false, possibly itself unreadable — still
// sits under the derived ID where resolution cannot see it. Settled negatives
// are NOT pending: a proven mismatch releases the repo (a different project's
// layers do not govern it) and an unreadable marker holds it closed through
// the snapshot bridge instead.
func (m *Manager) rootAttributionPendingFor(repoID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for derivedID, probe := range m.rootHealProbes {
		if probe.settled {
			continue
		}
		if _, deleted := m.deletedRootRepos[derivedID]; deleted {
			// The project behind this probe was deleted: its tombstone (and,
			// once the probe completes and re-attributes, the alias) owns the
			// suppression. Holding the candidate repo attribution-pending on
			// a probe that may be stalled forever would fail a dead project's
			// repo closed indefinitely (#3299 review round 9). The probe
			// itself keeps running — its eventual match publishes the alias
			// that lets deletion suppression reach the real identity.
			continue
		}
		if c := probe.candidate.Load(); c != nil && c.ID == repoID {
			return true
		}
	}
	return false
}

func cloneLayerMap(in map[string]*config.RootAgentLayer) map[string]*config.RootAgentLayer {
	out := make(map[string]*config.RootAgentLayer, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneUnresolvedMap(in map[string]unresolvedProjectRecord) map[string]unresolvedProjectRecord {
	out := make(map[string]unresolvedProjectRecord, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
