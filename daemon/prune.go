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
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// This file owns `af sessions prune` (#5136): the manual, opt-in reclaim for
// archived sessions whose worktrees accumulate forever (the motivating fleet
// measured ~146G under <AF home>/archived). The CLI is a thin flag layer;
// everything destructive happens HERE, inside the daemon's existing lifecycle
// fences, so a prune can never race an archive, restore, kill, or reload into
// a half-deleted state.
//
// Prune deletes ONLY af-owned state: the archived worktree under <AF
// home>/archived plus the repo's stale `git worktree` registration. Provider
// homes are deliberately untouched — a session's Claude/Codex transcript
// files belong to the agent, not af, and can be live elsewhere (a carried
// conversation, a handoff, a manual `claude --resume`), so deleting them by
// the record's ids would be over-deletion af cannot prove safe.
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
//   4. Stamps pruned_at on the record and persists+publishes it — the
//      tombstone, which keeps the row listed with title/branch/archive-time.
//
// Order (3)→(4) is deliberate: the durable marker always lands AFTER the
// physical deletion it documents. A crash between them leaves an archived
// row whose files are gone — a state a re-run of the same command finds still
// eligible and finishes, since the deletion step is idempotent. The
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
	// Only, when non-empty, restricts the run to those session identities —
	// the confirmed-plan binding the CLI sends on a TTY-confirmed apply, so
	// the operator's yes covers exactly the rows the dry run showed and a
	// session that became eligible while the prompt was open cannot be
	// deleted unreviewed. Repo-qualified because titles collide across an
	// --all run. Absent/nil means unrestricted (the non-TTY apply, which
	// intentionally plans and applies in one step). An explicitly empty list
	// must never widen to unrestricted: apply rejects it outright, and the
	// CLI never sends it — a TTY-confirmed empty plan returns without
	// applying. The control socket cannot distinguish nil from empty (gob
	// collapses them), so the apply-time rejection is what fails closed on
	// transports that can carry the distinction.
	Only []PrunePlanRef `json:"only,omitempty"`
}

// PrunePlanRef names one session the operator confirmed for pruning. ID is
// the stable session identity the dry-run plan carried: a row removed while
// the TTY prompt sits open can be REPLACED by a same-title session — which
// the repo+title key alone would match, pruning a session the operator never
// saw (#5136 review). An empty ID (a client that predates the field) falls
// back to the repo+title binding.
type PrunePlanRef struct {
	RepoID string `json:"repo_id"`
	Title  string `json:"title"`
	ID     string `json:"id,omitempty"`
}

