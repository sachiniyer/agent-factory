package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// This file owns `af sessions prune` (#5136): the manual, opt-in reclaim for
// archived sessions whose worktrees and provider captures accumulate forever
// (the motivating fleet measured ~146G under <AF home>/archived). The CLI is a
// thin flag layer; everything destructive happens HERE, inside the daemon's
// existing lifecycle fences, so a prune can never race an archive, restore,
// kill, or reload into a half-deleted state.
//
// What apply does to an eligible row, in order and per session:
//
//   1. Takes the per-session operation lock (bounded, same lockWithin path
//      archive and restore use), then claims killsInFlight — the same claim
//      that makes Kill/restore/DeleteProject refuse while the row mutates.
//   2. Re-verifies eligibility against the CURRENT record, because the wait
//      in (1) is exactly the window a concurrent lifecycle op had to move it.
//   3. Deletes the archived worktree via sessiongit.RemoveWorktreeDir — the
//      audited, bounded removal that prunes the repo's stale `git worktree`
//      registrations and verifies deregistration afterwards. The BRANCH is
//      deliberately untouched: it is the only thing the tombstone promises.
//   4. Deletes the session's provider transcript/capture files, matched by
//      the conversation ids the record carries (never by path guessing).
//   5. Stamps pruned_at on the record and persists+publishes it — the
//      tombstone, which keeps the row listed with title/branch/archive-time.
//
// Order (3)→(4)→(5) is deliberate: the durable marker always lands AFTER the
// physical deletions it documents. A crash between them leaves an archived
// row whose files are gone — a state a re-run of the same command finds still
// eligible and finishes, since every deletion step is idempotent. The
// inverse ordering (tombstone first) would leave files stranded behind a
// "already pruned" skip with no restore path.

// PruneSessionsRequest is the control-RPC body for `af sessions prune`.
type PruneSessionsRequest struct {
	// RepoID scopes the run to one project. Empty only when All is set.
	RepoID string `json:"repo_id,omitempty"`
	// All selects every project the daemon holds records for.
	All bool `json:"all,omitempty"`
	// OlderThan is a Go duration ("720h", "24h") measured from each session's
	// archive time — never from arbitrary record mutation. Required; prune has
	// no default retention because the owner chose an opt-in command, not a
	// policy (#5136).
	OlderThan string `json:"older_than"`
	// Apply performs the deletion. False is the dry run: the same candidates
	// are listed with the bytes they would reclaim and nothing is changed.
	Apply bool `json:"apply,omitempty"`
}

// PrunedSessionEntry is one session the run pruned (apply) or would prune
// (dry run).
type PrunedSessionEntry struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// Branch is the work's durable handle: it is kept, and is the restore
	// refusal's pointer back to it.
	Branch         string    `json:"branch"`
	ArchivedAt     time.Time `json:"archived_at"`
	ReclaimedBytes int64     `json:"reclaimed_bytes"`
	// PrunedAt is stamped only on the apply path.
	PrunedAt time.Time `json:"pruned_at,omitzero"`
}

// PruneSkippedEntry is one session the run evaluated and refused, with the
// reason — non-archived rows, in-flight operations, incomplete archive moves,
// too-recent archives, already-pruned tombstones, and rows the daemon could
// not materialize all land here rather than being silently filtered out.
type PruneSkippedEntry struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Reason string `json:"reason"`
}

// PruneSessionsResponse is the prune run's full accounting.
type PruneSessionsResponse struct {
	OK             bool      `json:"ok"`
	Applied        bool      `json:"applied"`
	OlderThan      string    `json:"older_than"`
	ArchivedBefore time.Time `json:"archived_before"`
	// Pruned holds the would-prune (dry run) or actually-pruned (apply) rows.
	Pruned []PrunedSessionEntry `json:"pruned"`
	// Skipped holds rows refused BEFORE anything was deleted — they are
	// untouched. Re-running with a different --older-than or after the blocking
	// condition clears can admit them.
	Skipped []PruneSkippedEntry `json:"skipped,omitempty"`
	// Incomplete holds rows whose deletion STARTED but could not be confirmed
	// finished — a worktree git refused to release, or the tombstone write that
	// failed after the files were gone. These MUST be distinguished from
	// Skipped: they are partially applied and need operator attention or a
	// re-run, not a future cutoff.
	Incomplete     []PruneSkippedEntry `json:"incomplete,omitempty"`
	ReclaimedBytes int64               `json:"reclaimed_bytes"`
	Warnings       []string            `json:"warnings,omitempty"`
	MutationOutcome
}

