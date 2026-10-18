package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// This file owns `af sessions prune` (#5136): the manual, opt-in reclaim
// command for archived sessions whose worktrees accumulate forever (the
// motivating fleet measured ~146G under <AF home>/archived). Slice 1 is the
// READ-ONLY half — the dry-run listing: it reports which archived sessions a
// future --apply would reclaim, what each would free, and why the ineligible
// rows refuse. af writes NOTHING: no deletion, no tombstone, no record
// update — every probe below is a bounded read (lstat, git status, git
// worktree list), and the af-writes-nothing property is proven by a test that
// fingerprints the AF home and the archive before and after a run. The apply
// half — deletion, tombstones, the confirmed-plan binding — lands in the
// follow-up issue filed from this PR's review.
//
// The eligibility rules are a pure function of the record (session.
// PruneSkipReason) plus read-only filesystem evidence (pruneFilesystemRefusal)
// so the listing cannot drift from what apply will refuse.

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
}

// PruneCandidate is one session the run lists as reclaimable.
type PruneCandidate struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// Branch is the work's durable handle: it is kept, and is the pointer a
	// future apply's restore refusal names back to it.
	Branch     string    `json:"branch"`
	ArchivedAt time.Time `json:"archived_at"`
	// ReclaimableBytes is ALLOCATED disk space (st_blocks×512), not apparent
	// file size — sparse holes are not counted, and a hard-linked inode is
	// credited only when deleting this tree removes its last link (#5136
	// Codex round 4). It is what `rm -rf` of the worktree would free — the
	// dry run reclaims nothing, so the field names what is reclaimABLE;
	// reclaimed_bytes belongs to the follow-up's apply (#5189).
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
}

// PruneSkippedEntry is one session the run evaluated and refused, with the
// reason — non-archived rows, in-flight operations, incomplete archive moves,
// too-recent archives, and rows the daemon could not materialize all land here
// rather than being silently filtered out.
type PruneSkippedEntry struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Reason string `json:"reason"`
}

// PruneSessionsResponse is the dry run's full accounting.
type PruneSessionsResponse struct {
	OK             bool      `json:"ok"`
	OlderThan      string    `json:"older_than"`
	ArchivedBefore time.Time `json:"archived_before"`
	// Candidates holds the rows a future --apply would reclaim. The key is
	// "candidates", not "pruned" — a dry run prunes nothing, and #5189's
	// apply wants "pruned" for rows it actually removed.
	Candidates []PruneCandidate `json:"candidates"`
	// Skipped holds rows refused BEFORE anything was deleted — they are
	// untouched. Re-running with a different --older-than or after the blocking
	// condition clears can admit them.
	Skipped          []PruneSkippedEntry `json:"skipped,omitempty"`
	ReclaimableBytes int64               `json:"reclaimable_bytes"`
	Warnings         []string            `json:"warnings,omitempty"`
}

// PruneSessions evaluates archived sessions against req and lists the ones
// whose worktree bytes are reclaimable. af writes nothing — the
// delete+tombstone apply lands in the follow-up issue referenced in the file
// header.
func (m *Manager) PruneSessions(req PruneSessionsRequest) (PruneSessionsResponse, error) {
	// Candidates is a required array on the wire: a nil slice marshals as
	// null and breaks clients that iterate without a null special case, so
	// the empty-result response must carry [] (#5136 Codex round 8).
	resp := PruneSessionsResponse{OK: true, Candidates: []PruneCandidate{}}
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

	candidates, skipped, warnings := m.pruneCandidates(req, resp.ArchivedBefore)
	resp.Skipped = skipped
	resp.Warnings = warnings
	resp.Candidates = candidates
	for _, entry := range candidates {
		resp.ReclaimableBytes += entry.ReclaimableBytes
	}
	return resp, nil
}

// prunePostProbeHook is a test seam: fired between the filesystem refusals
// and the publish-time state recheck so a test can interleave a lifecycle
// transition into the gap the recheck exists to close (#5136 Codex round 13).
// Nil in production.
var prunePostProbeHook func(*session.Instance)

