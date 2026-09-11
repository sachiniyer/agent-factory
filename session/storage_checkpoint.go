package session

// sealArchiveCheckpoint prevents any later BeginArchive and returns the current
// archive generation's settlement fence, if one is already in flight. The seal
// and BeginArchive both use i.mu, so there is no admission gap between them.
func (i *Instance) sealArchiveCheckpoint() <-chan struct{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.archiveCheckpointSealed = true
	return i.archiveSettled
}

// checkpointSnapshot is one repo's storage projection plus the archive rows
// that require a disk reconciliation while the repo file lock is held.
type checkpointSnapshot struct {
	group           []InstanceData
	inFlightArchive map[string]struct{}
	prePushArchive  map[string]InstanceData
}

func (s checkpointSnapshot) shouldWrite() bool {
	return len(s.group) > 0 || len(s.prePushArchive) > 0
}

func (s checkpointSnapshot) needsArchiveReconcile() bool {
	return len(s.inFlightArchive) > 0 || len(s.prePushArchive) > 0
}

// snapshotInstancesForCheckpoint applies SaveInstances' retention rules to a
// single repo. SaveInstances calls it once to decide whether the repo has a
// checkpoint claim, then again inside the repo file lock. The second snapshot
// closes the window where an archive begins after the first collection pass:
// the archive writer either settled before the lock (and this sees its final
// in-memory state) or persists after the checkpoint releases the lock.
func snapshotInstancesForCheckpoint(instances []*Instance) checkpointSnapshot {
	snapshot := checkpointSnapshot{}
	for _, inst := range instances {
		data := inst.ToInstanceData()
		status := data.Status
		pendingHandoff := data.PendingHandoffMission != ""
		pendingTaskOutcome := data.TaskRunInterruptionPending
		unknownRuntimeCleanup := data.RuntimeCleanupStateUnknown
		unresolvedRelocation := data.Worktree.RelocationRecovery != nil
		archiveReportPending := data.archiveReportPending
		isSandbox := isSandboxBackendType(data.BackendType)

		// The branch can survive an archive/restore round trip, so only the
		// process-local fence raised after this generation's push proves that the
		// current archive has crossed its durable boundary.
		pendingArchiveSandbox := isSandbox && data.InFlightOp == OpArchiving &&
			data.Liveness != LiveArchived && data.archivePushCompleted
		prePushArchiveSandbox := isSandbox && data.InFlightOp == OpArchiving &&
			data.Liveness != LiveArchived && !data.archivePushCompleted
		if prePushArchiveSandbox {
			if snapshot.prePushArchive == nil {
				snapshot.prePushArchive = make(map[string]InstanceData)
			}
			key := archiveRowKey(data)
			if prior, ok := snapshot.prePushArchive[key]; !ok || data.UpdatedAt.After(prior.UpdatedAt) {
				snapshot.prePushArchive[key] = data
			}
		}

		pendingTabs := len(data.PendingTabs) > 0
		durableRetention := pendingHandoff || pendingTaskOutcome || unknownRuntimeCleanup ||
			unresolvedRelocation || archiveReportPending || pendingArchiveSandbox
		lostSandbox := lostSandboxRecord(data)
		if (status == Loading || status == Deleting) && !durableRetention {
			continue
		}
		if !inst.Started() && status != Archived && !data.UserKilled &&
			!data.StartupStateUnknown && !durableRetention && !pendingTabs && !lostSandbox {
			continue
		}

		if pendingArchiveSandbox {
			// A live cleanup handle means teardown has not established completion.
			// Preserve that exact identity across restart; post-teardown snapshots
			// have no handle and must not manufacture an unknown-cleanup boundary.
			if data.runtimeCleanup != nil {
				data.RuntimeCleanupStateUnknown = true
			}
			if snapshot.inFlightArchive == nil {
				snapshot.inFlightArchive = make(map[string]struct{})
			}
			snapshot.inFlightArchive[archiveRowKey(data)] = struct{}{}
		}
		snapshot.group = append(snapshot.group, data.ForStorage())
	}
	return snapshot
}