// record folds a top-level error into the mutation outcome, the same
// convention the sibling lifecycle responses use.
func (r *PruneSessionsResponse) record(err error) bool {
	return r.MutationOutcome.record(err)
}

// pruneCandidate is one eligible session plus what deleting it reclaims.
type pruneCandidate struct {
	key      string
	repoID   string
	instance *session.Instance
	entry    PrunedSessionEntry
	// convFiles is resolved at scan time so the dry-run size and the apply
	// deletion agree on the same file set.
	convFiles session.PruneConversationFiles
}

// PruneSessions evaluates archived sessions against req and, when Apply is
// set, deletes the eligible ones. See the file header for the apply order.
func (m *Manager) PruneSessions(req PruneSessionsRequest) (PruneSessionsResponse, error) {
	resp := PruneSessionsResponse{Applied: req.Apply}
	olderThan := strings.TrimSpace(req.OlderThan)
	if olderThan == "" {
		return resp, fmt.Errorf("older_than is required — prune is deliberately opt-in and never guesses a retention period")
	}
	duration, err := time.ParseDuration(olderThan)
	if err != nil || duration <= 0 {
		return resp, fmt.Errorf("older_than %q is not a positive Go duration (for example \"720h\" for thirty days)", req.OlderThan)
	}
	resp.OlderThan = olderThan
	resp.ArchivedBefore = time.Now().Add(-duration)
	if req.RepoID == "" && !req.All {
		return resp, fmt.Errorf("a scope is required: pass repo_id for one project or all=true for every project")
	}

	candidates, skipped, warnings := m.pruneCandidates(req, resp.ArchivedBefore)
	resp.Skipped = skipped
	resp.Warnings = warnings

	for _, cand := range candidates {
		entry := cand.entry
		if req.Apply {
			warns, partial, pruneErr := m.pruneOneSession(cand, resp.ArchivedBefore)
			resp.Warnings = append(resp.Warnings, warns...)
			if pruneErr != nil {
				bucket := &resp.Skipped
				if partial {
					bucket = &resp.Incomplete
				}
				*bucket = append(*bucket, PruneSkippedEntry{
					Title: entry.Title, RepoID: entry.RepoID, Reason: pruneErr.Error()})
				continue
			}
			entry.PrunedAt = time.Now()
		}
		resp.Pruned = append(resp.Pruned, entry)
		resp.ReclaimedBytes += entry.ReclaimedBytes
	}
	resp.OK = true
	return resp, nil
}