// pruneCandidates enumerates every in-scope session row — the live map plus
// on-disk rows the daemon could not materialize — classifies each as
// eligible/skipped, and measures what the eligible ones would reclaim.
func (m *Manager) pruneCandidates(req PruneSessionsRequest, cutoff time.Time) ([]PruneCandidate, []PruneSkippedEntry, []string) {
	type candidateRow struct {
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
		rows = append(rows, candidateRow{repoID: repoID, instance: instance})
	}
	deleting := make(map[string]bool, len(m.projectDeletes))
	for repoID := range m.projectDeletes {
		if req.All || repoID == req.RepoID {
			deleting[repoID] = true
		}
	}
	// Sessions still provisioning live only here — the ghost sweep cannot
	// find an unpersisted row, so without this they would vanish from the
	// response despite the contract that every in-scope non-candidate is
	// accounted (#5136 Codex round 14).
	type pendingRow struct {
		repoID string
		title  string
	}
	var pending []pendingRow
	for key, p := range m.pendingCreates {
		repoID, _ := splitDaemonInstanceKey(key)
		if req.RepoID != "" && repoID != req.RepoID {
			continue
		}
		pending = append(pending, pendingRow{repoID: repoID, title: p.Title})
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

	for _, p := range pending {
		if deleting[p.repoID] {
			continue
		}
		skipped = append(skipped, PruneSkippedEntry{Title: p.title, RepoID: p.repoID,
			Reason: "session is still being created"})
	}

	// See PruneSessions: the slice must be non-nil so a zero-candidate run
	// marshals [] rather than null.
	candidates := []PruneCandidate{}
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
		_, busy := m.killsInFlight[daemonInstanceKey(row.repoID, data.Title)]
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
		worktreeBytes, sizeErr := session.DirSizeBytes(data.Worktree.WorktreePath)
		if sizeErr != nil {
			warnings = append(warnings, fmt.Sprintf("session %q: could not fully measure %s: %v", data.Title, data.Worktree.WorktreePath, sizeErr))
		}
		if prunePostProbeHook != nil {
			prunePostProbeHook(row.instance)
		}
		// Revalidate AFTER the filesystem probes AND the size walk: a
		// restore or kill can begin while either ran — the stale snapshot
		// still classifies archived, and the missing-path arm treats a
		// moved-away tree as a zero-byte candidate — so a session already
		// live again would publish (#5136 Codex round 13; the walk's 10s
		// window is part of the gap, round 14). Re-read the instance and
		// re-run every quiet-state gate rather than trusting the earlier
		// snapshot.
		fresh := row.instance.ToInstanceData()
		if reason := session.PruneSkipReason(fresh, cutoff); reason != "" {
			skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID,
				Reason: "session state changed while being evaluated: " + reason})
			continue
		}
		m.mu.Lock()
		_, busy = m.killsInFlight[daemonInstanceKey(row.repoID, data.Title)]
		captures = m.pendingConversationCaptures[row.instance]
		m.mu.Unlock()
		switch {
		case busy || captures > 0:
			skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID,
				Reason: "session state changed while being evaluated: an operation is already in progress"})
			continue
		case !row.instance.GetArchiveReport().Empty():
			skipped = append(skipped, PruneSkippedEntry{Title: data.Title, RepoID: row.repoID,
				Reason: "session state changed while being evaluated: incomplete archive"})
			continue
		}
		entry := PruneCandidate{
			ID:         data.ID,
			Title:      data.Title,
			RepoID:     row.repoID,
			Branch:     pruneBranchFor(data),
			ArchivedAt: session.ArchiveTimeFor(data),
		}
		entry.ReclaimableBytes = worktreeBytes
		candidates = append(candidates, entry)
	}
	sort.Slice(candidates, func(a, b int) bool {
		if candidates[a].RepoID != candidates[b].RepoID {
			return candidates[a].RepoID < candidates[b].RepoID
		}
		return candidates[a].Title < candidates[b].Title
	})
	sort.Slice(skipped, func(a, b int) bool {
		if skipped[a].RepoID != skipped[b].RepoID {
			return skipped[a].RepoID < skipped[b].RepoID
		}
		return skipped[a].Title < skipped[b].Title
	})
	return candidates, skipped, warnings
}

