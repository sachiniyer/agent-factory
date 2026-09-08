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
		return (!destExists || existing != base) && archiveDiskNameKey(existing) == archiveDiskNameKey(base)
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
	// them or the directory is temporarily missing. Like create admission, retain
	// both the recorded location and the title-derived future destination.
	disk, err := loadArchiveOwnerData(repoID)
	if err != nil {
		return fmt.Errorf("cannot archive session %q: cannot read archive owners for destination %s: %w", inst.Title, dest, err)
	}
	for _, data := range disk {
		if data.Title == inst.Title || session.RecordedLiveness(data) != session.LiveArchived {
			continue
		}
		existing := ""
		for _, name := range archiveRecordClaimNames(data) {
			if collides(name) {
				existing = name
				break
			}
		}
		if existing == "" {
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
	names, err := snapshotArchiveDirectory(filepath.Dir(dest))
	if err != nil {
		return "", err
	}
	return names.collision(filepath.Base(dest), destExists), nil
}

// archiveDirectoryNames snapshots every literal spelling for each portable key.
// Keeping equivalent spellings allows exact-path owner diagnostics to skip only
// the exact entry while still refusing a differently spelled neighbour.
type archiveDirectoryNames map[string][]string

func snapshotArchiveDirectory(parent string) (archiveDirectoryNames, error) {
	entries, err := sessiongit.BoundedReadDir(parent)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cannot inspect archive directory %s; check that the filesystem is responsive, then retry archive: %w", parent, err)
	}
	names := make(archiveDirectoryNames, len(entries))
	for _, entry := range entries {
		key := archiveDiskNameKey(entry.Name())
		names[key] = append(names[key], entry.Name())
	}
	return names, nil
}

func (names archiveDirectoryNames) collision(base string, destExists bool) string {
	for _, name := range names[archiveDiskNameKey(base)] {
		if !destExists || name != base {
			return name
		}
	}
	return ""
}

// archiveRelocationSnapshot resolves and reads the parent once per suffix walk.
func archiveRelocationSnapshot(repoID string) (archiveDirectoryNames, error) {
	dest, err := archivedWorktreePath(repoID, "session")
	if err != nil {
		return nil, fmt.Errorf("%w: cannot resolve archive destination: %v", errTitleCheckFatal, err)
	}
	names, err := snapshotArchiveDirectory(filepath.Dir(dest))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errTitleCheckFatal, err)
	}
	return names, nil
}

func (names archiveDirectoryNames) validateArchiveRelocationDestination(title string) error {
	base := sanitizeArchiveTitle(title)
	if existing := names.collision(base, false); existing != "" {
		return fmt.Errorf("destination %s collides with existing archive %q (same portable name)", base, existing)
	}
	return nil
}
