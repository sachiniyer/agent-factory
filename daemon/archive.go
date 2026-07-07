package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// ArchiveSession archives a session (#1028): it tears down the session's tmux
// (agent + shell/process tabs) while PRESERVING the record, relocates the
// worktree to the global archive dir (<AF_HOME>/archived/<repoID>/<title>/), and
// marks the instance Archived. The instance stays in the manager map as an inert
// row (unlike Kill, which deletes it); a later RestoreArchived brings it back.
//
// Concurrency mirrors KillSession: it registers in killsInFlight (so a
// concurrent kill or a second archive is rejected, and the Lost-restore /
// finish-kill passes skip it) and holds the per-session op-lock (so archive,
// kill, and Lost-recovery never interleave). Returns the relocated worktree's
// new path.
func (m *Manager) ArchiveSession(req ArchiveSessionRequest) (string, error) {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return "", err
	}
	if session.IsReservedTitle(req.Title) {
		return "", fmt.Errorf("cannot archive the reserved %q session", req.Title)
	}
	if instance == nil {
		// A ghost disk record with no live instance has no in-memory worktree to
		// relocate; there is nothing coherent to archive.
		return "", fmt.Errorf("cannot archive session %q: it is not currently active", req.Title)
	}
	if instance.IsRemote() {
		return "", fmt.Errorf("cannot archive remote session %q: it has no local worktree to relocate", req.Title)
	}
	if instance.IsExternalWorktree() {
		// An in-place/external worktree (`af sessions create --here`, #1107 — also
		// how root is set up) IS the user's own working tree; archive relocates
		// the worktree, which MoveWorktree refuses for external worktrees. Reject
		// it HERE, in the upfront guard, so nothing is torn down for a session
		// that can never be archived — otherwise the rejection would only surface
		// in the move step, after tmux is already down, leaving a broken
		// half-archive that rolls back to Lost.
		return "", fmt.Errorf("cannot archive an in-place/external worktree session %q — archive relocates the worktree, which isn't supported for in-place sessions", req.Title)
	}
	if instance.GetLiveness() == session.LiveArchived {
		return "", fmt.Errorf("session %q is already archived", req.Title)
	}
	if instance.GetInFlightOp() != session.OpNone {
		return "", fmt.Errorf("session %q is busy (%v); try again in a moment", req.Title, instance.GetStatus())
	}

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return "", fmt.Errorf("an operation is already in progress for session %q", req.Title)
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	// Re-verify under the op-lock: findSession released m.mu, so a racing kill
	// may have torn the session down (map entry gone/replaced) or tombstoned it.
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance || instance.UserKilled() {
		return "", fmt.Errorf("session %q changed state before archive could start", req.Title)
	}

	dest, err := archivedWorktreePath(repoID, req.Title)
	if err != nil {
		return "", err
	}

	// Raise the archive fence through the chokepoint (#1195 Phase 2d): BeginArchive
	// sets OpArchiving (I4) so the status poll skips this instance (and the
	// checkpoint save skips persisting a mid-op row) while tmux is down and the
	// worktree is mid-move — never misread as Lost. started is left true
	// throughout so a move failure self-heals via the Lost loop. OpArchiving is a
	// DISTINCT op from a kill, so the fence can never be confused with a TUI
	// optimistic kill (#1187). The up-front guards (not archived, op==None) plus
	// the op-lock guarantee this edge is legal; a rejection would surface here
	// before any teardown.
	if err := instance.Transition(session.BeginArchive()); err != nil {
		return "", fmt.Errorf("cannot archive session %q: %w", req.Title, err)
	}

	// Tear down tmux and relocate the worktree in one call: the move is folded
	// into the teardown core immediately after the pane-exit wait (#1195 Ph2b),
	// so no live pane is cwd'd in the worktree during the move (previously a
	// separate MoveArchivedWorktree step relying on duplicated ordering prose).
	if err := instance.ArchiveTeardown(dest); err != nil {
		// The worktree is still at a valid location (the git layer guarantees
		// worktreePath points at the actual bytes even on a repair failure).
		// Roll the fence back to Lost — started is still true and the agent tmux
		// binding was kept — so the Lost-restore loop re-spawns the agent in place.
		// Persist the recovery-eligible state, then surface the failure.
		_ = instance.Transition(session.AbortArchiveToLost())
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("failed to archive session %q (its agent will be restored in place): %w", req.Title, err)
	}

	// Success: worktree relocated, tmux down. Commit the inert Archived state
	// (started=false, op cleared) — reachable only from the fence (I2) — and
	// persist the new path + status.
	_ = instance.Transition(session.CommitArchive())
	archivedPath := instance.GetWorktreePath()
	m.persistInstance(repoID, instance)
	log.InfoLog.Printf("archived session %q (repo %s): tmux torn down, worktree moved to %s", req.Title, repoID, archivedPath)
	return archivedPath, nil
}

