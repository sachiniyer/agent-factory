package commands

import (
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/log"
)

// unreadableRepoIDs cross-checks the on-disk instances/ directory against the
// repoIDs GetAllInstances actually surfaced, and returns the ones it did not.
//
// It is a compensating control, not a nicety: LoadAllRepoInstances SKIPS a
// per-repo record file it cannot read (an unsupported newer schema version, a
// permission failure) and returns the rest, so a repo can be absent from its map
// while its records — and the branches those records describe — are very much
// still on disk. Reset treats each one it finds here like a corrupt record:
// leave the repo and its branches intact rather than erasing it wholesale, so an
// unreadable record never has its branch orphaned or deleted by guessing.
//
// Which is exactly why a FAILED read of this directory may not answer "no
// additional repos" (#2870). That answer silently disables the control, and the
// wipe then takes the whole instances/ tree including the records nobody could
// read. A missing directory is a determinate empty — a home with no records has
// nothing to preserve, and must still be resettable — but anything else fails
// closed, at plan time, before a single byte is deleted.
func unreadableRepoIDs(instancesDir string, seen map[string]struct{}) ([]string, error) {
	entries, err := os.ReadDir(instancesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read stored session records in %s: %w", instancesDir, err)
	}
	var unsurfaced []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := seen[e.Name()]; ok {
			continue
		}
		log.WarningLog.Printf("reset: leaving repo %s intact: unreadable session records", e.Name())
		unsurfaced = append(unsurfaced, e.Name())
	}
	return unsurfaced, nil
}
