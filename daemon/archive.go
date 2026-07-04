package daemon

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
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
	switch instance.GetStatus() {
	case session.Archived:
		return "", fmt.Errorf("session %q is already archived", req.Title)
	case session.Loading, session.Deleting:
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

	// Fence the operation window with Deleting: the status poll skips a Deleting
	// instance (and the checkpoint save skips persisting it) while tmux is down
	// and the worktree is mid-move, so it is never misread as Lost. started is
	// left true throughout so a move failure self-heals via the Lost loop.
	instance.SetStatus(session.Deleting)

	instance.ArchiveTeardown()

	if err := instance.MoveArchivedWorktree(dest); err != nil {
		// The worktree is still at a valid location (the git layer guarantees
		// worktreePath points at the actual bytes even on a repair failure).
		// Mark Lost — started is still true and the agent tmux binding was kept —
		// so the Lost-restore loop re-spawns the agent in place. Persist the
		// recovery-eligible state and surface the failure.
		instance.SetStatus(session.Lost)
		m.persistInstance(repoID, instance)
		return "", fmt.Errorf("failed to archive session %q (its agent will be restored in place): %w", req.Title, err)
	}

	// Success: worktree relocated, tmux down. Flip to the inert Archived state
	// (started=false) and persist the new path + status.
	instance.SetArchived()
	archivedPath := instance.GetWorktreePath()
	m.persistInstance(repoID, instance)
	log.InfoLog.Printf("archived session %q (repo %s): tmux torn down, worktree moved to %s", req.Title, repoID, archivedPath)
	return archivedPath, nil
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
