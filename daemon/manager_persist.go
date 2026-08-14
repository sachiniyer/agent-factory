package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
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
// Corrupted per-repo files are skipped (mirroring findInstanceDataByTitle); only
// a failure to enumerate repos at all is returned as an error.
func collectTitleRepoPathsOnDisk(title string) (map[string]string, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, fmt.Errorf("failed to load instances: %w", err)
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
	return found, nil
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
		return nil, "", fmt.Errorf("instance %q %w", title, errSessionNotFound)
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
	return nil, "", fmt.Errorf("instance %q %w", title, errSessionNotFound)
}

// ghostKillTmuxByName issues a tmux kill-session for a persisted sanitized
// name. Package-level so tests can stub it without invoking real tmux. The
// af_ prefix check refuses to act on names the daemon would never write, so a
// corrupted store can't make us kill an unrelated tmux session. Mirror of the
// api/sessions.go helper added in #536 — duplicated here because daemon/
// cannot import api/ without a cycle.
// It reports the tmux.PaneState alongside the error for the same reason the
// teardown modes do (#1917): the caller goes on to DELETE this ghost's worktree,
// so it must be able to tell "tmux confirmed the session is gone" from "tmux never
// answered". A refused name never ran a tmux command, so its state is known.
//
// The bool reports that the session VANISHED WITH NO PANE OBSERVED, so its marker
// evidence was vacuous. The caller acts on it AFTER closing every ghost name —
// never per-name, or an earlier vanished tab would scan while a later one is
// still live, refuse, and return before that later tab is ever closed, leaving
// the record permanently stuck (#2998, Codex on #3001).
var ghostKillTmuxByName = func(sanitizedName string) (tmux.PaneState, bool, error) {
	if !strings.HasPrefix(sanitizedName, tmux.TmuxPrefix) {
		return tmux.PaneStateKnown, false, fmt.Errorf("refusing to kill tmux session without %q prefix: %q", tmux.TmuxPrefix, sanitizedName)
	}
	return tmux.NewTmuxSessionFromSanitizedName(sanitizedName, "").CloseAndWaitForPaneExitReportingBlindness()
}

// ghostCleanupWorktree performs best-effort worktree teardown for a ghost
// session whose live restore failed. Package-level so tests can stub it.
// Deliberately no uncommitted-changes check here, unlike the TUI kill path
// (#815): this runs daemon-side with no user to warn, only for sessions whose
// records are already unrestorable, and the caller has already committed to
// deleting the record — a status probe could only block cleanup, not save data.
//
// "Best-effort" covers what git ANSWERED with. It does NOT cover a cleanup cut off
// by its own deadline: the caller deletes this ghost's record next, and that record
// is the only handle anything has on the leftovers. Report that so the caller keeps
// it (#1917) — the third site in this PR where a bounded call failed, someone logged
// it, and a destructive step went ahead anyway.
// ghostWorktreeRemovable is the eligibility predicate ghostCleanupWorktree bails
// on, named once so the occupancy gate cannot guard a workspace that the cleanup
// would never touch. Gating a record whose cleanup is a no-op can only retain it
// forever (#2998 review).
func ghostWorktreeRemovable(data *session.InstanceData) bool {
	current := data.RestoreArchiveRollbackFence()
	restored, err := current.RestoreRelocationRecoveryOriginals()
	return err == nil && ghostRestoredWorktreeRemovable(&restored)
}

