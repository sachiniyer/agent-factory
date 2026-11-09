package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// #6986: the daemon reports the repos it could not load on
// SnapshotResponse.SkippedRepos — a repo whose instances.json is corrupted or
// unreadable has sessions the daemon cannot show (#4742 added the field, #5413
// closed the same gap on the web). Every TUI read path dropped it, so the
// sidebar and the always-visible Projects section presented the truncated list
// as complete: a skipped repo's sessions vanished, and its Projects row read
// "0 sessions" — which is what delete-project's confirmation then claimed.
//
// The model here is deliberately the simple one: home holds ONE retained set —
// the skipped repos the LATEST successful snapshot reported — and every
// consumer reads it. Rows and skip state always travel inside the same
// SnapshotResponse, so they can never be sampled from two different snapshots.
// There are no per-picker generations and no fetch-start ordering: being
// briefly over-cautious after a skip clears is acceptable, acting on an
// incomplete view is not.
//
// Two kinds of fetch write the set:
//   - the unscoped all-repos read (RepoID:"") is the completeness witness for
//     the Projects section and the project picker — its skipped set replaces
//     the retained set wholesale (applySkippedRepos);
//   - a repo-scoped read reports only its own repo's entries, so it folds into
//     the set (mergeSkippedReposFor): its repo's entries replace, the rest
//     stand. A scope of "" is the unscoped read itself, so registry mode takes
//     the wholesale path.
//
// Both only ever run on a successful response: a failed fetch is not a
// healthy read, so it retains the last set rather than clearing the warning
// on a hiccup.
//
// While the set is non-empty the bar carries a standing notice naming the
// repos and their remedy — the same per-reason repair wording the CLI and web
// print — and a changed set announces once as a retained transient so `E`
// reaches the full text. The standing line is composed with the web-listener
// refusal notice so the two conditions never erase each other (#6997 review:
// owner-per-slot writes let one clobber the other).

// The four reason → remedy strings mirror api/skipped_repos.go's hint constants
// and api/api.go's corruptedRepoRepairHint verbatim — the same strings the
// web's skippedRepoRepairHint mirrors in TypeScript — so every surface sends
// the operator to the same remedy. The app package does not import api (the
// TUI is a thin HTTP client with no CLI surface), so they are duplicated here;
// skipped_repos_test.go pins them verbatim.
const (
	skippedRepoUnreadableHint = "Make the file(s) readable by the user the af daemon runs as (check ownership, permissions and the disk), then restart the daemon so it re-reads them; do not delete them, they hold the hidden sessions."

	skippedRepoNewerSchemaHint = "Upgrade af to a version that understands the newer state file, then restart the daemon; do not edit or delete these files, they hold sessions this binary cannot read."

	skippedRepoRowsFailedHint = "Restore or delete the sessions the daemon could not load (their worktree or tmux is gone), then the daemon re-reads the complete list on the next poll; e.g. `af sessions restore <title>` or `af sessions kill <title>`; the instances.json itself is not the problem."

	skippedRepoCorruptedHint = "Stop the af daemon, copy the corrupt file(s) aside, fix the JSON or restore it from a backup, then start the daemon; af keeps no backup of these files."
)

// applySkippedRepos replaces the retained set with the one a successful
// snapshot just reported and returns whether it changed (the caller repaints).
// An empty set retracts the notice — the latest read is complete. A non-empty
// set (re)arms the one-shot announcement: the first time THIS set is seen the
// poll drains it into a retained transient, so identical polls do not spam the
// bar every 750ms and a new set announces again.
func (m *home) applySkippedRepos(skipped []daemon.SkippedRepo) bool {
	if skippedReposEqual(m.skippedRepos, skipped) {
		return false
	}
	m.skippedRepos = append([]daemon.SkippedRepo(nil), skipped...)
	sort.Slice(m.skippedRepos, func(i, j int) bool {
		return m.skippedRepos[i].RepoID < m.skippedRepos[j].RepoID
	})
	m.skippedReposNoticeQueued = len(m.skippedRepos) > 0
	m.applyStandingNotice()
	return true
}

// mergeSkippedReposFor folds one repo-scoped reply's skipped set into the
// retained set: the repo's own entries are replaced by what the reply reports,
// the rest of the set stands. Called with the scope the fetch requested (the
// daemon scopes SkippedRepos to it), so an empty scope is the unscoped read —
// the reply IS the whole set and takes the wholesale path.
func (m *home) mergeSkippedReposFor(repoID string, entries []daemon.SkippedRepo) bool {
	if repoID == "" {
		return m.applySkippedRepos(entries)
	}
	next := make([]daemon.SkippedRepo, 0, len(m.skippedRepos)+len(entries))
	for _, s := range m.skippedRepos {
		if s.RepoID != repoID {
			next = append(next, s)
		}
	}
	return m.applySkippedRepos(append(next, entries...))
}

// skippedReposGuardRefusal is the one guard the destructive-view property hangs
// on: while the latest snapshot reports any skipped repo, an action whose
// safety depends on a complete view refuses — every count on screen may be
// hiding the sessions the skipped set names. The refusal names the repos so
// the reason is actionable, and it does not pretend the failure was a fetch
// error (#6997 review: conflating the two reasons was misleading).
func (m *home) skippedReposGuardRefusal() error {
	if len(m.skippedRepos) == 0 {
		return nil
	}
	return fmt.Errorf("the session list is incomplete — %d repo(s) skipped: %s; their sessions are hidden and an action over the whole list would act on a partial view",
		len(m.skippedRepos), m.skippedRepoNames())
}

