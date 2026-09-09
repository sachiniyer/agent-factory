package session

// reconcilePendingArchiveRows updates group in-place: for each row whose
// snapshot key (ID, or Title when ID is empty) appears in inFlightArchive,
// if the corresponding row in onDisk has Liveness == LiveArchived the disk
// version replaces the stale in-memory snapshot. This closes the race where
// SaveInstances snapshots a mid-archive row before ArchiveSandbox writes
// i.Branch, but a targeted writer (CommitArchive / persistInstanceData)
// commits the correct Archived state to disk before the per-repo file lock
// is acquired.
func reconcilePendingArchiveRows(group []InstanceData, inFlightArchive map[string]struct{}, onDisk []InstanceData) {
	for i, row := range group {
		key := row.ID
		if key == "" {
			key = row.Title
		}
		if _, tracked := inFlightArchive[key]; !tracked {
			continue
		}
		for _, d := range onDisk {
			diskKey := d.ID
			if diskKey == "" {
				diskKey = d.Title
			}
			if diskKey != key {
				continue
			}
			if d.Liveness == LiveArchived {
				group[i] = d
			}
			break
		}
	}
}
