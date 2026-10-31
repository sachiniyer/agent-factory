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
// motivating fleet measured ~146G under <AF home>/archived). The dry-run
// listing reports which archived sessions an --apply would reclaim, what
// each would free, and why the ineligible rows refuse. It writes NOTHING:
// no deletion, no tombstone, no record update — every probe below is a
// bounded read (lstat, git status, git worktree list), and the
// af-writes-nothing property is proven by a test that fingerprints the AF
// home and the archive before and after a run. The apply half — deletion,
// tombstones, the confirmed-plan binding — landed in #5189 and lives in
// prune_apply.go; PruneSessions dispatches to it when req.Apply is set.
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
	// Apply deletes what the verdict lists (#5189). Without it the call is the
	// strictly read-only dry run.
	Apply bool `json:"apply,omitempty"`
	// Only binds an apply to the plan the user confirmed: session IDs the dry run
	// listed. Apply acts on Only ∩ what the verdict still passes; a row outside it
	// is never touched, and a planned row that no longer passes is reported as
	// skipped. Empty means "plan and apply in one run".
	Only []string `json:"only,omitempty"`
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
	// the apply (#5189) reports its estimate as reclaimed_estimate_bytes.
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	// BranchPushed is information, not a gate: the row records whether af ever
	// saw the branch pushed. The kept local branch is the preservation, and it
	// holds every commit of the worktree (HEAD == branch tip is a gate).
	BranchPushed bool `json:"branch_pushed"`
	// head is the object id the verdict proved equal to the branch tip; it goes
	// into the durable intent.
	head string
}

// PruneSkippedEntry is one session the run evaluated and refused, with the
// reason — non-archived rows, in-flight operations, incomplete archive moves,
// too-recent archives, and rows the daemon could not materialize all land here
// rather than being silently filtered out.
type PruneSkippedEntry struct {
	ID string `json:"id,omitempty"`
	// Interrupted marks a row whose earlier prune did not finish: an apply
	// completes it from its durable intent, so it belongs in the plan.
	Interrupted bool   `json:"interrupted,omitempty"`
	Title       string `json:"title"`
	RepoID      string `json:"repo_id"`
	Reason      string `json:"reason"`
}

// PruneSessionsResponse is the dry run's full accounting.
type PruneSessionsResponse struct {
	OK             bool      `json:"ok"`
	OlderThan      string    `json:"older_than"`
	ArchivedBefore time.Time `json:"archived_before"`
	// Candidates holds the rows an --apply would reclaim. The key is
	// "candidates", not "pruned" — a dry run prunes nothing, and the apply
	// (#5189) uses "pruned" for rows it actually removed.
	Candidates []PruneCandidate `json:"candidates"`
	// Skipped holds rows refused BEFORE anything was deleted — they are
	// untouched. Re-running with a different --older-than or after the blocking
	// condition clears can admit them.
	Skipped          []PruneSkippedEntry `json:"skipped,omitempty"`
	ReclaimableBytes int64               `json:"reclaimable_bytes"`
	Warnings         []string            `json:"warnings,omitempty"`

	// The fields below are set only by --apply (#5189).
	//
	// Applied is true when this was an apply run (so empty Pruned means "nothing
	// was deleted", not "this was a dry run").
	Applied bool `json:"applied,omitempty"`
	// Pruned holds rows whose worktree was deleted and which are now tombstones.
	Pruned []PrunedEntry `json:"pruned,omitempty"`
	// Incomplete holds rows whose deletion started but whose record is not final;
	// distinct from Skipped, which is untouched. Re-running completes them.
	Incomplete []PruneIncompleteEntry `json:"incomplete,omitempty"`
	// ReclaimedEstimateBytes is "up to": the sum of the allocated-size estimates
	// of the pruned rows. It is never presented as freed space.
	ReclaimedEstimateBytes int64 `json:"reclaimed_estimate_bytes,omitempty"`
	// FreeBytes* are the MEASURED free space on the archive filesystem(s) before
	// and after, so the gap to the estimate (reflink clones, shared extents) is
	// visible rather than silently credited.
	FreeBytesBefore uint64 `json:"free_bytes_before,omitempty"`
	FreeBytesAfter  uint64 `json:"free_bytes_after,omitempty"`
	FreeBytesKnown  bool   `json:"free_bytes_known,omitempty"`
}

