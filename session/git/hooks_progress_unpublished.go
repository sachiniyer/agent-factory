package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Called with the publication lock held. No publisher can have an unreferenced
// directory here, even if its write/sync stalled past the grace period. A crash
// releases the lock, and its abandoned directory becomes eligible after grace.
func pruneUnpublishedHookReceipts(dir string, entries []os.DirEntry, now time.Time) error {
	referenced := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if (!strings.HasPrefix(name, "progress-") && !strings.HasPrefix(name, "retired-entries-")) || !strings.HasSuffix(name, ".json") {
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("cannot determine receipt references from non-regular journal %s", name)
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		var p hookProgress
		if err := json.Unmarshal(data, &p); err != nil {
			return err
		}
		if !strings.HasPrefix(filepath.Base(p.Directory), "entries-") {
			return fmt.Errorf("journal %s has no receipt directory", name)
		}
		// Preserve by basename even when the journal used a symlink spelling
		// of this home. An ambiguous reference must retain, never delete.
		referenced[filepath.Base(p.Directory)] = true
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "entries-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if referenced[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if now.Sub(info.ModTime()) < progressGraceAge {
			continue
		}
		if _, err := withInactiveHookProgressLease(path, func() error { return os.RemoveAll(path) }); err != nil {
			return err
		}
	}
	return nil
}
