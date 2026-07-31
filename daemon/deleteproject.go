package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// repoSessionTitlesLocked returns the titles of the repo's sessions the daemon still
// tracks, across BOTH maps a session can live in — m.pendingCreates (a create that has
// passed validation but not finished provisioning, so it is NOT yet in m.instances,
// #2549) and m.instances (its live, non-archived rows). This exists because
// m.instances is not the universe (the same class the #1892 ghostTaskRuns comment in
// manager.go warns about, reached through a different door): a delete that walked only
// m.instances would miss a still-provisioning create, deregister the project, and let
// the create finish into a live orphan.
//
// inFlightOnly selects the ones a delete cannot archive YET — every pending create,
// plus any m.instances row with an op in flight — which is what the up-front fail-closed
// gate refuses on. With inFlightOnly false it returns every live-or-pending session,
// which is the concurrent-create re-check before the deregister. The caller holds m.mu.
func (m *Manager) repoSessionTitlesLocked(repoID string, inFlightOnly bool) []string {
	var titles []string
	// A pending create is always in flight (still provisioning), so it counts either way.
	for key := range m.pendingCreates {
		if rid, title := splitDaemonInstanceKey(key); rid == repoID {
			titles = append(titles, title)
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
		titles = append(titles, title)
	}
	return titles
}

// deregisterRootAgents is the durable root_agents removal DeleteProject runs. A
// package var so tests can force a persist failure in isolation (exercising the
// #1740-review fatal-on-config-failure path) without disturbing the real config.
var deregisterRootAgents = config.DeregisterRootAgentsForRepo

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
	// Deregistered is true when this delete removed the repo's durable #2355 registry
	// record (#2456). It is what lets the projects-changed signal fire for a
	// registered project with NO live sessions — otherwise a delete that archived
	// nothing would publish nothing and the registered project would linger.
	Deregistered bool
}

// normalizeDeleteProjectPath resolves an existing path to its canonical main
// repo root and identity. A stale/missing path falls back to its cleaned spelling
// so deleting an unknown or moved project remains an idempotent no-op.
func normalizeDeleteProjectPath(path string) (string, string) {
	root := config.ExpandTilde(strings.TrimSpace(path))
	if repo, err := config.RepoFromPath(root); err == nil {
		return repo.Root, repo.ID
	}
	root = filepath.Clean(root)
	return root, config.RepoIDFromRoot(root)
}

