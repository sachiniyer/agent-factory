package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// repoSessionTitlesLocked returns the titles of the repo's sessions the daemon still
// tracks, across every create/restore phase: m.reservedTitles (admitted but not yet
// projected), m.pendingCreates (provisioning, so not yet in m.instances, #2549),
// m.restoresInFlight (the restore-only subset of m.killsInFlight), and m.instances
// (live, non-archived rows). m.instances is not the universe (the same class the #1892
// ghostTaskRuns comment in manager.go warns about, reached through a different door): a
// delete that walked only it would miss a still-provisioning create or a restoring
// archive, deregister the project, and let that session finish into a live orphan.
//
// inFlightOnly selects the ones a delete cannot archive YET — every pending create,
// plus any m.instances row with an op in flight — which is what the up-front fail-closed
// gate refuses on. With inFlightOnly false it returns every live-or-pending session,
// which is the concurrent-create re-check before the deregister. The caller holds m.mu.
func (m *Manager) repoSessionTitlesLocked(repoID string, inFlightOnly bool) []string {
	titleSet := make(map[string]struct{})
	// A successful reservation is already an admitted create, even during the
	// narrow interval before CreateSession publishes pendingCreates. Counting it
	// here makes the delete fence decision atomic with reserveCreate under m.mu.
	for key := range m.reservedTitles {
		if rid, title := splitDaemonInstanceKey(key); rid == repoID {
			titleSet[title] = struct{}{}
		}
	}
	// A pending create is always in flight (still provisioning), so it counts either way.
	for key := range m.pendingCreates {
		if rid, title := splitDaemonInstanceKey(key); rid == repoID {
			titleSet[title] = struct{}{}
		}
	}
	// Restore claims are admitted under m.mu. Count them even while an archived
	// row has not yet transitioned to Lost+OpRestoring; the matching restore
	// admission checks projectDeletes under this same lock. Other lifecycle ops
	// retain DeleteProject's established per-session partial-failure behavior.
	//
	// A restore still WAITING for its session's operation lock is deliberately not
	// here (#3600): it claims only once it holds that lock, so a delete raised
	// during the wait is admitted, and the restore then reads projectDeletes under
	// this same m.mu and refuses itself. Both orders end with exactly one of the
	// two acting; which one is decided by whichever reaches m.mu first, and the
	// loser's refusal names the fence it lost to.
	for key := range m.restoresInFlight {
		if rid, title := splitDaemonInstanceKey(key); rid == repoID {
			titleSet[title] = struct{}{}
		}
	}
	for key, inst := range m.instances {
		rid, title := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		if inFlightOnly && inst.GetInFlightOp() == session.OpNone {
			continue
		}
		titleSet[title] = struct{}{}
	}
	titles := make([]string, 0, len(titleSet))
	for title := range titleSet {
		titles = append(titles, title)
	}
	return titles
}

// deregisterRootAgents is the durable root_agents removal DeleteProject runs. A
// package var so tests can force a persist failure in isolation (exercising the
// #1740-review fatal-on-config-failure path) without disturbing the real config.
var deregisterRootAgents = config.DeregisterRootAgentsForRepo

// deleteProjectPreSweepHookForTest, when non-nil, runs immediately before the
// durable root_agents sweep decides which identities to remove. That window —
// after the selectors and the claimant were resolved, before the locked config
// mutation — is where a checkout appearing at an absent recorded root makes the
// path's hash somebody else's (#3530 review id 3917445672), and nothing else
// can hold it open: in a real daemon the two are microseconds apart.
var deleteProjectPreSweepHookForTest func()

// deleteProjectPostClaimantHookForTest, when non-nil, runs after the claimant
// scan and before the identity-transition check that follows it. That ordering
// is #3530 review id 3917929613's subject — a checkout REAPPEARING while the
// claimant scan reads it — and the two are microseconds apart in a real
// daemon.
var deleteProjectPostClaimantHookForTest func()

