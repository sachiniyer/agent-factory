package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
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

// persistInstanceData replaces the on-disk record for data.Title in repoID's
// instances file with data, under the per-repo file lock, leaving every other
// record untouched. It is the targeted, clobber-safe persist primitive for
// in-place mutations of an existing session (CloseTab, SetPRInfo, status/limit
// polls, archive) — the single-writer direction of #960 — analogous to
// appendInstanceData for creates and storage.DeleteInstance for kills. It
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
		return nil, "", fmt.Errorf("instance %q not found", title)
	}

	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, "", fmt.Errorf("failed to load instances: %w", err)
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
	return nil, "", fmt.Errorf("instance %q not found", title)
}

// ghostKillTmuxByName issues a tmux kill-session for a persisted sanitized
// name. Package-level so tests can stub it without invoking real tmux. The
// af_ prefix check refuses to act on names the daemon would never write, so a
// corrupted store can't make us kill an unrelated tmux session. Mirror of the
// api/sessions.go helper added in #536 — duplicated here because daemon/
// cannot import api/ without a cycle.
var ghostKillTmuxByName = func(sanitizedName string) error {
	if !strings.HasPrefix(sanitizedName, tmux.TmuxPrefix) {
		return fmt.Errorf("refusing to kill tmux session without %q prefix: %q", tmux.TmuxPrefix, sanitizedName)
	}
	return tmux.NewTmuxSessionFromSanitizedName(sanitizedName, "").CloseAndWaitForPaneExit()
}

// ghostCleanupWorktree performs best-effort worktree teardown for a ghost
// session whose live restore failed. Package-level so tests can stub it.
// Deliberately no uncommitted-changes check here, unlike the TUI kill path
// (#815): this runs daemon-side with no user to warn, only for sessions whose
// records are already unrestorable, and the caller has already committed to
// deleting the record — a status probe could only block cleanup, not save data.
var ghostCleanupWorktree = func(data *session.InstanceData, title string) {
	if data.Worktree.RepoPath == "" || data.Worktree.WorktreePath == "" || data.Worktree.ExternalWorktree {
		return
	}
	branchCreatedByUs := true
	if data.Worktree.BranchCreatedByUs != nil {
		branchCreatedByUs = *data.Worktree.BranchCreatedByUs
	}
	gw, gwErr := git.NewGitWorktreeFromStorage(
		data.Worktree.RepoPath,
		data.Worktree.WorktreePath,
		data.Worktree.SessionName,
		data.Worktree.BranchName,
		data.Worktree.BaseCommitSHA,
		data.Worktree.ExternalWorktree,
		branchCreatedByUs,
	)
	if gwErr != nil {
		log.WarningLog.Printf("ghost session %q: failed to load worktree for cleanup: %v", title, gwErr)
		return
	}
	if cleanupErr := gw.Cleanup(); cleanupErr != nil {
		log.WarningLog.Printf("ghost session %q: worktree cleanup failed: %v", title, cleanupErr)
	}
}

// ghostCleanup runs best-effort teardown of a ghost session's external
// resources. Tmux teardown is independent of worktree state (#516/#549): a
// ghost record can have an empty worktree path while a tmux session with the
// persisted name is still running, so the two branches share no condition.
// Tmux goes FIRST: a still-running agent writing into the worktree while git
// recursively deletes it leaks a half-deleted directory (#802).
func ghostCleanup(data *session.InstanceData, title string) {
	if data.TmuxName != "" {
		if killErr := ghostKillTmuxByName(data.TmuxName); killErr != nil {
			log.WarningLog.Printf("ghost session %q: tmux cleanup failed: %v", title, killErr)
		}
	}
	ghostCleanupWorktree(data, title)
}