// registeredProjectRootForRepoID resolves the path needed by
// config.DeregisterProject when a direct client supplies only the daemon's
// repo identity. Registry read failure is an unknown outcome, not evidence that
// no matching registration exists, so callers must treat an error as fatal
// before mutating sessions or root-agent state.
func registeredProjectRootForRepoID(repoID string) (string, error) {
	projects, err := config.ListProjects()
	if err != nil {
		return "", fmt.Errorf("read durable project registry: %w", err)
	}
	var root string
	for _, project := range projects {
		if config.RepoIDFromRoot(project.Root) != repoID {
			continue
		}
		if root != "" && filepath.Clean(root) != filepath.Clean(project.Root) {
			return "", fmt.Errorf("repo id %s matches multiple registered roots %q and %q; cannot determine which project to deregister", repoID, root, project.Root)
		}
		root = project.Root
	}
	return root, nil
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
	repoID := strings.TrimSpace(req.RepoID)
	repoPath := strings.TrimSpace(req.RepoPath)
	if repoID == "" {
		if repoPath == "" {
			return DeleteProjectResult{}, fmt.Errorf("delete project: repo_id or repo_path is required")
		}
		// Expand a leading ~ BEFORE canonicalizing: git (RepoFromPath) does not do
		// tilde expansion — the shell normally would — so a literal "~/repo" request
		// (a root_agents key spelling, or a hand-typed CLI arg) must be expanded here
		// or it resolves to nothing and silently misses (#1740 review).
		// Canonicalize the path the SAME way the daemon keys repos everywhere:
		// resolve it to the main-repo root (git toplevel) and hash THAT, so a
		// symlinked / trailing-slash / relative / subdirectory / ".."-laden form of
		// a REAL project still resolves to its sessions' repo id — never a silent
		// miss. Only when the path does not resolve to a git repo (a moved/removed/
		// typo'd project) fall back to hashing the cleaned path: that yields no
		// match, which is the clean idempotent no-op deleting an unknown project
		// must be (Sachin's locked semantics), not a wrong-project hit.
		repoPath, repoID = normalizeDeleteProjectPath(repoPath)
	} else if repoPath == "" {
		var err error
		repoPath, err = registeredProjectRootForRepoID(repoID)
		if err != nil {
			return DeleteProjectResult{RepoID: repoID}, fmt.Errorf("delete project %s: could not determine its registered root from repo_id; nothing was changed: %w", repoID, err)
		}
	} else {
		// Both selectors must describe one project. Otherwise RepoID chooses the
		// sessions/root-agent state while RepoPath chooses a potentially different
		// registry row — a split-target partial delete hidden behind success.
		var pathRepoID string
		repoPath, pathRepoID = normalizeDeleteProjectPath(repoPath)
		if pathRepoID != repoID {
			return DeleteProjectResult{RepoID: repoID}, fmt.Errorf("delete project: repo_id %s does not match repo_path %q (repo id %s); nothing was changed", repoID, repoPath, pathRepoID)
		}
	}
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
	m.mu.Lock()
	starting := m.repoSessionTitlesLocked(repoID, true)
	m.mu.Unlock()
	if len(starting) > 0 {
		sort.Strings(starting)
		return result, fmt.Errorf("delete project %s: session(s) %v are still starting; nothing was changed — delete again once they are ready", repoID, starting)
	}

	// Durably drop the repo's root_agents opt-in FIRST, before mutating ANY state,
	// and treat a failed write as FATAL to the whole delete (#1740 review): if this
	// removal fails, a daemon restart would re-register the root and the project
	// would REAPPEAR — so reporting success would be a lie. Persisting before any
	// in-memory change also keeps the failure ATOMIC: on error nothing is archived
	// AND no in-memory suppression is left behind, so a failed delete leaves the
	// project fully intact. A project with no opt-in is a nil, nil no-op.
	if removed, cfgErr := deregisterRootAgents(repoID); cfgErr != nil {
		return result, fmt.Errorf("delete project %s: could not durably remove its root_agents opt-in — the project would reappear on daemon restart, so nothing was changed; retry: %w", repoID, cfgErr)
	} else if len(removed) > 0 {
		log.InfoLog.Printf("delete project %s: removed %d root_agents opt-in(s): %v", repoID, len(removed), removed)
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
	m.suppressRootAgent(repoID)

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
		_, archived, err := m.ArchiveSession(ArchiveSessionRequest{ID: t.id, Title: t.title, RepoID: repoID})
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
		return result, fmt.Errorf("delete project %s: archived %d, tore down %d, but %d session(s) could not be removed (retry to finish): %w",
			repoID, len(result.Archived), len(result.Killed), len(errs), errors.Join(errs...))
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
		return result, fmt.Errorf("delete project %s: archived %d, tore down %d, but a session started for this project meanwhile (%v); the project was NOT removed — delete again to finish",
			repoID, len(result.Archived), len(result.Killed), appeared)
	}

	// Every session for the repo is archived and none is starting — NOW drop the durable
	// #2355 registry record (after the archive, #2549): the deregister must never land
	// while a session survives, or the delete leaves a live orphan in a repo no longer in
	// the registry. repoPath is either caller-provided and canonicalized, or resolved from
	// the daemon's durable registry for the documented RepoID-only form. An empty root
	// means no matching registration existed at resolution time, so there is nothing to
	// deregister. Recorded so the projects-changed publish fires even when no session was
	// archived (a registered, sessionless project).
	if repoPath != "" {
		deregistered, regErr := config.DeregisterProject(repoPath)
		if regErr != nil {
			// SYMMETRIC to an archive failure, and NOT "nothing changed": the sessions are
			// already archived here, so a retry re-archives nothing and finishes the deregister
			// — at worst an empty-but-registered project, never a live orphan.
			return result, fmt.Errorf("delete project %s: archived %d session(s) but could not remove its durable registry record — the project still lists; retry to finish, a retry re-archives nothing: %w", repoID, len(result.Archived), regErr)
		}
		if deregistered {
			result.Deregistered = true
			log.InfoLog.Printf("delete project %s: removed its durable registry record", repoID)
		}
	}

	log.InfoLog.Printf("deleted project %s: archived %d session(s), tore down %d in-place session(s)", repoID, len(result.Archived), len(result.Killed))
	return result, nil
}

// suppressRootAgent marks repoID's project as deleted for the rest of this
// daemon's life so the ensure loop stops (re-)creating its always-on root agent,
// and clears the kill-grace record so no stale grace window survives (#1735). The
// ensure loop is keyed by config path, not repoID, so the deletedRootRepos check
// (which resolves each path to its repoID) is where suppression takes effect.
func (m *Manager) suppressRootAgent(repoID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedRootRepos[repoID] = struct{}{}
	delete(m.rootKilledAt, repoID)
}
