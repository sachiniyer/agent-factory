package session

import (
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// DeleteInstanceByStableID removes an instance from storage only when the
// record still matches the stable session identity captured by the caller. A
// false nil result means a same-titled record exists but belongs to a different
// instance, so the caller must treat the delete as stale and leave it alone.
// Empty IDs are legacy-compatible and fall back to title matching.
//
// It takes the instances flock with a DEADLINE (config.WithFileLockTimeout), not
// the blocking WithFileLock every other Storage writer uses: a contended lock
// surfaces as a retryable config.ErrLockTimeout instead of parking the caller
// forever. See InstanceDeleteLockTimeout for why this writer in particular
// cannot afford an unbounded wait.
func (s *Storage) DeleteInstanceByStableID(title, id string) (bool, error) {
	path, pathErr := config.RepoInstancesPath(s.repoID)
	if pathErr != nil {
		return false, pathErr
	}
	deleted := false
	sameTitleDifferentID := false
	if err := config.WithFileLockTimeout(path, InstanceDeleteLockTimeout, func() error {
		raw, err := s.state.GetInstances(s.repoID)
		if err != nil {
			return err
		}
		if raw == nil || string(raw) == "[]" || string(raw) == "null" {
			return fmt.Errorf("instance not found: %s", title)
		}

		var data []InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("failed to parse instances: %w", err)
		}

		filtered := make([]InstanceData, 0, len(data))
		found := false
		for _, d := range data {
			if d.Title == title {
				if stableIDMatches(d.ID, id) {
					found = true
					deleted = true
					continue
				}
				sameTitleDifferentID = true
			}
			filtered = append(filtered, d)
		}

		if !found {
			if sameTitleDifferentID {
				return nil
			}
			return fmt.Errorf("instance not found: %s", title)
		}

		out, err := json.Marshal(filtered)
		if err != nil {
			return fmt.Errorf("failed to marshal instances: %w", err)
		}
		return s.state.SaveInstances(s.repoID, out)
	}); err != nil {
		return false, err
	}
	return deleted, nil
}

func stableIDMatches(recordID, expectedID string) bool {
	return expectedID == "" || recordID == "" || recordID == expectedID
}