// validateGhostWorktreeDestructionAdmission is the persisted-row twin of
// Instance.ValidateWorktreeDestructionAdmission. A cleanup-ready ghost has no
// live GitWorktree, so reconstruct only its recovery handle, normalize restart
// state, and validate the exact archive plus the still-missing origin before the
// kill tombstone is written.
func validateGhostWorktreeDestructionAdmission(data *session.InstanceData) error {
	current := data.RestoreArchiveRollbackFence()
	restored, err := current.RestoreRelocationRecoveryOriginals()
	if err != nil {
		return fmt.Errorf("restore cleanup ownership: %w", err)
	}
	recovery := restored.Worktree.RelocationRecovery
	if recovery == nil {
		return nil
	}
	if recovery.State == git.RelocationRecoveryCleanupStalled && !recovery.IdentityKnown {
		// Preserve the existing generic-cleanup retry admission. It carries no
		// repo-gone deletion identity and is outside this specialized consumer.
		return nil
	}
	if recovery.State != git.RelocationRecoveryCleanupReady &&
		recovery.State != git.RelocationRecoveryCleanupStalled &&
		recovery.State != git.RelocationRecoveryCleanupFinalizing {
		return fmt.Errorf("persisted worktree recovery state %q is unresolved", recovery.State)
	}
	if !ghostRestoredWorktreeRemovable(&restored) {
		return fmt.Errorf("cleanup-ready recovery does not identify an AF-owned worktree")
	}
	branchCreatedByUs := false
	if restored.Worktree.BranchCreatedByUs != nil {
		branchCreatedByUs = *restored.Worktree.BranchCreatedByUs
	}
	gw, err := git.NewGitWorktreeFromStorage(
		restored.Worktree.RepoPath,
		restored.Worktree.WorktreePath,
		restored.Worktree.SessionName,
		restored.Worktree.BranchName,
		restored.Worktree.BaseCommitSHA,
		restored.Worktree.ExternalWorktree,
		branchCreatedByUs,
	)
	if err != nil {
		return fmt.Errorf("load cleanup-ready worktree: %w", err)
	}
	if err := gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:             recovery.State,
		AlternatePath:     recovery.AlternatePath,
		IdentityKnown:     recovery.IdentityKnown,
		Device:            recovery.Device,
		Inode:             recovery.Inode,
		FileType:          recovery.FileType,
		CleanupGeneration: recovery.CleanupGeneration,
	}); err != nil {
		return fmt.Errorf("restore cleanup-ready recovery: %w", err)
	}
	_, normalized, unresolved := gw.RelocationSnapshot()
	if !unresolved ||
		(normalized.State != git.RelocationRecoveryCleanupReady &&
			normalized.State != git.RelocationRecoveryCleanupFinalizing) {
		return fmt.Errorf("persisted cleanup recovery normalized to non-admissible state %q", normalized.State)
	}
	return gw.ValidateRelocationCleanupAdmission()
}

