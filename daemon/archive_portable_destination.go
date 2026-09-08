package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/session"
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
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot archive session %q: cannot inspect archive directory %s: %w", inst.Title, filepath.Dir(dest), err)
	}
	for _, entry := range entries {
		if collides(entry.Name()) {
			return collision(entry.Name())
		}
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
		existing := sanitizeArchiveTitle(data.Title)
		if data.Worktree.WorktreePath != "" {
			existing = filepath.Base(data.Worktree.WorktreePath)
		}
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
