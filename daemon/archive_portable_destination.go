package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/session"
)

// inspectArchivePortableNamespace runs only after the destination identity retry
// check. An absent exact spelling is not sufficient on case-sensitive volumes:
// earlier legacy archives may have occupied another spelling of the same key.
func inspectArchivePortableNamespace(repoID string, inst *session.Instance, dest string) error {
	base := filepath.Base(dest)
	collision := func(existing string) error {
		return fmt.Errorf("cannot archive session %q: destination %s collides with existing archive %q (same portable name)", inst.Title, dest, existing)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot archive session %q: cannot inspect archive directory %s: %w", inst.Title, filepath.Dir(dest), err)
	}
	for _, entry := range entries {
		if archiveTitlesCollide(entry.Name(), base) {
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
		if !archiveTitlesCollide(existing, base) {
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