var ghostCleanupWorktree = func(
	data *session.InstanceData,
	title string,
	checkpoint func(*session.InstanceData) error,
) (git.CleanupState, error, <-chan error) {
	current := data.RestoreArchiveRollbackFence()
	restored, restoreErr := current.RestoreRelocationRecoveryOriginals()
	if restoreErr != nil {
		return git.CleanupStateUnknown, fmt.Errorf("ghost recovery ownership is invalid: %w", restoreErr), nil
	}
	if restored.ArchiveReport != nil {
		report := restored.ArchiveReport.Clone()
		report.RollbackFence = nil
		restored.ArchiveReport = &report
	}
	checkpointBaseline := restored
	if recovery := restored.Worktree.RelocationRecovery; recovery != nil {
		clone := *recovery
		checkpointBaseline.Worktree.RelocationRecovery = &clone
	}
	if restored.ArchiveReport != nil {
		report := restored.ArchiveReport.Clone()
		checkpointBaseline.ArchiveReport = &report
	}
	*data = restored
	persisted := data
	if !ghostRestoredWorktreeRemovable(data) {
		return git.CleanupSettled, nil, nil
	}
	// Unknown provenance means KEEP (#1953): a nil flag predates 2026-04-17 and
	// cannot establish that AF created the branch, and the only thing this
	// authorizes downstream is Cleanup()'s `git branch -D`. Mirrors the default in
	// session.FromInstanceData. (The ExternalWorktree bail above already covers
	// external records; this covers a legacy linked worktree that Setup built on
	// a branch the user already had.)
	branchCreatedByUs := false
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
		// Nothing was attempted, so nothing is unknown; the record may still go.
		log.WarningLog.Printf("ghost session %q: failed to load worktree for cleanup: %v", title, gwErr)
		return git.CleanupSettled, nil, nil
	}
	recovery := data.Worktree.RelocationRecovery
	if recovery != nil {
		if recoveryErr := gw.RestoreRelocationRecovery(git.RelocationRecovery{
			State:             recovery.State,
			AlternatePath:     recovery.AlternatePath,
			IdentityKnown:     recovery.IdentityKnown,
			Device:            recovery.Device,
			Inode:             recovery.Inode,
			FileType:          recovery.FileType,
			CleanupGeneration: recovery.CleanupGeneration,
		}); recoveryErr != nil {
			log.WarningLog.Printf("ghost session %q: invalid relocation recovery handle: %v", title, recoveryErr)
			return git.CleanupStateUnknown, recoveryErr, nil
		}
	}
	if recovery != nil && !gw.HasUnresolvedRelocation() {
		// RestoreRelocationRecovery normalized the identity-unknown
		// cleanup_stalled record away as process-epoch state (#3278 review).
		// Mirror that in the loaded row: leaving the raw pointer would let the
		// caller misclassify this record-free run as a descriptor cleanup
		// after a boundary refusal and latch "restart before retrying" over a
		// worker that never ran.
		data.Worktree.RelocationRecovery = nil
	}
	if data.ArchiveReport != nil {
		gw.RestoreArchiveReport(data.ArchiveReport.Clone())
	}
	restoreCheckpoint := gw.SetRepoGoneFinalizationCheckpoint(func() error {
		beforeGhostCheckpointSnapshot()
		// A descriptor worker may outlive ghostCleanup's caller deadline. Build a
		// detached checkpoint so late persistence never races reads of the temporary
		// loaded row owned by the manager call.
		checkpointData := checkpointBaseline
		if recovery := checkpointBaseline.Worktree.RelocationRecovery; recovery != nil {
			clone := *recovery
			checkpointData.Worktree.RelocationRecovery = &clone
		}
		if checkpointBaseline.ArchiveReport != nil {
			report := checkpointBaseline.ArchiveReport.Clone()
			checkpointData.ArchiveReport = &report
		}
		projectGhostPersistenceSnapshot(&checkpointData, gw)
		if checkpoint == nil {
			return nil
		}
		return checkpoint(&checkpointData)
	})
	defer restoreCheckpoint()
	_, normalized, unresolved := gw.RelocationSnapshot()
	if unresolved && (normalized.State == git.RelocationRecoveryCleanupReady ||
		normalized.State == git.RelocationRecoveryCleanupFinalizing) {
		claim, claimErr := gw.ClaimRelocationSource()
		if claimErr != nil {
			return git.CleanupStateUnknown, claimErr, nil
		}
		state, cleanupErr, lateResult := gw.CleanupClaimedRepoGoneWithLateResult(claim)
		projectGhostPersistenceSnapshot(persisted, gw)
		return state, cleanupErr, lateResult
	}
	// Ghost twin of the live deletion-boundary recheck (#3278 review). The
	// admission probe was point-in-time; re-establish the origin immediately
	// before ordinary cleanup while the archived directory still exists, or
	// answered missing-origin failures would settle the teardown and the row
	// delete would orphan an archive a ghost can never re-authorize.
	// Derived from the NORMALIZED lifecycle, not the raw persisted pointer
	// (#3278 review): RestoreRelocationRecovery deliberately drops an
	// identity-unknown cleanup_stalled record as process-epoch state, so a
	// restarted daemon's gw is record-free even though the row still carries
	// the pointer — and that run needs every record-free guard.
	archivedRecordFree := data.Status == session.Archived &&
		!unresolved && ghostRestoredWorktreeRemovable(data)
	if archivedRecordFree {
		if _, statErr := git.BoundedLstat(data.Worktree.WorktreePath); statErr != nil {
			// A timeout must stop here rather than fall through to Cleanup's
			// own unbounded stat of the same stalled path (#3278 review);
			// only ENOENT means there is nothing left to guard.
			if !errors.Is(statErr, os.ErrNotExist) {
				return git.CleanupStateUnknown, fmt.Errorf(
					"the archived ghost worktree's state at %s could not be established at its cleanup boundary — kill again once the path answers: %w",
					data.Worktree.WorktreePath, statErr,
				), nil
			}
		} else if originErr := git.CheckRepoPresentForRelocation(data.Worktree.RepoPath); originErr != nil {
			return git.CleanupStateUnknown, fmt.Errorf(
				"origin repo state for this archived ghost could not be proven present at its cleanup boundary; the archived worktree was left intact — kill again once the origin state settles: %w",
				originErr,
			), nil
		}
	}
	cleanup := gw.Cleanup
	if archivedRecordFree {
		// Registered-only, like the live archived kill (#3278 review): the
		// unregistered RemoveAll fallback cannot distinguish the archive from
		// a directory that replaced it.
		cleanup = gw.CleanupRegisteredOnly
	}
	state, cleanupErr := cleanup()
	// Same postcondition as the live teardown (#3278 review): the pre-check
	// narrows the race, this closes it. A settled ordinary cleanup must prove
	// the archive conclusively absent before the row, its only handle, may be
	// deleted.
	if archivedRecordFree && state == git.CleanupSettled {
		if settleErr := session.ArchivedCleanupSettled(data.Worktree.WorktreePath); settleErr != nil {
			retained := fmt.Errorf(
				"ordinary ghost cleanup reported settled but %v — kill again to re-establish its state",
				settleErr,
			)
			if cleanupErr != nil {
				retained = errors.Join(retained, cleanupErr)
			}
			return git.CleanupStateUnknown, retained, nil
		}
	}
	if cleanupErr != nil {
		log.WarningLog.Printf("ghost session %q: worktree cleanup failed: %v", title, cleanupErr)
	}
	return state, cleanupErr, nil
}

