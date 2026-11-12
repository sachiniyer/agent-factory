package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

func appendInstanceData(repoID string, data session.InstanceData) error {
	data = data.ForStorage()
	return config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		for i := range existing {
			if existing[i].Title != data.Title {
				continue
			}
			// A Loading ghost left by an older TUI binary (#551) should
			// be overwritten rather than blocking the new session.
			// validateTitleAvailableLocked already cleared this title,
			// so reaching here under a same-titled non-Loading entry
			// is a real conflict.
			if existing[i].Status == session.Loading {
				existing[i] = data
				return json.MarshalIndent(existing, "", "  ")
			}
			return nil, fmt.Errorf("session with title %q already exists: %w", data.Title, errConcurrentCreate)
		}
		existing = append(existing, data)
		return json.MarshalIndent(existing, "", "  ")
	})
}

// testHookPersistInstanceData runs on every targeted record write, after the
// storage projection is applied and before any disk I/O, so a test can fail one
// exact write in isolation. The #2781 duplicate-delivery repro needs the handoff
// settlement's persist — and only that one — to fail while every other write in
// the same session stays real. Returns nil in production, and for any write a
// test does not target.
var testHookPersistInstanceData = func(string, session.InstanceData) error { return nil }

// persistInstanceData replaces the on-disk record for data.Title in repoID's
// instances file with data, under the per-repo file lock, leaving every other
// record untouched. It is the targeted, clobber-safe persist primitive for
// in-place mutations of an existing session (CloseTab, status/limit
// polls, archive) — the single-writer direction of #960 — analogous to
// appendInstanceData for creates and DeleteInstanceByStableID for kills. It
// deliberately does NOT use a whole-list SaveInstances, which would re-serialize
// the manager's entire view and reintroduce the dual-writer clobber surface #960
// is retiring.
//
// It matches the row to overwrite by title AND stable id (#1723, the same
// "key by stable id, not title/ordinal" class as #1678/#1738): if a record with
// the same title carries a DIFFERENT stable id, a kill/recreate has replaced the
// session out from under this writer, so it REFUSES to write rather than clobber
// the new instance's identity with the caller's stale data. stableIDMatchesForDaemon
// treats an empty id on either side as a match, so legacy records without a
// stored id, and callers whose in-memory instance predates the id, still persist.
// Errors when no record with that title exists (the caller already resolved a
// live instance, so a missing disk record means storage drifted out from under
// us).
func persistInstanceData(repoID string, data session.InstanceData) error {
	data = data.ForStorage()
	if err := testHookPersistInstanceData(repoID, data); err != nil {
		return err
	}
	found := false
	sameTitleDifferentID := false
	if err := config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		// Prefer the row whose stable id matches; only if NO same-titled row
		// shares this id do we treat it as an identity change. Scanning the whole
		// slice (rather than deciding on the first title hit) keeps a stray
		// duplicate-title row — a foreign id ordered before the real one — from
		// masking the legitimate write and failing a live caller (Greptile P1).
		match := -1
		for i := range existing {
			if existing[i].Title != data.Title {
				continue
			}
			if stableIDMatchesForDaemon(existing[i].ID, data.ID) {
				match = i
				break
			}
			// A same-titled record with a different stable id belongs to a
			// different (newer) session; never overwrite its identity.
			sameTitleDifferentID = true
		}
		if match >= 0 {
			existing[match] = data
			found = true
			return json.MarshalIndent(existing, "", "  ")
		}
		// Leave the file unchanged when no matching-id record exists; the caller
		// turns !found / sameTitleDifferentID into an error below.
		return raw, nil
	}); err != nil {
		return err
	}
	if !found && sameTitleDifferentID {
		return fmt.Errorf("instance %q identity changed in storage", data.Title)
	}
	if !found {
		return fmt.Errorf("instance %q not found in storage", data.Title)
	}
	return nil
}

