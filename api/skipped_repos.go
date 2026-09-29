package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/daemon"
)

// unreadableRepoRepairHint is the remedy for a repo the daemon skipped because
// it could not READ its instances.json (#4783). corruptedRepoRepairHint says to
// fix the JSON, which is wrong advice for a file whose bytes may be fine and
// that has only lost its permissions or its disk. Deleting it would discard the
// very sessions the refusal is protecting, so the hint says not to.
const unreadableRepoRepairHint = "Make the file(s) readable by the user the af daemon runs as (check ownership, permissions and the disk), then restart the daemon so it re-reads them; do not delete them, they hold the hidden sessions."

// newerSchemaRepoRepairHint is the remedy for a repo whose instances.json a
// newer af wrote (#4783). Repairing or deleting that file discards sessions
// this binary cannot see, so the only safe step is an upgrade.
const newerSchemaRepoRepairHint = "Upgrade af to a version that understands the newer state file, then restart the daemon; do not edit or delete these files, they hold sessions this binary cannot read."

// rowsFailedRepoRepairHint is the remedy for a repo the daemon skips because its
// instances.json parsed but some of its rows could not be loaded — the worktree
// or tmux session a Live row points at is gone, not the file (#4876). The file is
// fine, so the operator restores or deletes the unloadable sessions, not the
// JSON, and the daemon re-reads the complete list on the next poll.
const rowsFailedRepoRepairHint = "Restore or delete the sessions the daemon could not load (their worktree or tmux is gone), then the daemon re-reads the complete list on the next poll; e.g. `af sessions restore <title>` or `af sessions delete <title>`; the instances.json itself is not the problem."

// skippedRepoGroups is a Snapshot's skipped set sorted by the remedy each repo
// needs.
type skippedRepoGroups struct {
	corrupted, unreadable, newerSchema []string
	rowsFailed                         []string
	rowsFailedRows                     int
}

// splitSkippedRepos groups the skipped set by reason. Any reason this client
// does not know counts as corrupted: an empty reason is what an older daemon
// sends, and corrupted is all it ever meant.
func splitSkippedRepos(skipped []daemon.SkippedRepo) skippedRepoGroups {
	var g skippedRepoGroups
	for _, s := range skipped {
		switch s.Reason {
		case daemon.SkippedRepoReasonUnreadableInstancesJSON:
			g.unreadable = append(g.unreadable, s.RepoID)
		case daemon.SkippedRepoReasonNewerSchemaInstancesJSON:
			g.newerSchema = append(g.newerSchema, s.RepoID)
		case daemon.SkippedRepoReasonRowsFailedToLoad:
			g.rowsFailed = append(g.rowsFailed, s.RepoID)
			g.rowsFailedRows += s.FailedRows
		default:
			g.corrupted = append(g.corrupted, s.RepoID)
		}
	}
	return g
}

// skippedReposError is corruptedReposError for a daemon Snapshot's skipped set,
// which can name corrupted, unreadable and newer-schema repos (#4783). Each kind
// gets its own sentence and remedy. A set with only corrupted repos renders
// exactly as corruptedReposError, so the daemon path and the disk fallback still
// read the same for the same corruption.
func skippedReposError(skipped []daemon.SkippedRepo) error {
	g := splitSkippedRepos(skipped)
	var parts []string
	if len(g.corrupted) > 0 {
		parts = append(parts, corruptedReposError(g.corrupted).Error())
	}
	if len(g.unreadable) > 0 {
		parts = append(parts, fmt.Sprintf("%d repo(s) have an instances.json the daemon could not read, and their sessions are hidden until it can: %s\n%s",
			len(g.unreadable), corruptedRepoPaths(g.unreadable), unreadableRepoRepairHint))
	}
	if len(g.newerSchema) > 0 {
		parts = append(parts, fmt.Sprintf("%d repo(s) have an instances.json written by a newer af, and their sessions are hidden until af is upgraded: %s\n%s",
			len(g.newerSchema), corruptedRepoPaths(g.newerSchema), newerSchemaRepoRepairHint))
	}
	if len(g.rowsFailed) > 0 {
		parts = append(parts, fmt.Sprintf("%d session(s) across %d repo(s) could not be loaded (worktree or tmux gone) and their sessions list is hidden until they are restored or deleted: %s\n%s",
			g.rowsFailedRows, len(g.rowsFailed), corruptedRepoPaths(g.rowsFailed), rowsFailedRepoRepairHint))
	}
	return errors.New(strings.Join(parts, "\n"))
}

// skippedReposSuffix is corruptedReposSuffix for a daemon Snapshot's skipped
// set, the miss caveat get and whoami append; see skippedReposError.
func skippedReposSuffix(skipped []daemon.SkippedRepo) string {
	g := splitSkippedRepos(skipped)
	var parts []string
	if len(g.corrupted) > 0 {
		parts = append(parts, corruptedReposSuffix(g.corrupted))
	}
	if len(g.unreadable) > 0 {
		parts = append(parts, fmt.Sprintf("%d repo(s) have an instances.json the daemon could not read and may be hiding it: %s\n%s",
			len(g.unreadable), corruptedRepoPaths(g.unreadable), unreadableRepoRepairHint))
	}
	if len(g.newerSchema) > 0 {
		parts = append(parts, fmt.Sprintf("%d repo(s) have an instances.json written by a newer af and may be hiding it: %s\n%s",
			len(g.newerSchema), corruptedRepoPaths(g.newerSchema), newerSchemaRepoRepairHint))
	}
	if len(g.rowsFailed) > 0 {
		parts = append(parts, fmt.Sprintf("%d session(s) across %d repo(s) could not be loaded (worktree or tmux gone) and may be hiding it: %s\n%s",
			g.rowsFailedRows, len(g.rowsFailed), corruptedRepoPaths(g.rowsFailed), rowsFailedRepoRepairHint))
	}
	return strings.Join(parts, "\n")
}