// PruneSessions evaluates archived sessions against req and lists the ones
// whose worktree bytes are reclaimable. Without req.Apply it writes nothing;
// with it, pruneApply deletes what the verdict lists (#5189).
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

	if req.Apply {
		return m.pruneApply(req, resp)
	}
	if len(req.Only) > 0 {
		return resp, fmt.Errorf("only binds an --apply to a confirmed plan; it has no meaning on a dry run")
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
		entry, skip, warns := m.pruneVerdict(row.repoID, row.instance, cutoff, false)
		warnings = append(warnings, warns...)
		if skip != nil {
			skipped = append(skipped, *skip)
			continue
		}
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
		type loaded struct {
			rows  map[string]json.RawMessage
			skips []config.RepoInstancesSkip
			err   error
		}
		l := boundedRead(func() loaded {
			r, sk, e := config.LoadAllRepoInstancesReportingSkipDetailsSilent()
			return loaded{r, sk, e}
		}, loaded{err: fmt.Errorf("reading the session records did not answer within %s", pruneProbeDeadline)})
		rows, skips, err = l.rows, l.skips, l.err
		if err != nil {
			return nil, []string{fmt.Sprintf("could not enumerate repo session records for ghost detection: %v", err)}
		}
	} else {
		type loaded struct {
			raw json.RawMessage
			err error
		}
		l := boundedRead(func() loaded {
			r, e := config.LoadRepoInstances(req.RepoID)
			return loaded{r, e}
		}, loaded{err: fmt.Errorf("reading the session records did not answer within %s", pruneProbeDeadline)})
		raw, err := l.raw, l.err
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
func pruneFilesystemRefusal(data session.InstanceData) (head, reason string) {
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
				return "", fmt.Sprintf("origin repository %s is gone, so the branch prune promises to keep is gone with it — restore the repository first, or preserve the worktree contents and remove it manually", repoPath)
			case probeErr != nil:
				return "", fmt.Sprintf("could not establish whether repo %s is present: %v", repoPath, probeErr)
			}
			// Repo present but the tree is gone, so the recorded branch is
			// the ONLY recovery handle the candidate still promises — it
			// must resolve before the row lists as reclaimable (#5136 Codex
			// round 11). The probe's error is UNKNOWN, not absent
			// (#3243): refuse rather than guess either way.
			branch := pruneBranchFor(data)
			switch exists, berr := boundedLocalBranchExists(repoPath, branch); {
			case berr != nil:
				return "", fmt.Sprintf("could not verify recovery branch %s in %s: %v", branch, repoPath, berr)
			case !exists:
				return "", fmt.Sprintf("recovery branch %s no longer exists in %s — the record has no kept branch to promise", branch, repoPath)
			}
			return "", ""
		}
		return "", fmt.Sprintf("could not inspect %s before deletion: %v", wtPath, lerr)
	}
	// A symlink at the recorded path is an alias the record never wrote —
	// archive relocates the directory, it does not link it. Identity checks
	// resolve through the link, so a swapped-in alias to a live checkout
	// would pass them and list foreign content as reclaimable, while the
	// size walk measures the link rather than the tree it names (#5136
	// Codex round 14). Refuse rather than resolve.
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Sprintf("%s is a symlink — refusing to prove identity through an alias the record never wrote; restore the real archived directory or remove the link", wtPath)
	}

	switch {
	case probeErr == nil:
		// Repo-present: the same bidirectional evidence the kill path requires
		// (#3278), PLUS the occupant's registered branch — the pointer alone
		// proves only same-repo membership, and a different session's worktree
		// parked at this recycled path would pass it (#5136 review).
		if err := sessiongit.VerifyRegisteredWorktreeOccupantBranch(wtPath, repoPath, pruneBranchFor(data)); err != nil {
			return "", fmt.Sprintf("%s could not be verified as this session's worktree: %v", wtPath, err)
		}
		// Repo+branch+path are still REUSABLE spellings: after the original
		// archive's removal, `git worktree add <path> <branch>` recreates all
		// three. The af-owned nonce stamped into the registration leaf at archive
		// time is the evidence a re-create cannot fake; a row archived before the
		// nonce existed is held to a stricter predate rule instead (#5189).
		if err := sessiongit.VerifyArchiveOccupant(wtPath, data.ArchiveNonce, session.ArchiveTimeFor(data)); err != nil {
			return "", fmt.Sprintf("%s could not be verified as this session's worktree: %v", wtPath, err)
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
			return "", fmt.Sprintf("could not verify recovery branch %s in %s: %v", branch, repoPath, berr)
		case !exists:
			return "", fmt.Sprintf("recovery branch %s no longer exists in %s — the record has no kept branch to promise", branch, repoPath)
		}
	case errors.Is(probeErr, sessiongit.ErrRepoGone):
		return "", fmt.Sprintf("origin repository %s is gone, so the branch prune promises to keep is gone with it — restore the repository first, or preserve the worktree contents and remove it manually", repoPath)
	default:
		return "", fmt.Sprintf("could not establish whether repo %s is present: %v", repoPath, probeErr)
	}

	// Uncommitted content in an archived worktree is the ONLY copy — archive
	// relocates bytes verbatim and snapshots only on the disposable-backends
	// path. IGNORED files count too: a gitignored .env exists nowhere else
	// and the kept branch cannot restore what it never tracked (#5136 Codex
	// round 5). The branch survives reclaim, but it has no such content, so
	// deleting here would silently destroy work the listing claims is
	// recoverable. Restore it or clean it first.
	reasons, err := sessiongit.WorktreeUnrecoverableContent(wtPath)
	if err != nil {
		return "", fmt.Sprintf("could not verify %s is clean before deletion: %v", wtPath, err)
	}
	if len(reasons) > 0 {
		return "", fmt.Sprintf("worktree holds content the kept branch does not contain: %s", strings.Join(reasons, "; "))
	}
	// Every commit of the worktree must be on the kept branch: HEAD is the branch
	// tip, so nothing detached or extra exists only here.
	head, err = sessiongit.WorktreeHeadMatchesBranch(wtPath, repoPath, pruneBranchFor(data))
	if err != nil {
		return "", fmt.Sprintf("%s holds work the kept branch does not: %v", wtPath, err)
	}
	return head, ""
}

// pruneBranchFor resolves the branch reclaim promises to keep. Branch is the
// record-level field; Worktree.BranchName is the worktree's own copy and the
// fallback for rows written before the top-level field.
func pruneBranchFor(data session.InstanceData) string { return session.KeptBranchOf(data) }

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
	if req.Apply {
		// The dry run stays a pure read; apply deletes, so it re-enters the
		// mutation admission gate like every other state mutation (#5189).
		if err := s.requireStateMutationAdmission(); err != nil {
			return err
		}
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	result, err := s.manager.PruneSessions(req)
	*resp = result
	return err
}

// boundedRead runs a persisted-row read on its own goroutine and returns its
// answer, or timeout after pruneProbeDeadline: a stalled AF-home mount must not
// wedge the prune handler (#5189). The parked read is the same accepted leak the
// other bounded probes carry.
func boundedRead[T any](read func() T, timeout T) T {
	done := make(chan T, 1)
	go func() { done <- read() }()
	select {
	case v := <-done:
		return v
	case <-time.After(pruneProbeDeadline):
		return timeout
	}
}
