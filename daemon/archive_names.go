package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// validateArchiveTitleLocked covers local, relocatable worktree name claims.
// The caller has already established that the candidate uses a local worktree.
// Branch names keep slashes, but archive directories fold them into dashes.
// ignore is the archived row the create is about to rename out of the way.
func (m *Manager) validateArchiveTitleLocked(repoID, title string, disk []session.InstanceData, ignore *session.Instance, inPlace bool) error {
	if inPlace {
		return nil
	}
	candidate := archiveTitleKey(title)
	collision := func(existing string) error {
		if archiveTitleKey(existing) == candidate {
			return fmt.Errorf("session titled %q already maps to archive directory %q", existing, sanitizeArchiveTitle(title))
		}
		return nil
	}
	for key := range m.reservedArchiveTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID {
			if err := collision(existing); err != nil {
				return err
			}
		}
	}
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil || inst == ignore || inst.Capabilities().Workspace != session.WorkspaceLocalWorktree || inst.IsExternalWorktree() {
			continue
		}
		if err := collision(inst.Title); err != nil {
			return err
		}
	}
	for _, data := range disk {
		if !data.UsesLocalTmux() || data.Status == session.Loading || (ignore != nil && data.Title == ignore.Title) {
			continue
		}
		if archiveTitleKey(data.Title) != candidate {
			continue
		}
		// Decode the same ownership projections as FromInstanceData before
		// interpreting ExternalWorktree. ForStorage sets it for af-owned trees
		// too, to keep older releases from destroying unresolved archives.
		decoded := data.RestoreArchiveRollbackFence()
		decoded, err := decoded.RestoreRelocationRecoveryOriginals()
		if err != nil {
			return fmt.Errorf("%w: cannot restore archive ownership for session %q: %v", errTitleCheckFatal, data.Title, err)
		}
		if !decoded.Worktree.ExternalWorktree {
			return collision(data.Title)
		}
	}
	return nil
}

// archiveTitleKey deliberately uses one portable comparison on every platform,
// including case-sensitive Linux filesystems. Keep sanitizeArchiveTitle as the
// on-disk spelling; only namespace admission folds case and Unicode composition.
func archiveTitleKey(title string) string {
	return cases.Fold().String(norm.NFC.String(sanitizeArchiveTitle(title)))
}

// checkArchiveDestination runs before editors, hooks, or tabs are stopped.
// A retry whose identity-checked source already occupies dest must still be
// allowed through to repair and settle the interrupted move.
func (m *Manager) checkArchiveDestination(repoID string, inst *session.Instance, dest, source string) error {
	if dest == "" {
		return fmt.Errorf("cannot archive session %q: archive destination is empty", inst.Title)
	}
	if dest == source {
		return nil
	}
	if _, err := sessiongit.BoundedLstat(dest); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot archive session %q: cannot inspect destination %s: %w", inst.Title, dest, err)
	}
	// Prefer the actual recorded path: archived-name reuse can change a title,
	// and old records may carry paths that no longer follow today's derivation.
	m.mu.Lock()
	for key, other := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid == repoID && other != nil && other != inst && other.GetWorktreePath() == dest {
			owner := other.Title
			m.mu.Unlock()
			return fmt.Errorf("cannot archive session %q: destination %s already exists and belongs to session %q", inst.Title, dest, owner)
		}
	}
	m.mu.Unlock()
	disk, err := loadRepoInstanceData(repoID)
	if err != nil {
		return fmt.Errorf("cannot archive session %q: destination %s already exists; cannot read its session owner: %w", inst.Title, dest, err)
	}
	for _, data := range disk {
		if data.Title != inst.Title && data.Worktree.WorktreePath == dest {
			return fmt.Errorf("cannot archive session %q: destination %s already exists and belongs to session %q", inst.Title, dest, data.Title)
		}
	}
	return fmt.Errorf("cannot archive session %q: destination %s already exists; no existing session owns it", inst.Title, dest)
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
