package session

import (
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// CheckpointLiveRows is the pre-drain checkpoint (#5846): it refreshes rows that
// already exist on disk from the live instances, and does nothing else.
//
// It exists because the whole-list SaveInstances writer takes its membership from
// a snapshot the caller captured BEFORE the repo file lock. That is correct for
// the final save, where every writer has been joined, but a checkpoint taken while
// a root replacement (or any targeted persist) can still commit would write the
// replaced instance back over the replacement's committed row. The property here
// is that a checkpoint never overwrites state newer than what it read:
//
//   - isCurrent is evaluated INSIDE the file lock, so a row whose instance has
//     been replaced or removed since the snapshot is skipped, not resurrected;
//   - a row that is not on disk is never added, and a disk row with no live
//     instance is left byte-for-byte (a replacement's committed record, a ghost);
//   - a repo with an archive in flight is left to its own targeted persist and the
//     final save, because reconciling archive fences is the whole-list writer's job.
//
// Any targeted persist that commits after this releases the lock simply applies on
// top, exactly as it would after any other write.
func (s *Storage) CheckpointLiveRows(instances []*Instance, isCurrent func(*Instance) bool) error {
	grouped := make(map[string][]*Instance)
	for _, inst := range instances {
		rid := inst.repoIDForStorage()
		grouped[rid] = append(grouped[rid], inst)
	}
	for rid, repoInstances := range grouped {
		path, pathErr := config.RepoInstancesPath(rid)
		if pathErr != nil {
			return pathErr
		}
		if err := config.WithFileLock(path, func() error {
			live := make([]*Instance, 0, len(repoInstances))
			for _, inst := range repoInstances {
				if isCurrent == nil || isCurrent(inst) {
					live = append(live, inst)
				}
			}
			checkpoint := snapshotInstancesForCheckpoint(live)
			if !checkpoint.shouldWrite() || checkpoint.needsArchiveReconcile() {
				return nil
			}
			raw, readErr := s.state.GetInstances(rid)
			if readErr != nil {
				if err := checkpointReadError(readErr, path); err != nil {
					return fmt.Errorf("checkpoint: read disk for repo %s: %w", rid, err)
				}
				return nil // unreadable file: never rewrite what cannot be read
			}
			onDisk, onDiskRaw := decodeOnDiskRows(raw)
			if len(onDisk) == 0 || len(onDisk) != len(onDiskRaw) {
				return nil
			}
			changed := false
			for _, data := range checkpoint.group {
				for i := range onDisk {
					if onDisk[i].Title != data.Title || !(data.ID != "" && onDisk[i].ID == data.ID || data.ID == "" && onDisk[i].ID == "") {
						continue
					}
					b, err := json.Marshal(data)
					if err != nil {
						return err
					}
					onDiskRaw[i] = b
					changed = true
					break
				}
			}
			if !changed {
				return nil
			}
			out, err := json.Marshal(onDiskRaw)
			if err != nil {
				return err
			}
			return s.state.SaveInstances(rid, out)
		}); err != nil {
			return err
		}
	}
	return nil
}