var beforeGhostCheckpointSnapshot = func() {}

// projectGhostPersistenceSnapshot copies relocation ownership and archive
// completeness from one atomic GitWorktree snapshot. ForStorage rebuilds every
// rollback projection from these canonical current-reader values before writing.
func projectGhostPersistenceSnapshot(data *session.InstanceData, gw *git.GitWorktree) {
	path, recovery, ok, report := gw.PersistenceSnapshot()
	data.Worktree.WorktreePath = path
	if !ok {
		data.Worktree.RelocationRecovery = nil
	} else {
		persisted := data.Worktree.RelocationRecovery
		if persisted == nil {
			external := data.Worktree.ExternalWorktree
			startupUnknown := data.StartupStateUnknown
			persisted = &session.GitWorktreeRelocationRecoveryData{
				OriginalExternalWorktree:    &external,
				OriginalBranchCreatedByUs:   cloneBool(data.Worktree.BranchCreatedByUs),
				OriginalStartupStateUnknown: &startupUnknown,
			}
			data.Worktree.RelocationRecovery = persisted
		}
		persisted.State = recovery.State
		persisted.CleanupLifecycle = ""
		persisted.AlternatePath = recovery.AlternatePath
		persisted.IdentityKnown = recovery.IdentityKnown
		persisted.Device = recovery.Device
		persisted.Inode = recovery.Inode
		persisted.FileType = recovery.FileType
		persisted.CleanupGeneration = recovery.CleanupGeneration
		persisted.CleanupOriginalExternalWorktree = nil
		persisted.CleanupOriginalBranchCreatedByUs = nil
		persisted.CleanupOriginalStartupStateUnknown = nil
	}
	if report.Empty() {
		data.ArchiveReport = nil
	} else {
		report.RollbackFence = nil
		data.ArchiveReport = &report
	}
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func ghostRestoredWorktreeRemovable(data *session.InstanceData) bool {
	return data.Worktree.RepoPath != "" && data.Worktree.WorktreePath != "" && !data.Worktree.ExternalWorktree
}

func (m *Manager) ghostCleanupStallActive(key, stableID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	stalledID, ok := m.ghostCleanupStalls[key]
	if !ok {
		return false
	}
	if stalledID != "" && stableID != "" && stalledID != stableID {
		delete(m.ghostCleanupStalls, key)
		return false
	}
	return true
}

func (m *Manager) markGhostCleanupStalled(key, stableID string) {
	m.mu.Lock()
	if m.ghostCleanupStalls == nil {
		m.ghostCleanupStalls = make(map[string]string)
	}
	m.ghostCleanupStalls[key] = stableID
	m.mu.Unlock()
}

func (m *Manager) clearGhostCleanupStall(key, stableID string) {
	m.mu.Lock()
	if current, ok := m.ghostCleanupStalls[key]; ok && stableIDMatchesForDaemon(current, stableID) {
		delete(m.ghostCleanupStalls, key)
	}
	m.mu.Unlock()
}

// completeLateGhostKill applies the same observable tail as a synchronous ghost
// kill after its identity-anchored worker removes the durable row. Root grace is
// armed before publishing removal so observers cannot reinterpret it as live.
func (m *Manager) completeLateGhostKill(repoID, title, stableID string) {
	if session.IsReservedTitle(title) {
		m.mu.Lock()
		m.rootKilledAt[repoID] = nowFunc()
		m.mu.Unlock()
		log.InfoLog.Printf("root agent for repo %s: finished a late ghost kill; the ensure loop will re-create it in ~%s unless the repo is removed from root_agents", repoID, rootKillHealDelay)
	}
	m.publishEvent(agentproto.EventSessionKilled, session.InstanceData{ID: stableID, Title: title})
}

func (m *Manager) persistGhostCleanupStall(repoID string, data *session.InstanceData) error {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	data.UserKilled = true
	return persistInstanceData(repoID, *data)
}

var (
	lateGhostDeleteSessionRecord  = (*Manager).deleteSessionRecord
	lateGhostCleanupRetryInterval = 10 * time.Second
)

// reconcileLateGhostCleanup consumes the descriptor worker's definitive result.
// The row remains the retry handle on any error; only a successful delete plus
// the normal editor fence may remove it.
func (m *Manager) reconcileLateGhostCleanup(repoID, title, key, stableID string, lateResult <-chan error) {
	go func() {
		if err := <-lateResult; err != nil {
			m.clearGhostCleanupStall(key, stableID)
			log.WarningLog.Printf("ghost session %q: descriptor cleanup finished late with an error; retaining its stalled record: %v", title, err)
			return
		}
		for {
			err := m.stopVSCodeForInstance(key, stableID)
			if err == nil {
				var deleted bool
				deleted, err = lateGhostDeleteSessionRecord(m, repoID, title, stableID, nil)
				if err == nil {
					m.clearGhostCleanupStall(key, stableID)
					if deleted {
						m.completeLateGhostKill(repoID, title, stableID)
						log.InfoLog.Printf("ghost session %q: reconciled late descriptor cleanup and removed its durable row", title)
					} else {
						log.InfoLog.Printf("ghost session %q: late descriptor cleanup belongs to a replaced row; releasing its process fence", title)
					}
					return
				}
			}
			log.WarningLog.Printf("ghost session %q: descriptor cleanup finished late, but final record cleanup failed; retrying in %s: %v", title, lateGhostCleanupRetryInterval, err)
			timer := time.NewTimer(lateGhostCleanupRetryInterval)
			<-timer.C
		}
	}()
}

// reconcileSettledGhostCleanup gives a synchronous descriptor success the same
// stable-ID finalizer when a later editor or row-delete step fails. Admission
// cannot be retried after the archive is gone; this finalizer owns only the tail.
func (m *Manager) reconcileSettledGhostCleanup(repoID, title, key, stableID string) {
	m.markGhostCleanupStalled(key, stableID)
	settled := make(chan error, 1)
	settled <- nil
	m.reconcileLateGhostCleanup(repoID, title, key, stableID, settled)
}

// ghostTmuxNames returns the deduped, ordered set of tmux session names a ghost
// record owns: the legacy agent-tab name (data.TmuxName) first, then each live
// tab's name, then each pending-cleanup handle's name, all in persisted order.
// Empty names are skipped. A post-#953 record repeats the agent tab in BOTH
// data.TmuxName and data.Tabs, so the dedupe collapses that to a single kill; a
// pre-#953 record has no Tabs and yields just data.TmuxName, keeping the legacy
// path byte-identical.
func ghostTmuxNames(data *session.InstanceData) []string {
	// Pre-sized to len(data.Tabs), without allocation-size arithmetic. The legacy
	// name is already one of the tabs on any post-#953 record, and the uncommon
	// pending handles can grow the slice themselves.
	//
	// The arithmetic tripped CodeQL's go/allocation-size-overflow (high) on
	// `len(...)+1` inside make(). That was a FALSE POSITIVE — data.Tabs is a
	// persisted session's tab list (an agent tab plus a handful of shell/process/
	// web tabs), so reaching an overflow would need ~2^63 tabs, which cannot
	// exist. But it is the same trade as #1988: the expression bought nothing on
	// a path that runs once per ghost record during cleanup, and code with no
	// allocation-size arithmetic beats code that argues with a scanner. Do not
	// add the +1 back — it re-raises two high-severity alerts on the security tab
	// to save an allocation nobody will ever measure (#2036).
	names := make([]string, 0, len(data.Tabs))
	seen := make(map[string]struct{}, len(data.Tabs))
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	add(data.TmuxName)
	for _, tab := range data.Tabs {
		add(tab.TmuxName)
	}
	// PENDING CLEANUP HANDLES too. These are tabs whose removal from Tabs is
	// already durable while their teardown was never confirmed (#2669), so they
	// are the names MOST likely to still have a live process — and they were the
	// only ones this did not enumerate. Omitting them meant kill deleted the
	// worktree around a session it had never attempted to stop (#3137).
	//
	// The ghostBlind guard does not cover the gap. CheckWorktreeOccupants runs
	// only when a tmux kill came back blind, so if every enumerated session dies
	// cleanly the guard never runs at all, and an un-enumerated handle keeps
	// writing into a directory git is deleting. Enumerating it is what makes the
	// kill attempt happen, and a blind result there is what arms the guard.
	for _, pending := range data.PendingTabCleanup {
		add(pending.TmuxName)
	}
	return names
}

// ghostCleanup runs best-effort teardown of a ghost session's external
// resources. Tmux teardown is independent of worktree state (#516/#549): a
// ghost record can have an empty worktree path while a tmux session with the
// persisted name is still running, so the two branches share no condition.
// Tmux goes FIRST: a still-running agent writing into the worktree while git
// recursively deletes it leaks a half-deleted directory (#802).
// It gates the worktree delete on tmux having ANSWERED, and returns an error the
// caller must not step over (#1917). This path is the ghost twin of teardownKill
// and had the identical defect the review caught there: it logged the tmux failure
// and cleaned the worktree regardless, so a wedged server meant deleting the
// workspace of a session that might still be running. Found by auditing every
// caller of the bounded teardown rather than by the review itself.
func ghostCleanup(
	data *session.InstanceData,
	title string,
	checkpoint func(*session.InstanceData) error,
) (error, <-chan error) {
	// Kill EVERY tmux session this ghost owns, not just the agent tab. The live
	// teardown path (Instance.teardownTabs) closes every tab's tmux; the ghost
	// path has no live Instance, so it must reconstruct the same set from the
	// persisted record. Before #2007 it killed only the legacy data.TmuxName (the
	// agent tab), so a multi-tab ghost's shell (`<agent>__shell`) and process
	// (`<agent>__btop`) tmux sessions leaked with no cleanup short of `af reset` —
	// the tab list persisted by #953 was never consulted here.
	//
	// The per-tab kills gate the worktree delete the same way the agent tab always
	// has (#1917): a tmux we could not confirm dead (state != Known) may still have
	// a pane writing into the workspace, so we stop before touching it and keep the
	// record intact for a retry. Tmux still goes FIRST for the #802 reason (a live
	// agent racing git's recursive delete leaks a half-deleted directory).
	ghostBlind := false
	for _, name := range ghostTmuxNames(data) {
		state, blind, killErr := ghostKillTmuxByName(name)
		ghostBlind = ghostBlind || blind
		if killErr != nil {
			log.WarningLog.Printf("ghost session %q: tmux cleanup failed for %q: %v", title, name, killErr)
		}
		if state != tmux.PaneStateKnown {
			return fmt.Errorf("ghost session %q: %w: leaving its workspace and record intact: %v",
				title, session.ErrPaneMayBeLive, killErr), nil
		}
	}
	// After every ghost tmux name is closed, and only if one of them was blind:
	// its marker evidence was vacuous, so a markerless process may still be inside
	// the workspace this is about to delete. Empty for an external record, matching
	// ghostCleanupWorktree's own bail — that path removes nothing, so there is no
	// destructive action to protect and the user's own checkout must not gate it.
	if ghostBlind && ghostWorktreeRemovable(data) {
		if err := session.CheckWorktreeOccupants(data.Worktree.WorktreePath); err != nil {
			return fmt.Errorf("ghost session %q: %w: leaving its workspace and record intact: %v",
				title, session.ErrPaneMayBeLive, err), nil
		}
	}
	state, cleanupErr, lateResult := ghostCleanupWorktree(data, title, checkpoint)
	if state != git.CleanupSettled {
		return fmt.Errorf("ghost session %q: %w: keeping its record so the cleanup can be retried: %v",
			title, session.ErrWorkspaceStateUnknown, cleanupErr), lateResult
	}
	return nil, nil
}
