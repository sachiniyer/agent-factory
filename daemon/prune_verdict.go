package daemon

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// pruneVerdict is THE judgment on one archived row, used by the dry run to list it
// and by --apply to act on it (#5189). There is no second copy of these rules:
// apply calls this function, under its claim, immediately before the first
// irreversible step, so what the dry run lists and what apply deletes cannot
// drift. It returns either a candidate or the reason the row is skipped.
//
// ownClaim is true when the caller already holds the row's killsInFlight claim
// (apply does); the busy check then ignores that one claim and nothing else.
func (m *Manager) pruneVerdict(repoID string, instance *session.Instance, cutoff time.Time, ownClaim bool) (PruneCandidate, *PruneSkippedEntry, []string) {
	var warnings []string
	data := instance.ToInstanceData()
	skip := func(reason string) (PruneCandidate, *PruneSkippedEntry, []string) {
		return PruneCandidate{}, &PruneSkippedEntry{ID: data.ID, Title: data.Title, RepoID: repoID, Reason: reason, Interrupted: data.PruneIntent != nil}, warnings
	}
	if reason := session.PruneSkipReason(data, cutoff); reason != "" {
		return skip(reason)
	}
	busyNow := func() (bool, int) {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, busy := m.killsInFlight[daemonInstanceKey(repoID, data.Title)]
		return busy && !ownClaim, m.pendingConversationCaptures[instance]
	}
	if busy, captures := busyNow(); busy {
		return skip("an operation is already in progress for this session")
	} else if captures > 0 {
		return skip("a provider conversation capture is still in flight")
	}
	if report := instance.GetArchiveReport(); !report.Empty() {
		// The live worktree carries an incomplete-archive report the serialized
		// row only stages: same refusal either way, read from the authoritative
		// handle.
		return skip("incomplete archive — retained source trees are present and need manual review")
	}
	// Prune acts only on a direct child of this repo's archive root; a record that
	// names any other location is refused here, so the dry run shows it too.
	if wt := data.Worktree.WorktreePath; wt != "" {
		if err := pruneUnderArchiveRoot(repoID, wt); err != nil {
			return skip(fmt.Sprintf("the recorded archive location is outside what prune manages: %v", err))
		}
	}
	// The plan must read like the apply it previews (#5136 review): the same
	// deletion-boundary refusals run here, so a foreign-path occupant or a dirty
	// worktree shows as skipped in the dry run instead of appearing prunable and
	// refusing only after --apply.
	head, reason := pruneFilesystemRefusal(data)
	if reason != "" {
		return skip(reason)
	}
	worktreeBytes, sizeErr := session.DirSizeBytes(data.Worktree.WorktreePath)
	if sizeErr != nil {
		warnings = append(warnings, fmt.Sprintf("session %q: could not fully measure %s: %v", data.Title, data.Worktree.WorktreePath, sizeErr))
	}
	if prunePostProbeHook != nil {
		prunePostProbeHook(instance)
	}
	// Revalidate AFTER the filesystem probes AND the size walk: a restore or kill
	// can begin while either ran — the stale snapshot still classifies archived,
	// and the missing-path arm treats a moved-away tree as a zero-byte candidate —
	// so a session already live again would publish (#5136 Codex rounds 13, 14).
	// Re-read the instance and re-run every quiet-state gate.
	fresh := instance.ToInstanceData()
	if reason := session.PruneSkipReason(fresh, cutoff); reason != "" {
		return skip("session state changed while being evaluated: " + reason)
	}
	// An archived-title reuse renames this row's title, worktree, and branch
	// without touching liveness or holding an op (#5136 Codex round 15).
	if fresh.ID != data.ID || fresh.Title != data.Title ||
		fresh.Worktree.WorktreePath != data.Worktree.WorktreePath ||
		pruneBranchFor(fresh) != pruneBranchFor(data) ||
		fresh.ArchiveNonce != data.ArchiveNonce ||
		!session.ArchiveTimeFor(fresh).Equal(session.ArchiveTimeFor(data)) {
		return skip("session identity changed while being evaluated — rerun to see the current record")
	}
	if busy, captures := busyNow(); busy || captures > 0 {
		return skip("session state changed while being evaluated: an operation is already in progress")
	}
	if !instance.GetArchiveReport().Empty() {
		return skip("session state changed while being evaluated: incomplete archive")
	}
	return PruneCandidate{
		ID:               data.ID,
		Title:            data.Title,
		RepoID:           repoID,
		Branch:           pruneBranchFor(data),
		ArchivedAt:       session.ArchiveTimeFor(data),
		ReclaimableBytes: worktreeBytes,
		BranchPushed:     !data.BranchUnpushed,
		head:             head,
	}, nil, warnings
}
