package daemon

// retractRereadOnUnloadableRows retracts a parsed-but-not-fully-loadable repo
// from reread so it stays skipped, and on the polling path records a
// rows-failed-to-load skip entry (with the failed-row count) so
// retainStillSkipped rewrites the previously-skipped repo's stale reason
// (corrupted/unreadable) to the accurate one — the file parsed; the worktree
// or tmux is what is gone (#4812, #4876).
//
// A PARTIAL loss is the same lie as a total one: list/get/whoami would serve
// N-1 of N rows as the complete answer the skip set exists to prevent, so the
// retraction fires whenever ANY row failed, not only when all did. A
// genuinely-empty file ([]) never reaches the caller's materialize loop: it
// is short-circuited there, so reread stays set and the repo still clears
// (zero sessions is the complete answer). A later poll that re-materializes
// every row re-arms reread and drops the repo (self-healing). Not seeded at
// startup (onPollingPath is false): a fresh repo with unloadable rows is out
// of this fix's scope, and seeding it would widen the startup skip set the PR
// does not touch.
func retractRereadOnUnloadableRows(reread map[string]bool, repoID string, dataLen, materialized, failedRows int, onPollingPath bool, skipped []SkippedRepo) []SkippedRepo {
	if materialized < dataLen {
		delete(reread, repoID)
		if onPollingPath && failedRows > 0 {
			skipped = append(skipped, SkippedRepo{
				RepoID:     repoID,
				Reason:     SkippedRepoReasonRowsFailedToLoad,
				FailedRows: failedRows,
			})
		}
	}
	return skipped
}