// DeleteProjectResult reports what DeleteProject did so the control server can
// publish one archived/killed event per affected session (plus a
// projects-changed signal), and the CLI/TUI can report the counts.
type DeleteProjectResult struct {
	RepoID string
	// Archived carries the full committed projection of every archived session so
	// lifecycle events are directly applicable by clients (#2680). Killed needs only
	// {ID, Title} for every in-place/external session torn down instead (archive can't
	// relocate an external worktree; its kill never touches the user's tree/branch).
	Archived []session.InstanceData
	Killed   []session.InstanceData
	// Warnings carries failed on-archive hooks for sessions whose archive still
	// committed. They do not block the remaining sessions or project
	// deregistration, but every caller must surface them as a committed outcome.
	Warnings []string
	// Deregistered is true when this delete removed the repo's durable #2355 registry
	// record (#2456). It is what lets the projects-changed signal fire for a
	// registered project with NO live sessions — otherwise a delete that archived
	// nothing would publish nothing and the registered project would linger.
	Deregistered bool
}

func deleteProjectFailure(result DeleteProjectResult, err error) error {
	if len(result.Warnings) == 0 {
		return err
	}
	return fmt.Errorf("%w; warning(s) from archive(s) that already committed: %s",
		err, strings.Join(result.Warnings, "\n"))
}

// DeleteProject deletes a project — a repo grouping of sessions (#1735) — with
// archive-then-remove semantics under the single-writer daemon:
//
//   - Every LIVE session of the repo is ARCHIVED (tmux torn down, worktree moved
//     to the archive dir, branch + state preserved) so it stays restorable via
//     RestoreArchived. Already-archived rows are left untouched — they are the
//     restorable state this delete preserves.
//   - An in-place/external worktree session (the always-on root agent, or an
//     `af sessions create --here` session) cannot be archived — archive relocates
//     the worktree, unsupported for the user's own checkout — so it is torn down
//     instead. That teardown never touches the user's tree or branch (#1107).
//   - The repo's root_agents opt-in is dropped (in-memory suppression for this
//     daemon's life + removed from config on disk) so the project does not linger
//     empty in the picker and no always-on root respawns.
//   - A durable project registration whose root matches RepoPath — or whose root
//     the daemon resolves from a RepoID-only request — is removed after every
//     session settles, so a registered-but-sessionless project also disappears.
//
// The user's real git repository is never touched. Because the active-projects
// list is derived from LIVE sessions, archiving them all removes the project from
// it; restoring any archived session makes the repo active again, but does not
// restore its durable registration or root_agents opt-in. Idempotent: deleting an
// unknown project archives nothing, drops no opt-in or registration, and returns
// a zero-count success; a registered project with no sessions is deregistered.
func (m *Manager) DeleteProject(req DeleteProjectRequest) (DeleteProjectResult, error) {
	target, err := m.resolveDeleteProjectTarget(req)
	if err != nil {
		return DeleteProjectResult{RepoID: target.repoID}, err
	}
	m.taskTargetMu.Lock()
	defer m.taskTargetMu.Unlock()
	return m.deleteProject(target)
}

type deleteProjectTarget struct {
	repoID   string
	repoPath string
	// claimantProjectID names the registry record whose root is repoPath, so a
	// suppression tombstone records WHOSE delete it was and a later proven
	// mismatch releases it only for that claimant (#3299 review round 15). It
	// is resolved here rather than at suppression time because finding it
	// stats every recorded root, and #3361's whole point is that a stalled
	// mount must not be walked while taskTargetMu is held. Best-effort: a
	// registry read failure leaves it "" — an unreleasable tombstone, the
	// conservative direction.
	claimantProjectID string
	// unattributedRecordRoot names a registry record this delete's identity
	// may have come from and cannot be shown not to have: one written before
	// af recorded repository identities, whose absent recorded root hashes to
	// exactly this id (#3363's third window). See unattributableLegacyRecord —
	// it is resolved there, ahead of taskTargetMu, for the same reason the
	// claimant is. Empty whenever a record WAS selected, which settles the
	// question, or when none could be this project's.
	unattributedRecordRoot    string
	unattributedRecordProject string
}