// applyStandingNotice recomposes the bar's standing line from its independent
// owners. ErrBox carries ONE standing slot, so two conditions must not write
// it separately — each keeps its own state and this is the only writer.
func (m *home) applyStandingNotice() {
	var parts []string
	if s := m.skippedReposStanding(); s != "" {
		parts = append(parts, s)
	}
	if m.webListenerNotice != "" {
		parts = append(parts, m.webListenerNotice)
	}
	m.errBox.SetStanding(strings.Join(parts, " · "))
}

// skippedReposStanding is the one-line condition the bar holds while any repo
// is skipped: it names the repos AND their remedy so even a truncated render
// starts with what matters, and the full text is the retained notice.
func (m *home) skippedReposStanding() string {
	if len(m.skippedRepos) == 0 {
		return ""
	}
	return fmt.Sprintf("Some sessions are hidden — %d repo(s) skipped: %s — %s",
		len(m.skippedRepos), m.skippedRepoNames(), m.skippedRepoHints())
}

// skippedReposNoticeText is the full announcement fired once per distinct
// skipped set: one line per repo of "label — reason. hint", mirroring the
// web's skippedReposNotice rows so TUI and web describe the same loss the
// same way.
func (m *home) skippedReposNoticeText() string {
	if len(m.skippedRepos) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Some sessions are hidden — the daemon could not load %d repo(s):", len(m.skippedRepos))
	for _, s := range m.skippedRepos {
		fmt.Fprintf(&b, "\n%s — %s. %s",
			m.skippedRepoLabel(s.RepoID), skippedRepoReasonLabel(s.Reason), skippedRepoHint(s.Reason))
		if s.FailedRows > 0 {
			fmt.Fprintf(&b, " (%d session(s) hidden)", s.FailedRows)
		}
	}
	return b.String()
}

// skippedRepoNames lists each skipped repo's label for the compact lines.
func (m *home) skippedRepoNames() string {
	names := make([]string, 0, len(m.skippedRepos))
	for _, s := range m.skippedRepos {
		names = append(names, m.skippedRepoLabel(s.RepoID))
	}
	return strings.Join(names, ", ")
}

// skippedRepoHints joins each DISTINCT reason's repair hint once, in set
// order — two repos sharing a failure get one remedy, not two.
func (m *home) skippedRepoHints() string {
	seen := map[string]bool{}
	var hints []string
	for _, s := range m.skippedRepos {
		if hint := skippedRepoHint(s.Reason); !seen[hint] {
			seen[hint] = true
			hints = append(hints, hint)
		}
	}
	return strings.Join(hints, " ")
}

// skippedRepoLabel names a skipped repo by its project root when the Projects
// list knows it (a repoID is a short hash, not a path — the web resolves the
// same way through RegisteredProject.repo_id), else by its instances.json
// path — the file to repair — else the bare id.
func (m *home) skippedRepoLabel(repoID string) string {
	for _, p := range m.sidebarProjects {
		if p.RepoID == repoID && p.Root != "" {
			return p.Root
		}
	}
	if !isRemoteTarget() {
		if path, err := config.RepoInstancesPath(repoID); err == nil && path != "" {
			return path
		}
	}
	return repoID
}

// skippedRepoReasonLabel renders the daemon's stable reason code the way the
// web's skippedRepoReasonLabel does, so TUI and web name the same failure the
// same way. Unknown/empty falls back to corrupted, which is all an older
// daemon ever sent (api's splitSkippedRepos classifies the same way).
func skippedRepoReasonLabel(reason string) string {
	switch reason {
	case daemon.SkippedRepoReasonUnreadableInstancesJSON:
		return "instances.json could not be read"
	case daemon.SkippedRepoReasonNewerSchemaInstancesJSON:
		return "instances.json written by a newer af"
	case daemon.SkippedRepoReasonRowsFailedToLoad:
		return "some sessions could not be loaded (worktree or tmux gone)"
	default:
		return "instances.json is corrupted"
	}
}

// skippedRepoHint maps a reason to its repair hint. Unknown/empty falls back
// to corrupted, matching every other surface's classification.
func skippedRepoHint(reason string) string {
	switch reason {
	case daemon.SkippedRepoReasonUnreadableInstancesJSON:
		return skippedRepoUnreadableHint
	case daemon.SkippedRepoReasonNewerSchemaInstancesJSON:
		return skippedRepoNewerSchemaHint
	case daemon.SkippedRepoReasonRowsFailedToLoad:
		return skippedRepoRowsFailedHint
	default:
		return skippedRepoCorruptedHint
	}
}

// skippedReposEqual compares two sets ignoring order — the daemon builds the
// list from a map, so order is unstable, and set equality is what the
// announce-once latch needs.
func skippedReposEqual(a, b []daemon.SkippedRepo) bool {
	if len(a) != len(b) {
		return false
	}
	byID := make(map[string]daemon.SkippedRepo, len(a))
	for _, s := range a {
		byID[s.RepoID] = s
	}
	for _, s := range b {
		prev, ok := byID[s.RepoID]
		if !ok || prev != s {
			return false
		}
	}
	return true
}
