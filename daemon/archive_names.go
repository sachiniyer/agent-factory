package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	folded := cases.Fold().String(norm.NFC.String(sanitizeArchiveTitle(title)))
	// Folding can decompose NFC input, so normalize the result as well.
	return norm.NFC.String(folded)
}

// archiveDestinationKey uses the same portable directory comparison as create
// admission. Legacy sessions can still have case/normalization-equivalent names.
func archiveDestinationKey(repoID, dest string) string {
	return daemonInstanceKey(repoID, filepath.Join(filepath.Dir(dest), archiveTitleKey(filepath.Base(dest))))
}

// releaseArchiveDestination cannot release a different instance's reservation.
// The archive caller defers it only after successful admission; failed probes
// release their own claim inside checkArchiveDestination.
func (m *Manager) releaseArchiveDestination(repoID string, inst *session.Instance, dest string) {
	key := archiveDestinationKey(repoID, dest)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reservedArchiveDestinations[key] == inst {
		delete(m.reservedArchiveDestinations, key)
	}
}

// checkArchiveDestination claims the destination before any filesystem probe or
// teardown and returns the pathname to pass to the move. The caller must defer
// releaseArchiveDestination with the original dest on success.
func (m *Manager) checkArchiveDestination(repoID string, inst *session.Instance, dest, source string) (moveDest string, err error) {
	if dest == "" {
		return "", fmt.Errorf("cannot archive session %q: archive destination is empty", inst.Title)
	}
	key := archiveDestinationKey(repoID, dest)
	m.mu.Lock()
	if owner := m.reservedArchiveDestinations[key]; owner != nil {
		ownerTitle := owner.Title
		m.mu.Unlock()
		return "", fmt.Errorf("cannot archive session %q: destination %s is being claimed by session %q", inst.Title, dest, ownerTitle)
	}
	if m.reservedArchiveDestinations == nil {
		m.reservedArchiveDestinations = make(map[string]*session.Instance)
	}
	m.reservedArchiveDestinations[key] = inst
	m.mu.Unlock()
	defer func() {
		if err != nil {
			m.releaseArchiveDestination(repoID, inst, dest)
		}
	}()
	return m.inspectArchiveDestination(repoID, inst, dest, source)
}

// inspectArchiveDestination runs before editors, hooks, or tabs are stopped.
// A retry already at its destination keeps the identity-checked source spelling
// so the git layer takes its repair path instead of attempting a no-replace move
// between two names for the same directory.
func (m *Manager) inspectArchiveDestination(repoID string, inst *session.Instance, dest, source string) (string, error) {
	if dest == source {
		return source, nil
	}
	destInfo, err := sessiongit.BoundedLstat(dest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dest, nil
		}
		return "", fmt.Errorf("cannot archive session %q: cannot inspect destination %s: %w", inst.Title, dest, err)
	}
	// Lstat follows parent-directory aliases without accepting a symlink that
	// occupies the final destination entry. Both probes retain their deadlines.
	if destInfo.IsDir() {
		sourceInfo, statErr := sessiongit.BoundedLstat(source)
		if statErr == nil && sourceInfo.IsDir() && os.SameFile(destInfo, sourceInfo) {
			return source, nil
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("cannot archive session %q: cannot inspect source %s: %w", inst.Title, source, statErr)
		}
	}
	// Prefer the actual recorded path: archived-name reuse can change a title,
	// and old records may carry paths that no longer follow today's derivation.
	m.mu.Lock()
	for key, other := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid == repoID && other != nil && other != inst && other.GetWorktreePath() == dest {
			owner := other.Title
			m.mu.Unlock()
			return "", fmt.Errorf("cannot archive session %q: destination %s already exists and belongs to session %q", inst.Title, dest, owner)
		}
	}
	m.mu.Unlock()
	disk, err := loadRepoInstanceData(repoID)
	if err != nil {
		return "", fmt.Errorf("cannot archive session %q: destination %s already exists; cannot read its session owner: %w", inst.Title, dest, err)
	}
	for _, data := range disk {
		if data.Title != inst.Title && data.Worktree.WorktreePath == dest {
			return "", fmt.Errorf("cannot archive session %q: destination %s already exists and belongs to session %q", inst.Title, dest, data.Title)
		}
	}
	return "", fmt.Errorf("cannot archive session %q: destination %s already exists; no existing session owns it", inst.Title, dest)
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