// pruneCandidates enumerates every in-scope session row — the live map plus
// on-disk rows the daemon could not materialize — classifies each as
// eligible/skipped, and measures what the eligible ones would reclaim.
func (m *Manager) pruneCandidates(req PruneSessionsRequest, cutoff time.Time) ([]pruneCandidate, []PruneSkippedEntry, []string) {
	type candidateRow struct {
		key      string
		repoID   string
		instance *session.Instance
	}
	var rows []candidateRow
	m.mu.Lock()
	for key, instance := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		if req.RepoID != "" && repoID != req.RepoID {
			continue
		}
		rows = append(rows, candidateRow{key: key, repoID: repoID, instance: instance})
	}
	deleting := make(map[string]bool, len(m.projectDeletes))
	for repoID := range m.projectDeletes {
		if req.All || repoID == req.RepoID {
			deleting[repoID] = true
		}
	}
	m.mu.Unlock()

	var skipped []PruneSkippedEntry
	var warnings []string

	// A repo mid-delete is skipped wholesale: its rows are being archived and
	// deregistered underneath us, and the admission-order reasoning in
	// claimRestoreOperation applies here unchanged.
	for repoID := range deleting {
		warnings = append(warnings, fmt.Sprintf("project %s is being deleted; its sessions were skipped", repoID))
	}

	// On-disk rows the daemon could not materialize are reported, never
	// pruned: a record that fails to load cannot be trusted to name the files
	// deletion should remove, and the fences that would have made its claim
	// safe only exist for materialized sessions.
	ghosts, ghostWarns := m.pruneGhostRows(req)
	warnings = append(warnings, ghostWarns...)
	skipped = append(skipped, ghosts...)

	afHome, err := config.GetConfigDir()
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("cannot resolve the agent-factory home for transcript cleanup: %v", err))
	}
	fileIndex := session.NewPruneFileIndex(afHome)

	var candidates []pruneCandidate
	for _, row := range rows {
		if deleting[row.repoID] {
			continue
		}
		data := row.instance.ToInstanceData()
		if reason := session.PruneSkipReason(data, cutoff); reason != "" {
			skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID, Reason: reason})
			continue
		}
		m.mu.Lock()
		_, busy := m.killsInFlight[row.key]
		captures := m.pendingConversationCaptures[row.instance]
		m.mu.Unlock()
		if busy {
			skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID,
				Reason: "an operation is already in progress for this session"})
			continue
		}
		if captures > 0 {
			skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID,
				Reason: "a provider conversation capture is still in flight"})
			continue
		}
		if report := row.instance.GetArchiveReport(); !report.Empty() {
			// The live worktree carries an incomplete-archive report the
			// serialized row only stages: same refusal either way, read from
			// the authoritative handle.
			skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID,
				Reason: "incomplete archive — retained source trees are present and need manual review"})
			continue
		}
		entry := PrunedSessionEntry{
			ID:         data.ID,
			Title:      data.Title,
			RepoID:     row.repoID,
			Branch:     pruneBranchFor(data),
			ArchivedAt: session.ArchiveTimeFor(data),
		}
		worktreeBytes, sizeErr := session.DirSizeBytes(data.Worktree.WorktreePath)
		if sizeErr != nil {
			warnings = append(warnings, fmt.Sprintf("session %q: could not fully measure %s: %v", data.Title, data.Worktree.WorktreePath, sizeErr))
		}
		convFiles, convWarns := fileIndex.FilesFor(data)
		warnings = append(warnings, convWarns...)
		convBytes := convFiles.Bytes
		for _, dir := range convFiles.Dirs {
			dirBytes, dirErr := session.DirSizeBytes(dir)
			if dirErr != nil {
				warnings = append(warnings, fmt.Sprintf("session %q: could not fully measure %s: %v", data.Title, dir, dirErr))
			}
			convBytes += dirBytes
		}
		entry.ReclaimedBytes = worktreeBytes + convBytes
		candidates = append(candidates, pruneCandidate{
			key: row.key, repoID: row.repoID, instance: row.instance,
			entry: entry, convFiles: convFiles,
		})
	}
	sort.Slice(candidates, func(a, b int) bool {
		if candidates[a].entry.RepoID != candidates[b].entry.RepoID {
			return candidates[a].entry.RepoID < candidates[b].entry.RepoID
		}
		return candidates[a].entry.Title < candidates[b].entry.Title
	})
	sort.Slice(skipped, func(a, b int) bool {
		if skipped[a].RepoID != skipped[b].RepoID {
			return skipped[a].RepoID < skipped[b].RepoID
		}
		return skipped[a].Title < skipped[b].Title
	})
	return candidates, skipped, warnings
}

// pruneOneSession applies the delete+tombstone sequence to one candidate
// under the per-session operation lock and the killsInFlight claim. partial
// reports whether deletion had already started when the returned error
// happened — false means nothing was touched (the caller's Skipped bucket),
// true means the row is in an unknown or partially-deleted state that needs
// attention or a re-run (Incomplete).
func (m *Manager) pruneOneSession(cand pruneCandidate, cutoff time.Time) ([]string, bool, error) {
	instance := cand.instance
	data := cand.entry
	key := cand.key
	var warnings []string

	// Op-lock BEFORE the claim — the #3600/#3715 order every exclusive
	// lifecycle op uses: the wait then holds nothing, so the row stays honestly
	// unclaimed rather than advertising a Kill that can only refuse.
	opLock, _, err := m.lockSessionOperationWithin(key, "prune", data.Title)
	if err != nil {
		return warnings, false, err
	}
	defer opLock.Unlock()

	// Re-verify under the op-lock: the row the scan classified may have moved
	// while this waited.
	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance {
		return warnings, false, fmt.Errorf("session %q changed state before prune could start", data.Title)
	}
	live := instance.ToInstanceData()
	if reason := session.PruneSkipReason(live, cutoff); reason != "" {
		return warnings, false, fmt.Errorf("no longer eligible: %s", reason)
	}

	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return warnings, false, fmt.Errorf("an operation is already in progress for session %q", data.Title)
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	// (1) The archived worktree. RemoveWorktreeDir owns the ownership gate, the
	// bounded git calls, the `git worktree prune` that clears the repo's stale
	// registration, and the deregistration verify. It deletes NO branch. Its
	// error means the directory's state is unknown — partial by definition.
	repoPath := live.Worktree.RepoPath
	if repoPath == "" {
		repoPath = live.Path
	}
	if _, err := sessiongit.RemoveWorktreeDir(repoPath, live.Worktree.WorktreePath); err != nil {
		return warnings, true, fmt.Errorf("removing archived worktree %s: %w", live.Worktree.WorktreePath, err)
	}

	// (2) The provider transcript/capture files, resolved at scan time from
	// the record's own conversation ids.
	warnings = append(warnings, session.DeleteConversationFiles(cand.convFiles)...)

	// (3) The tombstone, persisted and announced in the repo-ordered critical
	// section — the point of no return for the RECORD this time. Files are
	// already gone, so a failure here is reported loudly: the row stays
	// eligible and a re-run finishes the marker (every deletion above is
	// idempotent), which is exactly what makes delete-then-stamp safe.
	instance.MarkPruned(time.Now())
	if err := m.persistAndPublishInstanceErr(cand.repoID, instance); err != nil {
		// The durable tombstone did not land, so the in-memory marker must not
		// claim it did: roll it back or the retry this error prescribes would
		// skip the row as "already pruned" and leave the marker unwritten
		// forever.
		instance.MarkPruned(time.Time{})
		return warnings, true, fmt.Errorf("files were deleted but the pruned marker could not be persisted; re-run `af sessions prune --apply` to finish: %w", err)
	}
	return warnings, false, nil
}

