package session

// reconcilePendingArchiveRows updates group in-place, inside the per-repo file
// lock, against the just-read on-disk rows.
//
// Two separate races require reconciliation:
//
//  1. Tracked rows (inFlightArchive): the in-memory snapshot was taken while
//     OpArchiving was in flight (Branch non-empty). A targeted writer
//     (CommitArchive / persistInstanceData) may have already committed the
//     correct Archived or AbortArchiveToLost state to disk before this lock was
//     acquired. Prefer the committed disk row over the stale snapshot so the
//     wholesale overwrite never regresses a finished archive.
//
//  2. Untracked sandbox rows: an archive can raise OpArchiving AFTER
//     ToInstanceData collected the in-memory snapshot but BEFORE the file lock
//     was acquired. The checkpoint sees those as LiveRunning; the archive writer
//     may have already committed LiveArchived or LiveLost. Reconcile ALL
//     sandbox rows in the group, not just the tracked ones, to close this
//     window.
//
// In both cases the disk row is preferred only when:
//   - its committed liveness is a durable archive outcome (LiveArchived or
//     LiveLost with a non-empty Branch), AND
//   - its UpdatedAt is strictly after the snapshot's UpdatedAt, proving the
//     disk row advanced past the snapshot's archive — not a stale row from a
//     previous unrelated archive cycle.
func reconcilePendingArchiveRows(group []InstanceData, inFlightArchive map[string]struct{}, onDisk []InstanceData) {
	diskIndex := newestArchiveRowsByKey(onDisk)

	for i, row := range group {
		key := archiveRowKey(row)
		_, tracked := inFlightArchive[key]
		// For untracked rows, only reconcile sandbox-backend rows, which are
		// the ones an archive operation would touch.
		if !tracked && !isSandboxBackendType(row.BackendType) {
			continue
		}
		d, found := diskIndex[key]
		if !found {
			continue
		}
		if !committedArchiveOutcome(d) {
			continue
		}
		// Only prefer the disk row when it demonstrably advanced past this
		// snapshot: its UpdatedAt must be strictly later than the snapshot's.
		// This prevents adopting a stale Archived row from a previous archive
		// cycle while a fresh sandbox is running.
		if !d.UpdatedAt.After(row.UpdatedAt) {
			continue
		}
		group[i] = d
	}
}

// mergeCommittedArchiveRows appends a disk outcome only for a sandbox row that
// this checkpoint dropped before its current push completed, and only when the
// disk mutation is strictly newer than that dropped snapshot. The timestamp
// fence prevents a previous archive cycle's stale row from becoming proof of the
// current push merely because the current row is absent from group.
func mergeCommittedArchiveRows(
	group []InstanceData,
	onDisk []InstanceData,
	prePushArchive map[string]InstanceData,
) []InstanceData {
	if len(prePushArchive) == 0 {
		return group
	}
	// Build a key set for rows already in the group.
	inGroup := make(map[string]struct{}, len(group))
	for _, row := range group {
		inGroup[archiveRowKey(row)] = struct{}{}
	}
	diskIndex := newestArchiveRowsByKey(onDisk)
	seen := make(map[string]struct{}, len(diskIndex))
	for _, d := range onDisk {
		k := archiveRowKey(d)
		if _, done := seen[k]; done {
			continue
		}
		seen[k] = struct{}{}
		d = diskIndex[k]
		if !committedArchiveOutcome(d) {
			continue
		}
		if _, present := inGroup[k]; present {
			continue
		}
		prePush, tracked := prePushArchive[k]
		if !tracked || !d.UpdatedAt.After(prePush.UpdatedAt) {
			continue
		}
		group = append(group, d)
	}
	return group
}

func archiveRowKey(d InstanceData) string {
	if d.ID != "" {
		return d.ID
	}
	return d.Title
}

// newestArchiveRowsByKey collapses duplicate stable identities by mutation
// time. Targeted writers update the first matching legacy duplicate, so array
// order cannot decide which row carries the committed archive. On timestamp
// ties, a committed outcome wins over a non-committed stale duplicate.
func newestArchiveRowsByKey(rows []InstanceData) map[string]InstanceData {
	index := make(map[string]InstanceData, len(rows))
	for _, row := range rows {
		key := archiveRowKey(row)
		current, found := index[key]
		if !found || row.UpdatedAt.After(current.UpdatedAt) ||
			(row.UpdatedAt.Equal(current.UpdatedAt) && committedArchiveOutcome(row) &&
				!committedArchiveOutcome(current)) {
			index[key] = row
		}
	}
	return index
}

// committedArchiveOutcome reports whether a disk row represents a durable,
// committed outcome of an archive operation: either a successful archive
// (LiveArchived) or a push-succeeded/teardown-failed abort (LiveLost with a
// non-empty Branch that names the pushed work).
func committedArchiveOutcome(d InstanceData) bool {
	if d.Liveness == LiveArchived {
		return true
	}
	// AbortArchiveToLost: the push succeeded and branch is set, but sandbox
	// teardown failed. The row was transitioned to LiveLost carrying the pushed
	// branch so restore can re-provision from it. This is a durable committed
	// outcome that must not be overwritten with the stale pre-push snapshot.
	return d.Liveness == LiveLost && d.Branch != ""
}