// RestoreArchived restores an archived session (#1028): it moves the worktree
// back next to the repo (a free sibling path), re-registers it, re-spawns the
// agent, and marks the instance Running. Only the agent session is brought back
// — shell/process tabs were dropped at archive time. Returns the restored
// worktree path.
//
// Concurrency mirrors ArchiveSession/KillSession (killsInFlight + op-lock). On a
// repo-gone failure the archive is left intact with an actionable error; on a
// re-spawn failure the worktree is already back in place and the instance is
// left Lost so the #1108 restore loop heals it.
func (m *Manager) RestoreArchived(req RestoreArchivedRequest) (string, error) {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return "", err
	}
	if instance == nil {
		return "", fmt.Errorf("cannot restore session %q: no such session", req.Title)
	}
	if instance.GetLiveness() != session.LiveArchived {
		return "", fmt.Errorf("session %q is not archived", req.Title)
	}

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return "", fmt.Errorf("an operation is already in progress for session %q", req.Title)
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	// Re-verify under the op-lock (findSession released m.mu).
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance || instance.GetLiveness() != session.LiveArchived {
		return "", fmt.Errorf("session %q changed state before restore could start", req.Title)
	}

	repoPath := instance.GetRepoPath()
	if repoPath == "" {
		return "", fmt.Errorf("cannot restore session %q: no repo path on record", req.Title)
	}
	// Repo-gone check up front: SiblingWorktreePath and the worktree move both
	// need the origin repo, so surface the actionable message (archive left
	// intact) before either fails with a generic error.
	if _, statErr := os.Stat(repoPath); statErr != nil {
		return "", fmt.Errorf("cannot restore session %q: its origin repo %s is gone; the archived worktree is intact at %s — recover it manually with git", req.Title, repoPath, instance.GetWorktreePath())
	}
	dest, err := sessiongit.SiblingWorktreePath(repoPath, req.Title)
	if err != nil {
		return "", fmt.Errorf("cannot determine restore location for %q: %w", req.Title, err)
	}

	// Move the worktree back next to the repo. A repo-gone failure leaves the
	// archive intact (the git layer guarantees this) and surfaces an actionable
	// message; the instance stays Archived.
	if err := instance.RestoreArchivedWorktree(dest); err != nil {
		if errors.Is(err, sessiongit.ErrRepoGone) {
			return "", fmt.Errorf("cannot restore session %q: its origin repo is gone; the archived worktree is intact at %s — recover it manually with git: %w", req.Title, instance.GetWorktreePath(), err)
		}
		return "", fmt.Errorf("failed to restore worktree for %q: %w", req.Title, err)
	}

	// Worktree is back in place. Re-spawn the agent and flip Running. On a
	// re-spawn failure RestoreFromArchive leaves the instance started + Lost, so
	// the Lost-restore loop keeps retrying against the now-restored worktree.
	if err := instance.RestoreFromArchive(); err != nil {
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("restored worktree for %q but failed to re-spawn its agent (it will be retried): %w", req.Title, err)
	}

	worktreePath := instance.GetWorktreePath()
	m.persistInstance(repoID, instance)
	log.InfoLog.Printf("restored session %q (repo %s): worktree moved back to %s, agent re-spawned", req.Title, repoID, worktreePath)
	return worktreePath, nil
}

// archiveExclusiveTabLock serializes a tab spawn against an archive/kill/restore
// teardown for the session keyed by key (#1195). Those ops hold the per-session
// op-lock across their whole tmux teardown + worktree move, and — unlike Kill,
// which flips started=false — an archive keeps started=true throughout (so the
// #1108 rollback can self-heal a failed move to Lost), which means the
// instance-level #990 started guard never fires during archive. Without this, a
// CreateTab racing an ArchiveSession could spawn a tmux session into the worktree
// being moved out from under it, orphaning it.
//
// It takes the op-lock BEFORE any per-repo start lock the caller holds, matching
// the opLock→repoStartLock ordering the kill/archive paths use (persistInstance
// et al), so no ABBA deadlock is introduced. On success it returns the LOCKED
// op-lock — the caller must Unlock it. On rejection it releases the lock and
// returns an error: an archive/kill that beat us to the op-lock has fully
// completed by the time we acquire it (it held the lock across its entire
// teardown+move), so a mid-archive Deleting is never observed — only the terminal
// Archived. A completed kill leaves the stale instance started=false, which
// AddShellTab/AddProcessTab reject downstream.
func (m *Manager) archiveExclusiveTabLock(key string, instance *session.Instance) (*sync.Mutex, error) {
	opLock := m.opLockFor(key)
	opLock.Lock()
	if err := instance.TabSpawnBlocked(); err != nil {
		opLock.Unlock()
		return nil, err
	}
	return opLock, nil
}

// persistInstance writes one instance's authoritative data through the targeted
// per-repo writer under the repo start lock, mirroring refreshInstanceStatus's
// persist. Best-effort: a failed write only logs.
func (m *Manager) persistInstance(repoID string, instance *session.Instance) {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	err := persistInstanceData(repoID, instance.ToInstanceData())
	repoStartLock.Unlock()
	if err != nil {
		log.WarningLog.Printf("failed to persist instance %q: %v", instance.Title, err)
	}
}

// archivedWorktreePath returns the global archive location for a session's
// relocated worktree: <AGENT_FACTORY_HOME>/archived/<repoID>/<safeTitle>/. The
// repoID namespace prevents cross-repo title collisions; the title is sanitized
// for filesystem safety (the same scheme NewGitWorktree uses for session dirs).
func archivedWorktreePath(repoID, title string) (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve archive directory: %w", err)
	}
	return filepath.Join(dir, "archived", repoID, sanitizeArchiveTitle(title)), nil
}

// sanitizeArchiveTitle makes a session title safe as a single path segment,
// mirroring NewGitWorktree's safeSessionName handling (strip "..", "/"→"-",
// trim leading separators), falling back to "session" when nothing remains.
func sanitizeArchiveTitle(title string) string {
	s := strings.ReplaceAll(title, "..", "")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.TrimLeft(s, "-.")
	if s == "" {
		s = "session"
	}
	return s
}
