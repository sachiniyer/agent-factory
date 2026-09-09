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
	// Build a key→disk-row index for O(n) lookup.
	diskIndex := make(map[string]InstanceData, len(onDisk))
	for _, d := range onDisk {
		k := d.ID
		if k == "" {
			k = d.Title
		}
		diskIndex[k] = d
	}

	for i, row := range group {
		key := row.ID
		if key == "" {
			key = row.Title
		}
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

// mergeCommittedArchiveRows appends to group any rows from onDisk that carry a
// committed archive outcome (LiveArchived or LiveLost with a non-empty Branch)
// but are absent from group. This handles the race where a sandbox row was
// in the pre-push window (Branch still empty) when the checkpoint collected its
// in-memory snapshot, was therefore dropped from the in-memory group, but a
// targeted archive writer committed a durable row to disk before the file lock
// was acquired. Without this merge the wholesale write would overwrite the
// committed row with nothing (the row is absent from group), losing the only
// af-side handle to the pushed branch.
func mergeCommittedArchiveRows(group []InstanceData, onDisk []InstanceData) []InstanceData {
	// Build a key set for rows already in the group.
	inGroup := make(map[string]struct{}, len(group))
	for _, row := range group {
		k := row.ID
		if k == "" {
			k = row.Title
		}
		inGroup[k] = struct{}{}
	}
	for _, d := range onDisk {
		if !committedArchiveOutcome(d) {
			continue
		}
		k := d.ID
		if k == "" {
			k = d.Title
		}
		if _, present := inGroup[k]; present {
			continue
		}
		group = append(group, d)
	}
	return group
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
