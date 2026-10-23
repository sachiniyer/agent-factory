package daemon

// retractRereadOnUnloadableRows retracts a parsed-but-not-fully-loadable repo
// from reread so it stays skipped, and records a rows-failed-to-load skip entry
// (with the failed-row count). At startup that entry is the only signal that
// arms the orphan-sweep deferral for a repo whose file parsed but whose rows
// the daemon could not load — its still-running containers are just as absent
// from m.instances (and thus from the protected-slug set) as a corrupted
// repo's, so the sweep must defer on the same incomplete-view principle
// (#4998). On the polling path the entry also lets retainStillSkipped rewrite
// a previously-skipped repo's stale reason (corrupted/unreadable) to the
// accurate one — the file parsed; the worktree or tmux is what is gone
// (#4812, #4876).
//
// A PARTIAL loss is the same lie as a total one: list/get/whoami would serve
// N-1 of N rows as the complete answer the skip set exists to prevent, so the
// retraction fires whenever ANY row failed, not only when all did. A
// genuinely-empty file ([]) never reaches the caller's materialize loop: it
// is short-circuited there, so reread stays set and the repo still clears
// (zero sessions is the complete answer). A later poll that re-materializes
// every row re-arms reread and drops the repo (self-healing). Seeded on BOTH
// the startup and the polling refresh: at startup it spares the repo's live
// containers from the destructive sweep and refuses the partial snapshot on
// the wire; the polling path already refused it, and a mid-life row failure
// is invisible to the set anyway (retainStillSkipped returns nil when the
// startup skip set was empty, and the prior in-memory rows are re-hydrated
// so the failing rows are never reached on poll).
func retractRereadOnUnloadableRows(reread map[string]bool, repoID string, dataLen, materialized, failedRows int, skipped []SkippedRepo) []SkippedRepo {
	if materialized < dataLen {
		delete(reread, repoID)
		if failedRows > 0 {
			skipped = append(skipped, SkippedRepo{
				RepoID:     repoID,
				Reason:     SkippedRepoReasonRowsFailedToLoad,
				FailedRows: failedRows,
			})
		}
	}
	return skipped
}