// pruneGhostRows reports in-scope on-disk rows that never materialized into
// m.instances. They are skipped entries, not candidates: a row the daemon
// cannot reconstruct is not safe to delete by its own say-so.
func (m *Manager) pruneGhostRows(req PruneSessionsRequest) ([]PruneSkippedEntry, []string) {
	var rows map[string]json.RawMessage
	var skips []config.RepoInstancesSkip
	if req.All {
		var err error
		// Silent form: inside the daemon the default loader's warning is a
		// log-file write in AF home, which the read-only run must not cause
		// — the skip detail already reaches the caller through warnings
		// (#5136 Codex round 12).
		rows, skips, err = config.LoadAllRepoInstancesReportingSkipDetailsSilent()
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
			reason := "record could not be materialized by the daemon — inspect it before reclaiming; files were left untouched"
			if liveness != session.LiveArchived {
				// Report, never drop: an unmaterialized non-archived row
				// still belongs in the skipped accounting — a silently
				// vanished record hides the repair the operator owes it
				// (#5136 Codex round 10).
				reason = fmt.Sprintf("record could not be materialized by the daemon and is not archived (status %d) — the row needs repair, not reclaim; files were left untouched", item.Status)
			}
			skipped = append(skipped, PruneSkippedEntry{
				Title:  item.Title,
				RepoID: repoID,
				Reason: reason,
			})
		}
	}
	return skipped, warnings
}

// pruneFilesystemRefusal runs the deletion-boundary refusals the record alone
// cannot express — worktree identity and uncommitted content — and returns ""
// only when the path is provably safe to reclaim. The listing runs it so the
// dry run previews exactly what the future apply will refuse: a foreign-path
// occupant or a dirty worktree shows as skipped instead of appearing
// reclaimable (#5136 review).
//
// Every probe is a bounded READ (BoundedLstat, the pointer-check flights, the
// bounded git runner): af writes nothing here, and a plain os.Stat on a
// stalled FUSE/NFS mount could wedge the scan behind the mount forever —
// bounded probes cap that at their flight timeout.
//
// A missing path needs no identity proof — there is nothing to reclaim, so
// the row lists as a zero-byte candidate rather than a refusal.
//
// A repo-gone row is REFUSED outright: with the origin deleted there is no
// reachable repository or branch to satisfy the kept-branch promise, so the
// archived directory may be the last surviving copy of the work — a future
// apply could not delete it while claiming the branch survives (#5136
// review). Restore the repository first, or remove the directory by hand
// once its contents are preserved elsewhere.
func pruneFilesystemRefusal(data session.InstanceData) string {
	repoPath := data.Worktree.RepoPath
	if repoPath == "" {
		repoPath = data.Path
	}
	wtPath := data.Worktree.WorktreePath
	// Probe the origin FIRST: a missing worktree must not skip the repo-gone
	// refusal — an out-of-band delete plus a gone origin would otherwise list
	// as a zero-byte candidate with a "kept" branch that no longer exists
	// (#5136 Codex round 10). The answer feeds both the early return and the
	// identity checks below.
	probeErr := sessiongit.CheckRepoPresentForRelocation(repoPath)
	info, lerr := sessiongit.BoundedLstat(wtPath)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			switch {
			case errors.Is(probeErr, sessiongit.ErrRepoGone):
				return fmt.Sprintf("origin repository %s is gone, so the branch prune promises to keep is gone with it — restore the repository first, or preserve the worktree contents and remove it manually", repoPath)
			case probeErr != nil:
				return fmt.Sprintf("could not establish whether repo %s is present: %v", repoPath, probeErr)
			}
			// Repo present but the tree is gone, so the recorded branch is
			// the ONLY recovery handle the candidate still promises — it
			// must resolve before the row lists as reclaimable (#5136 Codex
			// round 11). The probe's error is UNKNOWN, not absent
			// (#3243): refuse rather than guess either way.
			branch := pruneBranchFor(data)
			switch exists, berr := boundedLocalBranchExists(repoPath, branch); {
			case berr != nil:
				return fmt.Sprintf("could not verify recovery branch %s in %s: %v", branch, repoPath, berr)
			case !exists:
				return fmt.Sprintf("recovery branch %s no longer exists in %s — the record has no kept branch to promise", branch, repoPath)
			}
			return ""
		}
		return fmt.Sprintf("could not inspect %s before deletion: %v", wtPath, lerr)
	}
	// A symlink at the recorded path is an alias the record never wrote —
	// archive relocates the directory, it does not link it. Identity checks
	// resolve through the link, so a swapped-in alias to a live checkout
	// would pass them and list foreign content as reclaimable, while the
	// size walk measures the link rather than the tree it names (#5136
	// Codex round 14). Refuse rather than resolve.
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Sprintf("%s is a symlink — refusing to prove identity through an alias the record never wrote; restore the real archived directory or remove the link", wtPath)
	}

	switch {
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
		// `git worktree list` porcelain reports the worktree's branch
		// ATTRIBUTE, not that the ref resolves — `update-ref -d` leaves the
		// registration and the dirty probe clean while erasing the recovery
		// handle the candidate advertises (#5136 Codex round 12). The
		// probe's error is UNKNOWN, not absent (#3243): refuse rather than
		// guess either way.
		branch := pruneBranchFor(data)
		switch exists, berr := boundedLocalBranchExists(repoPath, branch); {
		case berr != nil:
			return fmt.Sprintf("could not verify recovery branch %s in %s: %v", branch, repoPath, berr)
		case !exists:
			return fmt.Sprintf("recovery branch %s no longer exists in %s — the record has no kept branch to promise", branch, repoPath)
		}
	case errors.Is(probeErr, sessiongit.ErrRepoGone):
		return fmt.Sprintf("origin repository %s is gone, so the branch prune promises to keep is gone with it — restore the repository first, or preserve the worktree contents and remove it manually", repoPath)
	default:
		return fmt.Sprintf("could not establish whether repo %s is present: %v", repoPath, probeErr)
	}

	// Uncommitted content in an archived worktree is the ONLY copy — archive
	// relocates bytes verbatim and snapshots only on the disposable-backends
	// path. IGNORED files count too: a gitignored .env exists nowhere else
	// and the kept branch cannot restore what it never tracked (#5136 Codex
	// round 5). The branch survives reclaim, but it has no such content, so
	// deleting here would silently destroy work the listing claims is
	// recoverable. Restore it or clean it first.
	dirty, err := sessiongit.WorktreeDirtyFiles(wtPath)
	if err != nil {
		return fmt.Sprintf("could not verify %s is clean before deletion: %v", wtPath, err)
	}
	if dirty > 0 {
		return fmt.Sprintf("worktree holds %d uncommitted or ignored file(s) that the kept branch does not contain — restore the session or clean the tree first", dirty)
	}
	return ""
}

