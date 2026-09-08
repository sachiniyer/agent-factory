package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// inspectArchivePortableNamespace runs only after the destination identity retry
// check on every filesystem: Lstat may find an equivalent spelling on a
// case-insensitive volume or report it absent on a case-sensitive one.
func inspectArchivePortableNamespace(repoID string, inst *session.Instance, dest string, destExists bool) error {
	base := filepath.Base(dest)
	collides := func(existing string) bool {
		// Existing exact spellings retain the caller's path-based owner
		// diagnostic. Missing paths still need exact persisted-name claims.
		return (!destExists || existing != base) && archiveTitlesCollide(existing, base)
	}
	collision := func(existing string) error {
		return fmt.Errorf("cannot archive session %q: destination %s collides with existing archive %q (same portable name)", inst.Title, dest, existing)
	}
	existing, err := archiveDirectoryCollision(dest, destExists)
	if err != nil {
		return fmt.Errorf("cannot archive session %q: %w", inst.Title, err)
	}
	if existing != "" {
		return collision(existing)
	}
	// Persisted rows still claim their archive name when refresh cannot restore
	// them or the directory is temporarily missing. Use the recorded path because
	// archived-title reuse can rename the row without relocating its worktree.
	disk, err := loadRepoInstanceData(repoID)
	if err != nil {
		return fmt.Errorf("cannot archive session %q: cannot read archive owners for destination %s: %w", inst.Title, dest, err)
	}
	for _, data := range disk {
		if data.Title == inst.Title || session.RecordedLiveness(data) != session.LiveArchived {
			continue
		}
		existing := archiveClaimName(data)
		if !collides(existing) {
			continue
		}
		owned, err := ownsArchiveDirectory(data)
		if err != nil {
			return err
		}
		if owned {
			return collision(existing)
		}
	}
	return nil
}

// archiveDirectoryCollision is shared by archive admission and archived-name
// reuse. Every existing entry claims its portable key, including orphan trees.
func archiveDirectoryCollision(dest string, destExists bool) (string, error) {
	entries, err := sessiongit.BoundedReadDir(filepath.Dir(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("cannot inspect archive directory %s; check that the filesystem is responsive, then retry archive: %w", filepath.Dir(dest), err)
	}
	base := filepath.Base(dest)
	for _, entry := range entries {
		if (!destExists || entry.Name() != base) && archiveTitlesCollide(entry.Name(), base) {
			return entry.Name(), nil
		}
	}
	return "", nil
}

func validateArchiveRelocationDestination(repoID, title string) error {
	dest, err := archivedWorktreePath(repoID, title)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve archive destination: %v", errTitleCheckFatal, err)
	}
	existing, err := archiveDirectoryCollision(dest, false)
	if err != nil {
		return fmt.Errorf("%w: %w", errTitleCheckFatal, err)
	}
	if existing != "" {
		return fmt.Errorf("destination %s collides with existing archive %q (same portable name)", dest, existing)
	}
	return nil
}