// renameInstanceDataTitle rewrites the on-disk record currently stored under
// oldTitle to newData, which carries the session's NEW title and relocated
// worktree path (feat: reuse archived name). It matches the record by oldTitle and
// — when both records carry one — the stable ID, so a title reused elsewhere can't
// misdirect the rewrite. It refuses to proceed if newData.Title already names a
// DIFFERENT record, keeping the rename from clobbering an unrelated session. Errors
// when no record under oldTitle exists (the caller resolved a live archived
// instance, so a missing disk record means storage drifted out from under us).
func renameInstanceDataTitle(repoID, oldTitle string, newData session.InstanceData) error {
	newData = newData.ForStorage()
	found := false
	if err := config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		// A Loading ghost left by an older TUI binary (#551) does NOT reserve a
		// title: appendInstanceData overwrites a same-titled Loading ghost rather
		// than treating it as a conflict, and findTitleConflictLocked skips Loading
		// rows when deciding a title is free — which is exactly why
		// uniqueArchivedTitleLocked could hand us a newData.Title a ghost still
		// holds. Drop any such ghost before the collision check so the rename
		// REPLACES the ghost instead of writing a second same-titled record beside
		// it (#1951). Without this the ghost's empty ID also makes
		// stableIDMatchesForDaemon report a match, so the collision guard below
		// never fires and the duplicate persists silently — two rows sharing one
		// title, i.e. instances.json corruption.
		kept := existing[:0]
		for i := range existing {
			if existing[i].Title == newData.Title && existing[i].Status == session.Loading {
				continue
			}
			kept = append(kept, existing[i])
		}
		existing = kept
		for i := range existing {
			if existing[i].Title == newData.Title && !stableIDMatchesForDaemon(existing[i].ID, newData.ID) {
				return nil, fmt.Errorf("cannot rename archived session to %q: another session already holds that title", newData.Title)
			}
		}
		for i := range existing {
			if existing[i].Title != oldTitle {
				continue
			}
			if !stableIDMatchesForDaemon(existing[i].ID, newData.ID) {
				continue
			}
			existing[i] = newData
			found = true
			return json.MarshalIndent(existing, "", "  ")
		}
		return raw, nil
	}); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("archived instance %q not found in storage", oldTitle)
	}
	return nil
}

func loadRepoInstanceData(repoID string) ([]session.InstanceData, error) {
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		return nil, err
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("failed to parse existing instances: %w", err)
	}
	return data, nil
}

// collectTitleRepoPathsOnDisk returns repoID -> repo path for every PERSISTED row
// holding the title, across all repos.
//
// It exists because the in-memory instance map is not proof of uniqueness: a
// repo's row is skipped during refresh when it cannot be restored (its worktree
// or tmux session is gone), so a single in-memory match can hide a second repo
// that also holds the title. Callers union this with their in-memory matches
// before concluding a title is unambiguous — otherwise a daemon-up unscoped
// kill/archive would act on the restored session while the daemon-down disk path
// would correctly refuse to guess.
//
// Corrupted per-repo files are skipped (mirroring findInstanceDataByTitle); a
// failure to enumerate repos at all, or a per-repo file that could not be READ,
// is returned as an error.
//
// The second return names the repos whose file could not be READ. It is not
// optional detail: the caller unions this set with its live match and reads a
// short set as "no other project holds this title", which is the one conclusion
// a partial read cannot support (#3479).
//
// It is a second RETURN rather than an early error on purpose. A gap must be
// reported ALONGSIDE the evidence, never instead of it: refusing here would
// discard the matches the readable repos did yield, and those matches are
// positive findings that settle the ambiguity question on their own. Returning
// them empty made the caller — which resolves its live match when this errors —
// miss a readable second project it used to catch, i.e. strictly worse than the
// fail-open being fixed. Caught in review of #3479 bucket 1.
//
// Defence in depth today: refreshDaemonInstances runs the per-repo file cache
// (config.RepoInstancesFileCache.LoadAll) first, whose loadRepoFile refuses hard
// on an unreadable per-repo file, so findSession never reaches here in that
// state (pinned by TestDaemonLoadGateRefusesUnreadableRepoBeforeAnyTitleResolution).
// Correct on its own terms anyway: an identity guard should not depend on an
// upstream gate nobody thinks of as part of it.
func collectTitleRepoPathsOnDisk(title string) (map[string]string, []config.RepoInstancesSkip, error) {
	allInstances, unreadable, err := config.LoadAllRepoInstancesReportingSkipDetails()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load instances: %w", err)
	}
	found := make(map[string]string)
	for rid, raw := range allInstances {
		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			log.WarningLog.Printf("daemon skipping repo %s while checking title ambiguity: corrupted instances.json: %v", rid, err)
			continue
		}
		for i := range data {
			if data[i].Title == title {
				found[rid] = data[i].Path
				break
			}
		}
	}
	return found, unreadable, nil
}