// pruneBranchFor resolves the branch reclaim promises to keep. Branch is the
// record-level field; Worktree.BranchName is the worktree's own copy and the
// fallback for rows written before the top-level field.
func pruneBranchFor(data session.InstanceData) string {
	if data.Branch != "" {
		return data.Branch
	}
	return data.Worktree.BranchName
}

// boundedLocalBranchExists wraps sessiongit.LocalBranchExists — whose own
// stat/git probes are unbounded — in the same goroutine-flight concession
// DirSizeBytes makes for its walk (session/prune.go): a stalled origin must
// surface as a deadline to the listing, not an unbounded syscall. The parked
// worker is the same accepted leak the bounded walk carries.
func boundedLocalBranchExists(repoRoot, branch string) (bool, error) {
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := sessiongit.LocalBranchExists(repoRoot, branch)
		done <- result{ok, err}
	}()
	select {
	case r := <-done:
		return r.ok, r.err
	case <-time.After(pruneProbeDeadline):
		return false, fmt.Errorf("branch probe for %s in %s did not answer within %s", branch, repoRoot, pruneProbeDeadline)
	}
}

// pruneProbeDeadline bounds the filesystem/git probes the listing wraps
// itself (the branch check above). It matches the deadline session's own
// bounded helpers use — long enough for a healthy repo, short enough that a
// stalled mount fails the listing instead of hanging it.
const pruneProbeDeadline = 10 * time.Second

// PruneSessions is the control-socket/HTTP handler for the prune RPC. The
// run is a pure read — no admission gate beyond manager readiness, matching
// the other read-only RPCs.
func (s *controlServer) PruneSessions(req PruneSessionsRequest, resp *PruneSessionsResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	result, err := s.manager.PruneSessions(req)
	*resp = result
	return err
}