// deleteProject performs DeleteProject while taskTargetMu is already held from
// blocker preflight through the final session mutation.
func (m *Manager) deleteProject(resolved deleteProjectTarget) (DeleteProjectResult, error) {
	repoID := resolved.repoID
	repoPath := resolved.repoPath
	result := DeleteProjectResult{RepoID: repoID}

	// FAIL CLOSED, before mutating ANY state, if a session for this repo is still being
	// created (#2549). A create lives in m.pendingCreates through its slow provisioning
	// and only joins m.instances at the end (manager_create.go), and ArchiveSession
	// refuses a session with an in-flight op — so such a session can neither be seen by
	// an m.instances snapshot nor archived. Rather than block the caller on an arbitrary
	// clock (a git clone / image pull settles in minutes, not the seconds a bounded wait
	// could afford), refuse immediately and name the session: the create settles on its
	// own and a retry then removes the project cleanly. Because this runs before the
	// durable removals below, "nothing was changed" is literally true.
	// A provisional target reaches nothing by construction, which is the point
	// — but the row it would still deregister may have LIVE sessions filed
	// under the historical hash of its recorded path, created while that path
	// resolved (#3530 review id 3918535480). Deregistering while archiving
	// nothing leaves them orphaned behind a success. Which project that hash
	// names cannot be established from here — a stranger may have reused the
	// path, which is the collision this change removes — so the honest answer
	// is to refuse and name the remedy. The overwhelmingly common upgrade case,
	// a legacy row with no sessions left, is untouched.
	// The historical identity a provisional target's recorded path used to
	// have. Both checks below key on it, and both happen under ONE acquisition
	// of m.mu together with the fence installation (#3530 review id
	// 3919194996): checking in one critical section and fencing in another let
	// a checkout reappear in between, a create reserve under the historical id,
	// and that create commit after the registry row was removed — because the
	// fence went on the provisional id alone.
	fenced := []string{repoID}
	historical := ""
	if config.IsDerivedRepoID(repoID) && repoPath != "" {
		if id := config.RepoIDFromRoot(filepath.Clean(repoPath)); id != repoID {
			historical = id
			fenced = append(fenced, id)
		}
	}

	m.mu.Lock()
	// A provisional target reaches nothing by construction, which is the point
	// — but the row it would still deregister may have LIVE sessions filed
	// under the historical hash of its recorded path, created while that path
	// resolved (#3530 review id 3918535480). Deregistering while archiving
	// nothing leaves them orphaned behind a success. Which project that hash
	// names cannot be established from here — a stranger may have reused the
	// path, which is the collision this change removes — so the honest answer
	// is to refuse and name the remedy. The overwhelmingly common upgrade case,
	// a legacy row with no sessions left, is untouched.
	stranded := []string(nil)
	if historical != "" {
		stranded = m.repoSessionTitlesLocked(historical, false)
	}
	// The same question asked from the identity side (#3363's third window).
	// There, a provisional target could not reach live sessions filed under its
	// recorded path's hash; here the delete arrives AS that hash and cannot
	// reach the record — so removing these sessions would leave the durable
	// registration behind and the project would return on the next start.
	//
	// Only sessions make it worth refusing over. With nothing live under the
	// identity there is nothing the surviving record can strand, and the delete
	// stays the no-op #3638 made it — which is also the state a mid-delete
	// reconciliation resolves by writing the identity onto the row.
	//
	// Read under the SAME acquisition as the fence below, like stranded and for
	// the same reason (#3530 review id 3919194996): deciding in one critical
	// section and fencing in another lets a session arrive in between.
	unattributed := []string(nil)
	if resolved.unattributedRecordRoot != "" {
		unattributed = m.repoSessionTitlesLocked(repoID, false)
	}
	starting := m.repoSessionTitlesLocked(repoID, true)
	if len(stranded) == 0 && len(unattributed) == 0 && len(starting) == 0 {
		if m.projectDeletes == nil {
			m.projectDeletes = make(map[string]struct{})
		}
		for _, id := range fenced {
			m.projectDeletes[id] = struct{}{}
			m.stampProjectDeleteLocked(id)
		}
	}
	m.mu.Unlock()
	if len(stranded) > 0 {
		sort.Strings(stranded)
		return result, fmt.Errorf("delete project %s: this project was registered before af recorded repository identities, and session(s) %v are still live under the identity its recorded path used to have; deleting now would deregister the project and leave them behind — nothing was changed. Bring the checkout at %s back once so af can record the project's identity, then delete; or archive those sessions first", repoID, stranded, repoPath)
	}
	if len(unattributed) > 0 {
		sort.Strings(unattributed)
		return result, fmt.Errorf("delete project %s: session(s) %v are live under this identity and no registry record answers to it — project %s was registered before af recorded repository identities and its recorded path %s is unavailable, so af cannot establish whether that record is this project's; removing these sessions would leave its durable registration behind and the project would return on the next start, so nothing was changed. Bring the checkout at %s back once so af can record the project's identity, then delete; or archive those session(s) first and delete the project by that path", repoID, unattributed, resolved.unattributedRecordProject, resolved.unattributedRecordRoot, resolved.unattributedRecordRoot)
	}
	if len(starting) > 0 {
		sort.Strings(starting)
		return result, fmt.Errorf("delete project %s: session(s) %v are still starting or changing; nothing was changed — delete again once their operations finish", repoID, starting)
	}
	defer func() {
		m.mu.Lock()
		for _, id := range fenced {
			delete(m.projectDeletes, id)
		}
		// Stamp the REMOVE too, not only the install above. A create that began
		// before this delete did samples a counter value below the install's stamp,
		// so the install alone already covers it — but a create that began while
		// this delete was ALREADY running samples above the install and would see
		// nothing move. Stamping here puts the transition after that sample, which
		// is what makes an in-progress delete visible to a create that could not
		// have seen its start (#2947).
		for _, id := range fenced {
			m.stampProjectDeleteLocked(id)
		}
		m.mu.Unlock()
	}()

	// A task-target refusal is predictable and must precede BOTH stores this
	// operation mutates. Discovering it inside the later archive loop used to
	// remove root_agents and suppress respawn before returning the error. The
	// caller then had neither a deleted project nor its previous lifecycle
	// configuration. taskTargetMu keeps this preflight stable through the loop.
	taskTargets, err := m.preflightDeleteProjectTaskTargets(repoID)
	if err != nil {
		return result, err
	}

	// Durably drop the repo's root_agents opt-in FIRST, before mutating ANY state,
	// and treat a failed write as FATAL to the whole delete (#1740 review): if this
	// removal fails, a daemon restart would re-register the root and the project
	// would REAPPEAR — so reporting success would be a lie. Persisting before any
	// in-memory change also keeps the failure ATOMIC: on error nothing is archived
	// AND no in-memory suppression is left behind, so a failed delete leaves the
	// project fully intact. A project with no opt-in is a nil, nil no-op.
	// BOTH identities in ONE durable mutation (#3299 review rounds 6-7): a
	// re-attributed project's legacy key spelled as the unavailable recorded
	// path matches only its derived hash, and two separate writes could
	// remove one opt-in and then fail — falsifying the nothing-was-changed
	// guarantee below.
	// root_agents keys are PATHS, and the matcher resolves them to a canonical
	// identity — so a delete targeting a provisional id would sweep nothing and
	// leave the durable opt-in to start the root again on the next daemon run
	// (#3530 review id 3914971851).
	//
	// The PATH decides this, not the identity (#3530 review ids 3916379565,
	// 3916757161). A root_agents key is a path, and rootAgentKeyMatchesRepo
	// falls back to hashing one it cannot resolve — so a stale key spelled as
	// this project's recorded root answers to that path's hash and to nothing
	// else, whenever the recorded root is not its repository's identity root.
	// Master swept it by accident, because it addressed such a project BY that
	// hash; addressing the project as itself is what made the sweep have to say
	// so out loud.
	//
	// And the hash is supplied ONLY while the recorded root does not resolve
	// (review id 3917445672). That is not an optimisation: a repository
	// appearing there legitimately OWNS that id, so supplying it once a
	// checkout is present would sweep the occupant's own opt-in on behalf of a
	// delete that targeted this project. The check runs here, immediately
	// before the locked write, rather than at selector time — as late as it can
	// be, though check-then-act cannot be made atomic and a checkout appearing
	// inside that window is the residue this states rather than hides.
	if deleteProjectPreSweepHookForTest != nil {
		deleteProjectPreSweepHookForTest()
	}
	optInIDs := []string{repoID}
	if repoPath != "" {
		if pathID := config.RepoIDFromRoot(filepath.Clean(repoPath)); pathID != repoID {
			// An ANSWER is required, not merely a failure (#3530 review id
			// 3918379027, #3500's rule). A killed or unstartable git says
			// nothing about what occupies the path, and a repository that is
			// there OWNS this hash — so acting on an unanswered probe would
			// delete a live occupant's own opt-in on behalf of this project.
			// A DETERMINATE verdict, not merely an answered failure (#3530
			// review id 3919490145): dubious ownership or unreadable metadata
			// exits normally, and a replacement checkout that owns this hash
			// would have its own opt-in swept on behalf of the stale project.
			if _, err := config.RepoFromPath(repoPath); err != nil && config.PathIsDeterminatelyFree(repoPath, err) {
				optInIDs = append(optInIDs, pathID)
			}
		}
	}
	if removed, cfgErr := deregisterRootAgents(optInIDs...); cfgErr != nil {
		return result, fmt.Errorf("delete project %s: could not durably remove its root_agents opt-in — the project would reappear on daemon restart, so nothing was changed; retry: %w", repoID, cfgErr)
	} else if len(removed) > 0 {
		m.info().Printf("delete project %s: removed %d root_agents opt-in(s): %v", repoID, len(removed), removed)
	}

	// The repo's durable #2355 registry record is dropped LATER, only after every
	// session it promised to archive is actually archived (#2549): deregistering here,
	// before the archive, is what let a session still finishing its create survive as a
	// live orphan in a repo no longer in the registry. See the archive loop below.

	// The durable removal succeeded, so now apply the in-memory suppression that
	// stops the ensure loop re-creating the always-on root (m.cfg is immutable
	// after start). Doing it before the teardown guarantees no poll tick respawns
	// the root we are about to tear down; doing it only AFTER the persist means a
	// failed write above never leaves a dangling suppression (#1740 review).
	// The tombstone records WHOSE delete this was, so a later proven
	// mismatch can release it only for the deleted claimant itself (#3299
	// review round 15). Resolved before the lock — see deleteProjectTarget.
	m.suppressRootAgent(repoID, resolved.claimantProjectID)
	// A re-attribution probe for the deleted project is deliberately LEFT
	// RUNNING (#3299 review rounds 9-11, converged): while its marker read
	// is unfinished, the attribution-pending gate fails the candidate repo
	// closed — which doubles as this delete's suppression — and its eventual
	// completion publishes the reattributedFrom alias that carries the
	// tombstones above to the real identity. Retiring or exempting the probe
	// (both tried) opened a window where an in-memory legacy entry could
	// resurrect the deleted root before the alias existed.

	// Archive/kill every live session for the repo BEFORE removing its durable identity,
	// so no session outlives the project's registry entry (#2549). The phase-1 gate above
	// already refused if any session was mid-create, so every live session here is settled
	// and archivable. Acting with m.mu released — ArchiveSession/KillSession take their own
	// per-session locks and would deadlock under it.
	type target struct {
		id       string
		title    string
		external bool
	}
	var targets []target
	m.mu.Lock()
	for key, inst := range m.instances {
		rid, title := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		targets = append(targets, target{id: inst.ID, title: title, external: inst.IsExternalWorktree()})
	}
	m.mu.Unlock()

	// Deterministic order so a partial failure + retry is stable and the logs read
	// consistently.
	sort.Slice(targets, func(i, j int) bool { return targets[i].title < targets[j].title })

	var errs []error
	for _, t := range targets {
		if t.external {
			// Carry the stable identity captured under m.mu into the destructive
			// lookup. Besides making a concurrent completed kill distinguishable,
			// this prevents a same-title replacement from being torn down in the
			// original target's place. Legacy id-less rows retain title lookup.
			killed, err := m.KillSession(KillSessionRequest{ID: t.id, Title: t.title, RepoID: repoID})
			if errors.Is(err, errSessionNotFound) {
				// This is idempotent success only because t came from DeleteProject's
				// own under-lock snapshot: the target existed then and is gone now,
				// which is precisely the requested end state (#2124). A standalone
				// KillSession for a stale/never-existing target still returns the
				// same not-found error. Report the snapshotted identity so counts and
				// lifecycle events do not understate what the delete achieved.
				killed = session.InstanceData{ID: t.id, Title: t.title}
				err = nil
			}
			if isMutationCommitted(err) {
				// A committed kill is durable but NOT finished: the tombstoned row
				// was retained, so at the project level this session genuinely was
				// not removed and the delete must stay a retryable failure. Flatten
				// the marker before joining — errors.As walks the join, so a
				// committed member would make the whole project-delete failure read
				// as mutation_committed on the HTTP envelope, and a delete that
				// never deregistered would print as ok-with-warning (#3234).
				err = errors.New(err.Error())
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("session %q: %w", t.title, err))
				continue
			}
			result.Killed = append(result.Killed, session.InstanceData{ID: killed.ID, Title: killed.Title})
			continue
		}
		// Carry the same snapshotted stable identity into archive that the
		// external-session kill path above carries into KillSession. A title-only
		// lookup here can resolve a NEW same-title session created after the
		// snapshot and archive work the user never confirmed deleting.
		_, archived, err := m.archiveSession(ArchiveSessionRequest{ID: t.id, Title: t.title, RepoID: repoID}, taskTargets)
		if errors.Is(err, errSessionNotFound) {
			// Snapshot-gated idempotency, exactly like the kill path: this target
			// existed under m.mu and is now authoritatively absent by stable ID, so
			// the original is already gone. Do not infer anything about a possible
			// replacement; the live-session re-check below observes it separately
			// and keeps the project registered for a fresh delete decision.
			archived = session.InstanceData{ID: t.id, Title: t.title}
			err = nil
		}
		// A target that is ALREADY archived is idempotent SUCCESS, not a failure
		// (#2108). The snapshot above is a point-in-time read taken under m.mu and
		// then acted on with the lock released, so a concurrent ArchiveSession can
		// land in that window and leave a target in exactly the state this delete
		// wants it in. Counting that as a failure returned a partial-failure error,
		// omitted the session from Archived (an undercount), and pushed the TUI down
		// the error path — all for a session that IS archived. ArchiveSession returns
		// the resolved identity alongside the sentinel, so the row is reported with
		// its real {id, title}. Every OTHER error stays a failure: a busy session, an
		// op in flight, or a teardown that broke is genuinely not archived, and the
		// caller must be told to retry.
		if errors.Is(err, ErrAlreadyArchived) {
			err = nil
		}
		if isMutationCommitted(err) {
			result.Warnings = append(result.Warnings, err.Error())
			err = nil
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("session %q: %w", t.title, err))
			continue
		}
		result.Archived = append(result.Archived, archived)
	}

	if len(errs) > 0 {
		// A genuine teardown failure — including a session that raced into an in-flight op
		// after the phase-1 gate and so refused to archive. The delete is idempotent, so a
		// retry finishes the rest; the registry is NOT deregistered below, so no orphan is
		// left behind.
		return result, deleteProjectFailure(result, fmt.Errorf(
			"delete project %s: archived %d, tore down %d, but %d session(s) could not be removed (retry to finish): %w",
			repoID, len(result.Archived), len(result.Killed), len(errs), errors.Join(errs...)))
	}

	// Concurrent-create guard (#2549): re-check under m.mu, immediately before the
	// deregister, that no session appeared for the repo while the lock was released for the
	// archive above. A create that BEGINS after the phase-1 gate — landing in
	// m.pendingCreates, or completing into m.instances — would otherwise orphan the same
	// way through a narrower window. If one appeared, abort WITHOUT deregistering: the
	// sessions archived so far stay archived (idempotent), and a retry finishes once the
	// new session settles. A create landing in the microseconds between this check and the
	// DeregisterProject call below is the one remaining, deliberately documented window;
	// closing it fully would mean holding m.mu across the registry file lock (an ABBA
	// hazard) for a gap this narrow.
	m.mu.Lock()
	appeared := m.repoSessionTitlesLocked(repoID, false)
	m.mu.Unlock()
	if len(appeared) > 0 {
		sort.Strings(appeared)
		return result, deleteProjectFailure(result, fmt.Errorf(
			"delete project %s: archived %d, tore down %d, but a session started for this project meanwhile (%v); the project was NOT removed — delete again to finish",
			repoID, len(result.Archived), len(result.Killed), appeared))
	}

	// Every session for the repo is archived and none is starting — NOW drop the durable
	// #2355 registry record (after the archive, #2549): the deregister must never land
	// while a session survives, or the delete leaves a live orphan in a repo no longer in
	// the registry. repoPath is either caller-provided and canonicalized, or resolved from
	// the daemon's durable registry for the documented RepoID-only form. An empty root
	// means no matching registration existed at resolution time, so there is nothing to
	// deregister. Recorded so the projects-changed publish fires even when no session was
	// archived (a registered, sessionless project).
	if repoPath == "" && !config.IsDerivedRepoID(repoID) {
		// A reconciliation can write this identity onto a row AFTER the
		// selectors resolved and found nothing — that write is permanent, and
		// leaving the row would bring the project back on the next start
		// (#3530 review id 3919900658). The lookup lives in the registry, under
		// the lock the reconciliation itself takes, because a check out here
		// could only race it (#3530 review id 3919996245); its other refusals
		// — a read failure is not "no row", two rows sharing an identity are
		// #3611's ambiguity, and the row's root must be determinately absent —
		// are stated there.
		//
		// The absence observation is passed in BOUNDED (#3530 review id
		// 3919996261): this runs under taskTargetMu, and a recorded root on an
		// unresponsive mount would otherwise wedge every task-target update and
		// archive behind a stat. A bound that expires reports "not absent",
		// which declines the removal — the conservative direction, and the row
		// is taken by the next delete, which now finds it by the ordinary path.
		deregistered, root, regErr := config.DeregisterProjectByRecordedIdentity(repoID, boundedRecordRootAbsent)
		if regErr != nil {
			return result, deleteProjectFailure(result, fmt.Errorf(
				"delete project %s: archived %d session(s) but could not check whether a registry record recorded this identity meanwhile — the project may still list; retry to finish, a retry re-archives nothing: %w",
				repoID, len(result.Archived), regErr))
		}
		if deregistered {
			result.Deregistered = true
			m.info().Printf("delete project %s: a registry record recorded this identity while the delete was in flight; removed it too (recorded root %s)", repoID, root)
		}
	}
	if repoPath != "" {
		deregistered, regErr := config.DeregisterProject(repoPath)
		if regErr != nil {
			// SYMMETRIC to an archive failure, and NOT "nothing changed": the sessions are
			// already archived here, so a retry re-archives nothing and finishes the deregister
			// — at worst an empty-but-registered project, never a live orphan.
			return result, deleteProjectFailure(result, fmt.Errorf(
				"delete project %s: archived %d session(s) but could not remove its durable registry record — the project still lists; retry to finish, a retry re-archives nothing: %w",
				repoID, len(result.Archived), regErr))
		}
		if deregistered {
			result.Deregistered = true
			m.info().Printf("delete project %s: removed its durable registry record", repoID)
		}
	}

	m.info().Printf("deleted project %s: archived %d session(s), tore down %d in-place session(s)", repoID, len(result.Archived), len(result.Killed))
	return result, nil
}

