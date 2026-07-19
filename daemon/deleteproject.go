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

// deregisterRootAgents is the durable root_agents removal DeleteProject runs. A
// package var so tests can force a persist failure in isolation (exercising the
// #1740-review fatal-on-config-failure path) without disturbing the real config.
var deregisterRootAgents = config.DeregisterRootAgentsForRepo

// DeleteProjectResult reports what DeleteProject did so the control server can
// publish one archived/killed event per affected session (plus a
// projects-changed signal), and the CLI/TUI can report the counts.
type DeleteProjectResult struct {
	RepoID string
	// Archived carries the {ID, Title} of every session archived (restorable via
	// RestoreArchived). Killed carries the {ID, Title} of every in-place/external
	// session torn down instead (archive can't relocate an external worktree; its
	// kill never touches the user's tree/branch).
	Archived []session.InstanceData
	Killed   []session.InstanceData
}

// DeleteProject deletes a project — a repo grouping of sessions (#1735) — with
// ARCHIVE-THEN-REMOVE, reversible semantics, under the single-writer daemon:
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
//
// The user's real git repository is never touched. Because the active-projects
// list is derived from LIVE sessions, archiving them all removes the project from
// it; restoring any archived session brings the project back — the reversible
// contract. Idempotent: deleting an unknown or already-empty project archives
// nothing, drops no opt-in, and returns a zero-count success.
func (m *Manager) DeleteProject(req DeleteProjectRequest) (DeleteProjectResult, error) {
	repoID := strings.TrimSpace(req.RepoID)
	if repoID == "" {
		root := strings.TrimSpace(req.RepoPath)
		if root == "" {
			return DeleteProjectResult{}, fmt.Errorf("delete project: repo_id or repo_path is required")
		}
		// Expand a leading ~ BEFORE canonicalizing: git (RepoFromPath) does not do
		// tilde expansion — the shell normally would — so a literal "~/repo" request
		// (a root_agents key spelling, or a hand-typed CLI arg) must be expanded here
		// or it resolves to nothing and silently misses (#1740 review).
		root = config.ExpandTilde(root)
		// Canonicalize the path the SAME way the daemon keys repos everywhere:
		// resolve it to the main-repo root (git toplevel) and hash THAT, so a
		// symlinked / trailing-slash / relative / subdirectory / ".."-laden form of
		// a REAL project still resolves to its sessions' repo id — never a silent
		// miss. Only when the path does not resolve to a git repo (a moved/removed/
		// typo'd project) fall back to hashing the cleaned path: that yields no
		// match, which is the clean idempotent no-op deleting an unknown project
		// must be (Sachin's locked semantics), not a wrong-project hit.
		if repo, rerr := config.RepoFromPath(root); rerr == nil {
			repoID = repo.ID
		} else {
			repoID = config.RepoIDFromRoot(filepath.Clean(root))
		}
	}
	result := DeleteProjectResult{RepoID: repoID}

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

	// The durable removal succeeded, so now apply the in-memory suppression that
	// stops the ensure loop re-creating the always-on root (m.cfg is immutable
	// after start). Doing it before the teardown guarantees no poll tick respawns
	// the root we are about to tear down; doing it only AFTER the persist means a
	// failed write above never leaves a dangling suppression (#1740 review).
	m.suppressRootAgent(repoID)

	// Snapshot the repo's LIVE (non-archived) sessions under the lock, then act
	// with the lock released — ArchiveSession/KillSession take their own
	// per-session locks and would deadlock if called while holding m.mu.
	type target struct {
		title    string
		external bool
	}
	var targets []target
	m.mu.Lock()
	for key, inst := range m.instances {
		rid, title := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		if inst.GetLiveness() == session.LiveArchived {
			continue
		}
		targets = append(targets, target{title: title, external: inst.IsExternalWorktree()})
	}
	m.mu.Unlock()

	// Deterministic order so a partial failure + retry is stable and the logs read
	// consistently.
	sort.Slice(targets, func(i, j int) bool { return targets[i].title < targets[j].title })

	var errs []error
	for _, t := range targets {
		if t.external {
			killed, err := m.KillSession(KillSessionRequest{Title: t.title, RepoID: repoID})
			if err != nil {
				errs = append(errs, fmt.Errorf("session %q: %w", t.title, err))
				continue
			}
			result.Killed = append(result.Killed, session.InstanceData{ID: killed.ID, Title: killed.Title})
			continue
		}
		_, archived, err := m.ArchiveSession(ArchiveSessionRequest{Title: t.title, RepoID: repoID})
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
		result.Archived = append(result.Archived, session.InstanceData{ID: archived.ID, Title: archived.Title})
	}

	if len(errs) > 0 {
		// Partial: some sessions were busy or errored mid-teardown. The delete is
		// idempotent, so re-running it finishes the rest. Report what did happen.
		return result, fmt.Errorf("delete project %s: archived %d, tore down %d, but %d session(s) could not be removed (retry to finish): %w",
			repoID, len(result.Archived), len(result.Killed), len(errs), errors.Join(errs...))
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