func findInstanceDataByTitle(title, repoID string) (*session.InstanceData, string, error) {
	if repoID != "" {
		data, err := loadRepoInstanceData(repoID)
		if err != nil {
			return nil, "", err
		}
		for i := range data {
			if data[i].Title == title {
				return &data[i], repoID, nil
			}
		}
		return nil, "", scopedInstanceMissError{title: title}
	}

	allInstances, unreadable, err := config.LoadAllRepoInstancesReportingSkipDetails()
	if err != nil {
		return nil, "", fmt.Errorf("failed to load instances: %w", err)
	}
	// Unscoped, so the answer is "(row, repoID)" AND the claim that no other
	// project holds the title — the caller treats that repoID as THE project.
	// A file this could not read is exactly the evidence that would refute the
	// claim, so unlike the corrupted case below there is no answer left worth
	// giving: refuse before scanning rather than resolve a match whose
	// uniqueness is unproven (#3479). The scoped branch above never gets here,
	// so a project scope remains the escape.
	if len(unreadable) > 0 {
		return nil, "", fmt.Errorf("cannot resolve instance %q without a project scope: %s, and any of them may hold this title too; scope the lookup to a project, or %s",
			title, config.DescribeRepoInstancesSkips(unreadable), config.RepoInstancesSkipRemedy(unreadable))
	}
	var corrupted []string
	// Titles are unique per-repo: collect all matches so an unscoped lookup
	// reports ambiguity instead of resolving whichever repo the map walk reached
	// first (the disk mirror of findSession's unscoped branch).
	var matches []session.InstanceData
	var matchRepoIDs []string
	for rid, raw := range allInstances {
		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			// Warn and record the corrupted repo rather than silently
			// skipping it (#730). If the target title lives in this repo we
			// would otherwise report a misleading "not found".
			log.WarningLog.Printf("daemon skipping repo %s: corrupted instances.json: %v", rid, err)
			corrupted = append(corrupted, rid)
			continue
		}
		for i := range data {
			if data[i].Title == title {
				matches = append(matches, data[i])
				matchRepoIDs = append(matchRepoIDs, rid)
			}
		}
	}
	// Only a title held by distinct REPOS is ambiguous; duplicate rows inside one
	// repo's instances.json are a corruption artifact, not a cross-project clash.
	if len(session.DedupeSorted(matchRepoIDs)) > 1 {
		paths := make([]string, 0, len(matches))
		for i := range matches {
			paths = append(paths, matches[i].Path)
		}
		return nil, "", session.AmbiguousTitleError(title, paths)
	}
	if len(matches) > 0 {
		return &matches[0], matchRepoIDs[0], nil
	}
	if len(corrupted) > 0 {
		sort.Strings(corrupted)
		return nil, "", fmt.Errorf("instance %q not found; %d repo(s) have a corrupted instances.json that may be hiding it: %s", title, len(corrupted), strings.Join(corrupted, ", "))
	}
	// The none-held half of the contract (#6882): the scan just covered every
	// project, so the miss says no project holds the title and how to scope
	// the next request rather than a bare "not found" that reads as data
	// loss. Its Unwrap keeps the clean-miss sentinel reachable.
	return nil, "", unscopedInstanceMissError{title: title}
}
