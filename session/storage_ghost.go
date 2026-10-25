package session

// isLegacyTransientGhostRow identifies disposable projections, not sessions.
// Older binaries could strand Loading/Deleting rows on disk and every create
// path deliberately treats them as overwritable (#551): backfilling through
// ForStorage would settle their transient status and turn the ghost into a
// real title claim. Durable recovery markers are the exception — those rows
// represent work the daemon still owes — matching the loader-side carve-out
// (daemon.isLegacyTransientGhost in daemon/refresh_support.go) so the save and
// load paths reap the same rows.
//
// The two must agree: the loader skips disposable ghosts out of memory (and
// expects the next save to reap them), while durable ghosts stay on disk for
// repair. If the save path preserved rows the loader reaps, every save would
// resurrect them; if it reaped rows the loader keeps, each save would destroy
// them. Keep this predicate byte-identical to its daemon-side mirror.
func isLegacyTransientGhostRow(item InstanceData) bool {
	return (item.Status == Loading || item.Status == Deleting) &&
		item.PendingHandoffMission == "" && !item.RuntimeCleanupStateUnknown
}

// preserveUnmaterializedGhostRows appends on-disk rows that no in-memory
// instance represents, so the wholesale per-repo checkpoint rewrite does not
// silently erase them.
//
// refreshDaemonInstances deliberately leaves on disk any row
// fromInstanceDataForRefresh cannot materialize: it logs the failure, keeps the
// row counted against its task's cap (#1892), and promises the row "stays on
// disk for repair or manual cleanup" (daemon/daemon.go refresh loop). The
// shutdown checkpoint is the only production full-save; before this merge it
// rewrote the repo file purely from the in-memory instances, so in a repo that
// held at least one normally-loadable session every unloadable ("ghost") row in
// that repo was silently destroyed at daemon shutdown — the only durable
// record for PendingOnComplete teardown obligations (#4162, leaked
// environments), UserKilled tombstones whose kill could then never be finished
// (leaked tmux sessions/worktrees), armed LostRestoreWait retry records
// (#5295), incomplete-archive report rows (the only durable handle to retained
// source trees), and fail-closed relocation-recovery rows — plus loss of the
// ghost's contribution to the watch-task concurrency cap (#1892), letting a
// task exceed max_concurrent_runs after every restart.
//
// "Represented" is by title: titles are unique per repo (the daemon's
// findTitleConflictLocked enforces this on create), so an on-disk row whose
// title matches an in-memory instance is the durable record for that same
// logical session and the in-memory snapshot is authoritative — including a
// killed session the snapshot legitimately drops, which must NOT be
// re-persisted as a ghost (#819/#844). A row whose title is absent from memory
// is a true ghost: preserve it verbatim so the next daemon start can re-read
// it, re-count it against its task's cap, and re-offer repair.
//
// A disposable legacy transient ghost is the one carve-out: an older binary's
// stranded Loading/Deleting projection with no durable retention markers.
// Those rows represent work the daemon never owed (#551), and the loader
// already skips them out of memory, so preserving them here would resurrect
// exactly the ghosts the reap-on-save semantics exist to clear. The carve-out
// matches isLegacyTransientGhostRow and the existing
// TestRepoSaveReapsLegacyLoadingGhost / TestDaemonSaveReapsLegacyLoadingGhost
// contracts.
//
// dedupeInstanceData (run after this by SaveInstances) collapses any
// same-title duplicate; because represented rows are skipped here, ghosts
// appended below never share a title with an in-memory row, so dedupe leaves
// the appended ghost rows untouched.
func preserveUnmaterializedGhostRows(
	group []InstanceData,
	onDisk []InstanceData,
	inMemoryTitles map[string]struct{},
) []InstanceData {
	if len(onDisk) == 0 {
		return group
	}
	for _, d := range onDisk {
		if _, represented := inMemoryTitles[d.Title]; represented {
			continue
		}
		if isLegacyTransientGhostRow(d) {
			continue
		}
		group = append(group, d)
	}
	return group
}