// PrunedSessionEntry is one session the run pruned (apply) or would prune
// (dry run).
type PrunedSessionEntry struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// Branch is the work's durable handle: it is kept, and is the restore
	// refusal's pointer back to it.
	Branch     string    `json:"branch"`
	ArchivedAt time.Time `json:"archived_at"`
	// ReclaimedBytes is ALLOCATED disk space (st_blocks×512), not apparent
	// file size — sparse holes are not counted, and a hard-linked inode is
	// credited only when deleting this tree removes its last link (#5136
	// Codex round 4). It is what `rm -rf` of the worktree would free.
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
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
	if req.RepoID != "" && req.All {
		return resp, fmt.Errorf("repo_id and all are mutually exclusive — the same scopes the CLI refuses to combine")
	}
	if req.RepoID == "" && !req.All {
		return resp, fmt.Errorf("a scope is required: pass repo_id for one project or all=true for every project")
	}
	if req.Apply && req.Only != nil && len(req.Only) == 0 {
		// An explicitly empty confirmed set means "the operator confirmed
		// nothing", not "no restriction" — widening it would let a "Prune 0"
		// answer delete whatever became eligible since the dry run (#5136
		// review). Omit 'only' entirely for an unrestricted apply.
		return resp, fmt.Errorf("an explicitly empty only list applies to no sessions — omit 'only' for an unrestricted apply")
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
	// An incomplete row means deletion STARTED but could not be confirmed
	// finished — the response must not report success for that (#5136 review):
	// the structured Incomplete list tells the operator what needs attention,
	// and ok=false lets the CLI exit non-zero so automation sees it.
	resp.OK = len(resp.Incomplete) == 0
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

	var confirmed map[string]PrunePlanRef
	if req.Only != nil {
		// Non-nil is the confirmed-plan contract: exactly the named rows are
		// in scope — an EMPTY set scopes to nothing rather than widening to
		// everything, and apply rejects it at validation above.
		confirmed = make(map[string]PrunePlanRef, len(req.Only))
		for _, ref := range req.Only {
			confirmed[daemonInstanceKey(ref.RepoID, ref.Title)] = ref
		}
	}

	var candidates []pruneCandidate
	for _, row := range rows {
		if deleting[row.repoID] {
			continue
		}
		data := row.instance.ToInstanceData()
		if confirmed != nil {
			ref, named := confirmed[daemonInstanceKey(row.repoID, data.Title)]
			if !named {
				// Outside the confirmed set: not reported at all — the plan the
				// operator answered named it, so this run treats it as out of
				// scope rather than as a refusal needing a reason.
				continue
			}
			if ref.ID != "" && ref.ID != data.ID {
				// Same repo+title but a different session: the confirmed row
				// was removed and a replacement archived since the plan. The
				// operator never saw this one — report it rather than prune.
				skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID,
					Reason: "the confirmed session was replaced by a different session under the same title"})
				continue
			}
		}
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
		// The plan must read like the apply it previews (#5136 review): the
		// same deletion-boundary refusals run here, so a foreign-path occupant
		// or a dirty worktree shows as skipped in the dry run instead of
		// appearing prunable and refusing only after --apply.
		if reason := pruneFilesystemRefusal(data); reason != "" {
			skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID, Reason: reason})
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
		entry.ReclaimedBytes = worktreeBytes
		candidates = append(candidates, pruneCandidate{
			key: row.key, repoID: row.repoID, instance: row.instance, entry: entry,
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

	// Re-verify identity AND claim in ONE m.mu section: a create reusing this
	// archived title could otherwise slip between the checks — it holds m.mu,
	// sees no killsInFlight, renames/rekeys the row and relocates its worktree
	// — and prune would then act on the pre-rename path and stamp a tombstone
	// on the renamed row.
	m.mu.Lock()
	current := m.instances[key]
	if current != instance {
		m.mu.Unlock()
		return warnings, false, fmt.Errorf("session %q changed state before prune could start", data.Title)
	}
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

	// Re-verify eligibility against the CURRENT record under the claim: the
	// scan's candidate may have moved while the op-lock wait above blocked.
	live := instance.ToInstanceData()
	if reason := session.PruneSkipReason(live, cutoff); reason != "" {
		return warnings, false, fmt.Errorf("no longer eligible: %s", reason)
	}

	// (1) The archived worktree. RemoveWorktreeDir owns the ownership gate, the
	// bounded git calls, the `git worktree prune` that clears the repo's stale
	// registration, and the deregistration verify. It deletes NO branch. Its
	// error means the directory's state is unknown — partial by definition.
	repoPath := live.Worktree.RepoPath
	if repoPath == "" {
		repoPath = live.Path
	}
	wtPath := live.Worktree.WorktreePath

	// Re-run the deletion-boundary refusals under the claim — the same checks
	// the scan ran, now authoritative against the current record in the
	// window the op-lock wait and claim opened: the path's occupant must
	// still verify as THIS session's worktree (not merely a linked worktree
	// of the right repo — a same-repo replacement parked at a recycled path
	// is another session's checkout), and it must carry no uncommitted work,
	// which the tombstone's kept-branch promise does not cover.
	if reason := pruneFilesystemRefusal(live); reason != "" {
		return warnings, false, errors.New(reason)
	}

	if _, err := sessiongit.RemoveWorktreeDir(repoPath, wtPath); err != nil {
		return warnings, true, fmt.Errorf("removing archived worktree %s: %w", wtPath, err)
	}

	// (2) The tombstone, persisted and announced in the repo-ordered critical
	// section — the point of no return for the RECORD this time. Files are
	// already gone, so a failure here is reported loudly: the row stays
	// eligible and a re-run finishes the marker (every deletion above is
	// idempotent), which is exactly what makes delete-then-stamp safe.
	prevUpdatedAt := live.UpdatedAt
	instance.MarkPruned(time.Now())
	if err := m.persistAndPublishInstanceErr(cand.repoID, instance); err != nil {
		// The durable tombstone did not land, so the in-memory marker must not
		// claim it did: roll it back or the retry this error prescribes would
		// skip the row as "already pruned" and leave the marker unwritten
		// forever. Restoring updated_at keeps the archive-time fallback honest
		// for pre-upgrade rows, and the re-published projection puts the false
		// tombstone (and its suppressed restore affordance) back for clients
		// that already rendered the failed write's event.
		instance.UnmarkPruned(prevUpdatedAt)
		m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
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

// pruneFilesystemRefusal runs the deletion-boundary refusals the record alone
// cannot express — worktree identity and uncommitted content — and returns ""
// only when the path is provably safe for rm -rf. The scan calls it so the
// plan reads like the apply it previews; pruneOneSession re-runs it under the
// operation lock and killsInFlight claim, where its answer is authoritative.
//
// Every probe is bounded (BoundedLstat, the pointer-check flights, the bounded
// git runner): the apply caller holds the per-session operation lock, and a
// plain os.Stat on a stalled FUSE/NFS mount would wedge the lock, the claim,
// and every later candidate forever (#5136 review).
//
// A missing path needs no identity proof — there is nothing to delete, and
// RemoveWorktreeDir still clears the stale registration.
//
// A repo-gone row is REFUSED outright: with the origin deleted there is no
// reachable repository or branch to satisfy the tombstone's kept-branch
// promise, so the archived directory may be the last surviving copy of the
// work — prune cannot delete it while claiming the branch survives (#5136
// review). Restore the repository first, or remove the directory by hand
// once its contents are preserved elsewhere.
func pruneFilesystemRefusal(data session.InstanceData) string {
	repoPath := data.Worktree.RepoPath
	if repoPath == "" {
		repoPath = data.Path
	}
	wtPath := data.Worktree.WorktreePath
	if _, err := sessiongit.BoundedLstat(wtPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		return fmt.Sprintf("could not inspect %s before deletion: %v", wtPath, err)
	}

	switch probeErr := sessiongit.CheckRepoPresentForRelocation(repoPath); {
	case probeErr == nil:
		// Repo-present: the same bidirectional evidence the kill path requires
		// (#3278), PLUS the occupant's registered branch — the pointer alone
		// proves only same-repo membership, and a different session's worktree
		// parked at this recycled path would pass it (#5136 review).
		if err := sessiongit.VerifyRegisteredWorktreeOccupantBranch(wtPath, repoPath, pruneBranchFor(data)); err != nil {
			return fmt.Sprintf("%s could not be verified as this session's worktree: %v", wtPath, err)
		}
		// Repo+branch+path are still REUSABLE spellings: after the original
		// archive's removal, `git worktree add <path> <branch>` recreates all
		// three. The registration leaf's age is the evidence a re-create
		// cannot fake — it is always younger than the archive it pretends
		// to precede (#5136 Codex round 4).
		if err := sessiongit.VerifyWorktreeRegistrationPredates(wtPath, session.ArchiveTimeFor(data)); err != nil {
			return fmt.Sprintf("%s could not be verified as this session's worktree: %v", wtPath, err)
		}
	case errors.Is(probeErr, sessiongit.ErrRepoGone):
		return fmt.Sprintf("origin repository %s is gone, so the branch prune promises to keep is gone with it — restore the repository first, or preserve the worktree contents and remove it manually", repoPath)
	default:
		return fmt.Sprintf("could not establish whether repo %s is present: %v", repoPath, probeErr)
	}

	// Uncommitted content in an archived worktree is the ONLY copy — archive
	// relocates bytes verbatim and snapshots only on the disposable-backends
	// path. The tombstone promises the branch survives, but the branch has no
	// such content, so deleting here would silently destroy work the refusal
	// claims is recoverable. Restore it or clean it first.
	dirty, err := sessiongit.WorktreeDirtyFiles(wtPath)
	if err != nil {
		return fmt.Sprintf("could not verify %s is clean before deletion: %v", wtPath, err)
	}
	if dirty > 0 {
		return fmt.Sprintf("worktree holds %d uncommitted file(s) that the kept branch does not contain — restore the session or clean the tree first", dirty)
	}
	return ""
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