// pruneGhostRows reports in-scope on-disk rows that never materialized into
// m.instances. They are skipped entries, not candidates: a row the daemon
// cannot reconstruct is not safe to delete by its own say-so.
func (m *Manager) pruneGhostRows(req PruneSessionsRequest) ([]PruneSkippedEntry, []string) {
	var rows map[string]json.RawMessage
	var skips []config.RepoInstancesSkip
	if req.All {
		var err error
		rows, skips, err = config.LoadAllRepoInstancesReportingSkipDetails()
		if err != nil {
			return nil, []string{fmt.Sprintf("could not enumerate repo session records for ghost detection: %v", err)}
		}
	} else {
		raw, err := config.LoadRepoInstances(req.RepoID)
		if err != nil {
			return nil, []string{fmt.Sprintf("could not read session records for repo %s: %v", req.RepoID, err)}
		}
		rows = map[string]json.RawMessage{req.RepoID: raw}
	}
	var warnings []string
	for _, skip := range skips {
		warnings = append(warnings, fmt.Sprintf("repo %s skipped: its session records could not be read (%s)", skip.RepoID, skip))
	}
	var skipped []PruneSkippedEntry
	for repoID, raw := range rows {
		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			warnings = append(warnings, fmt.Sprintf("repo %s skipped: its session records are corrupted (%v)", repoID, err))
			continue
		}
		for _, item := range data {
			key := daemonInstanceKey(repoID, item.Title)
			m.mu.Lock()
			_, live := m.instances[key]
			m.mu.Unlock()
			if live {
				continue
			}
			liveness := item.Liveness
			if liveness == session.LivenessUnset {
				liveness = session.LivenessForStatus(item.Status)
			}
			if liveness != session.LiveArchived {
				continue
			}
			skipped = append(skipped, PruneSkippedEntry{
				Title:  item.Title,
				RepoID: repoID,
				Reason: "record could not be materialized by the daemon — inspect it before reclaiming; files were left untouched",
			})
		}
	}
	return skipped, warnings
}

// pruneBranchFor resolves the branch the tombstone promises to keep. Branch is
// the record-level field; Worktree.BranchName is the worktree's own copy and
// the fallback for rows written before the top-level field.
func pruneBranchFor(data session.InstanceData) string {
	if data.Branch != "" {
		return data.Branch
	}
	return data.Worktree.BranchName
}

// PruneSessions is the control-socket/HTTP handler for the prune RPC.
func (s *controlServer) PruneSessions(req PruneSessionsRequest, resp *PruneSessionsResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	// Apply is a state mutation and sits behind the same admission gate as the
	// other lifecycle verbs; the dry run is a read and only needs the manager.
	if req.Apply {
		if err := s.requireStateMutationAdmission(); err != nil {
			return err
		}
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	result, err := s.manager.PruneSessions(req)
	*resp = result
	if !resp.record(err) {
		return err
	}
	return nil
}
