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

// splitSkippedRepos sorts a Snapshot's skipped set by the remedy each repo
// needs. Any reason other than unreadable counts as corrupted: an empty reason
// is what an older daemon sends, and corrupted is all it ever meant.
func splitSkippedRepos(skipped []daemon.SkippedRepo) (corrupted, unreadable []string) {
	for _, s := range skipped {
		if s.Reason == daemon.SkippedRepoReasonUnreadableInstancesJSON {
			unreadable = append(unreadable, s.RepoID)
			continue
		}
		corrupted = append(corrupted, s.RepoID)
	}
	return corrupted, unreadable
}

// skippedReposError is corruptedReposError for a daemon Snapshot's skipped set,
// which can name both corrupted and unreadable repos (#4783). Each kind gets its
// own sentence and remedy. A set with only corrupted repos renders exactly as
// corruptedReposError, so the daemon path and the disk fallback still read the
// same for the same corruption.
func skippedReposError(skipped []daemon.SkippedRepo) error {
	corrupted, unreadable := splitSkippedRepos(skipped)
	var parts []string
	if len(corrupted) > 0 {
		parts = append(parts, corruptedReposError(corrupted).Error())
	}
	if len(unreadable) > 0 {
		parts = append(parts, fmt.Sprintf("%d repo(s) have an instances.json the daemon could not read, and their sessions are hidden until it can: %s\n%s",
			len(unreadable), corruptedRepoPaths(unreadable), unreadableRepoRepairHint))
	}
	return errors.New(strings.Join(parts, "\n"))
}

// skippedReposSuffix is corruptedReposSuffix for a daemon Snapshot's skipped
// set, the miss caveat get and whoami append; see skippedReposError.
func skippedReposSuffix(skipped []daemon.SkippedRepo) string {
	corrupted, unreadable := splitSkippedRepos(skipped)
	var parts []string
	if len(corrupted) > 0 {
		parts = append(parts, corruptedReposSuffix(corrupted))
	}
	if len(unreadable) > 0 {
		parts = append(parts, fmt.Sprintf("%d repo(s) have an instances.json the daemon could not read and may be hiding it: %s\n%s",
			len(unreadable), corruptedRepoPaths(unreadable), unreadableRepoRepairHint))
	}
	return strings.Join(parts, "\n")
}