func (m *Manager) preflightDeleteProjectTaskTargets(repoID string) (map[string][]task.Task, error) {
	m.mu.Lock()
	var titles []string
	hasRoot := false
	for key, instance := range m.instances {
		rid, title := splitDaemonInstanceKey(key)
		if rid != repoID || instance == nil || instance.GetLiveness() == session.LiveArchived {
			continue
		}
		titles = append(titles, title)
		hasRoot = hasRoot || title == session.RootSessionTitle
	}
	m.mu.Unlock()
	// An enabled root is part of the project's effective live-session set even
	// while its process is momentarily absent: the ensure loop will recreate it,
	// and task delivery waits for that same promise. Include the reserved target
	// before the empty-roster return, or DeleteProject can suppress re-creation
	// and strand a root-targeted task behind a title ordinary auto-create refuses.
	// For this consumer UNKNOWN behaves like yes (#3264): a fail-closed repo
	// (unloadable personal config, unlistable registry) reports
	// willMaterialize=false, but deleting through that answer would drop the
	// root_agents opt-in and leave the enabled task stranded the moment the
	// config becomes readable again — the exact hazard this preflight refuses.
	if !hasRoot {
		switch m.rootAgentMaterializeVerdictFor(repoID).reason {
		case rootAgentWillMaterialize, rootAgentRegistryUnreadable, rootAgentPersonalUnreadable, rootAgentProjectUnresolved, rootAgentRecordsUnreadable, rootAgentAttributionPending:
			titles = append(titles, session.RootSessionTitle)
		}
	}
	sort.Strings(titles)
	if len(titles) == 0 {
		return make(map[string][]task.Task), nil
	}

	taskTargets, err := m.loadEnabledTaskTargets(repoID)
	if err != nil {
		return nil, fmt.Errorf("delete project %s: could not determine whether enabled tasks target its sessions; nothing was changed: %w", repoID, err)
	}

	var blockers []string
	for _, title := range titles {
		targeted := taskTargets[title]
		if len(targeted) > 0 {
			blockers = append(blockers, fmt.Sprintf("session %q: %s", title, describeTargetTasks(targeted)))
		}
	}
	if len(blockers) > 0 {
		return nil, fmt.Errorf("delete project %s: enabled task(s) target session(s) it must remove: %s; disable or retarget them, then delete the project again; nothing was changed", repoID, strings.Join(blockers, "; "))
	}
	return taskTargets, nil
}

// suppressRootAgent marks repoID's project as deleted for the rest of this
// daemon's life so the ensure loop stops (re-)creating its always-on root agent,
// and clears the kill-grace record so no stale grace window survives (#1735). The
// ensure loop is keyed by config path, not repoID, so the deletedRootRepos check
// (which resolves each path to its repoID) is where suppression takes effect.
func (m *Manager) suppressRootAgent(repoID, claimantProjectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedRootRepos[repoID] = claimantProjectID
	delete(m.rootKilledAt, repoID)
}

// boundedRecordRootAbsentTimeout bounds one recorded-root stat taken while
// DeleteProject holds taskTargetMu. A stat cannot be cancelled, so what is
// bounded is the WAIT: an unresponsive mount leaves a goroutine parked on the
// syscall rather than the lifecycle lock parked on the mount (#3361's rule,
// #3530 review id 3919996261).
var boundedRecordRootAbsentTimeout = 250 * time.Millisecond

// recordRootAbsentProbe is the observation boundedRecordRootAbsent bounds. A
// package var only so a test can supply one that STALLS: an unresponsive mount
// is what this bound exists for, and nothing else reproduces it.
var recordRootAbsentProbe = recordRootAbsent
