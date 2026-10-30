package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// `af sessions prune --apply` (#5189). The design is the one posted on the issue;
// this file is its executor.
//
// ONE VERDICT: apply deletes only what pruneVerdict (prune_verdict.go) passes, and
// that is the same function the dry run lists with. The claim, the lock and the
// verdict are taken per row immediately before the first irreversible step.
//
// ORDER (each numbered step is a crash point the tests inject at):
//
//	1 intent  : persist prune_intent on the row
//	2 rename  : worktree → <archive>/.prune-<id>-<rand> (atomic, same directory)
//	3 leaf    : remove OUR registration leaf only
//	4 remove  : delete the renamed tree (and only that path)
//	5 tombstone: clear the intent, set pruned_at, persist
//
// After step 1 everything a re-run needs is on the row (pruneFinish below).

// PrunedEntry is one session whose archived worktree was deleted. The row stays,
// as a tombstone naming the kept branch.
type PrunedEntry struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// EstimatedBytes is the allocated-size estimate taken before deletion — an
	// upper bound ("up to"), not freed bytes.
	EstimatedBytes int64 `json:"estimated_bytes"`
	BranchPushed   bool  `json:"branch_pushed"`
	// Resumed is true when this run finished a deletion an earlier run started.
	Resumed bool `json:"resumed,omitempty"`
}

// PruneIncompleteEntry is a row whose worktree deletion started but whose record
// is not yet final — distinct from skipped (nothing was changed). Re-running
// completes it from the durable intent.
type PruneIncompleteEntry struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// Step is the numbered step that did not finish.
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// pruneStepHook is a test seam: called before each numbered step with its name; a
// non-nil return simulates a crash there (the run stops with no cleanup).
var pruneStepHook func(step string) error

// pruneRemoveTimeout bounds the recursive removal of one tombstoned tree.
var pruneRemoveTimeout = 10 * time.Minute

// pruneTombstonePersist writes the tombstone record under the repo's ordering lock.
// A package var so a test can fail exactly that write.
var pruneTombstonePersist = func(_ *Manager, repoID string, data session.InstanceData) error {
	return boundedPrunePersist(repoID, data)
}

// pruneBetweenTombstoneSteps is a test seam fired between the durable tombstone
// write and the in-memory commit, to interleave a competing writer there.
var pruneBetweenTombstoneSteps func()

// commitPruneTombstone writes the tombstone and then commits it in memory. The
// caller holds the repository start lock (lockForPrune), so the write and the
// commit are one critical section against every other writer.
func (m *Manager) commitPruneTombstone(repoID string, instance *session.Instance, at time.Time) error {
	tomb := instance.ToInstanceData()
	tomb.PrunedAt, tomb.PruneIntent = at, nil
	if err := pruneTombstonePersist(m, repoID, tomb); err != nil {
		return fmt.Errorf("the worktree is deleted but the record could not be saved (re-run to finish): %w", err)
	}
	if pruneBetweenTombstoneSteps != nil {
		pruneBetweenTombstoneSteps()
	}
	return instance.CompletePrune(at)
}

var errPruneCrash = errors.New("prune stopped (injected)")

func pruneStep(step string) error {
	if pruneStepHook != nil {
		return pruneStepHook(step)
	}
	return nil
}

// claimPruneOperation is claimRestoreOperation's admission for prune: it makes
// the row busy for every other lifecycle operation and refuses while the project
// is being deleted (or was, while this run waited for the row's lock).
func (m *Manager) claimPruneOperation(repoID, key, title string, waited time.Duration, sinceDeleteSeq uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, deleting := m.projectDeletes[repoID]; deleting {
		return fmt.Errorf("cannot prune session %q%s: project %s is being deleted", title, afterOpLockWait(waited), repoID)
	}
	if m.projectDeleteMovedLocked(repoID, sinceDeleteSeq) {
		return fmt.Errorf("cannot prune session %q%s: project %s was deleted while this run was waiting", title, afterOpLockWait(waited), repoID)
	}
	if _, busy := m.killsInFlight[key]; busy {
		return fmt.Errorf("an operation is already in progress for session %q%s", title, afterOpLockWait(waited))
	}
	m.killsInFlight[key] = struct{}{}
	return nil
}

func (m *Manager) releasePruneOperation(key string) {
	m.mu.Lock()
	delete(m.killsInFlight, key)
	m.mu.Unlock()
}

// pruneApply runs the apply half over the dry-run listing.
func (m *Manager) pruneApply(req PruneSessionsRequest, resp PruneSessionsResponse) (PruneSessionsResponse, error) {
	resp.Applied = true
	resp.Pruned = []PrunedEntry{}
	free := newFreeSpaceProbe(m.pruneArchiveRoots(req))

	// A deletion an earlier run started is finished first, from its durable
	// intent, so a crash never leaves a half-pruned row blocking the listing.
	for _, row := range m.pruneRows(req) {
		if row.instance.PruneIntent() == nil || (len(req.Only) > 0 && !containsString(req.Only, row.instance.ID)) {
			continue
		}
		entry, inc := m.resumePruneIntent(row.repoID, row.instance)
		switch {
		case inc != nil:
			resp.Incomplete = append(resp.Incomplete, *inc)
		case entry != nil:
			resp.Pruned = append(resp.Pruned, *entry)
		}
	}

	candidates, skipped, warnings := m.pruneCandidates(req, resp.ArchivedBefore)
	resp.Warnings = warnings
	only := map[string]bool{}
	for _, id := range req.Only {
		only[id] = true
	}
	bound := len(only) > 0
	for _, c := range candidates {
		if bound && !only[c.ID] {
			skipped = append(skipped, PruneSkippedEntry{ID: c.ID, Title: c.Title, RepoID: c.RepoID,
				Reason: "not in the confirmed plan — left untouched"})
			continue
		}
		entry, skip, inc := m.pruneApplyOne(c, resp.ArchivedBefore)
		switch {
		case skip != nil:
			skipped = append(skipped, *skip)
		case inc != nil:
			resp.Incomplete = append(resp.Incomplete, *inc)
		case entry != nil:
			resp.Pruned = append(resp.Pruned, *entry)
		}
	}
	if bound {
		seen := map[string]bool{}
		for _, c := range candidates {
			seen[c.ID] = true
		}
		for _, s := range skipped {
			if s.ID != "" {
				seen[s.ID] = true
			}
		}
		for id := range only {
			if !seen[id] {
				skipped = append(skipped, PruneSkippedEntry{ID: id, Reason: "planned session no longer exists or is no longer listed — nothing was deleted for it"})
			}
		}
	}
	resp.Skipped = skipped
	sort.Slice(resp.Skipped, func(a, b int) bool {
		if resp.Skipped[a].RepoID != resp.Skipped[b].RepoID {
			return resp.Skipped[a].RepoID < resp.Skipped[b].RepoID
		}
		return resp.Skipped[a].Title < resp.Skipped[b].Title
	})
	for _, p := range resp.Pruned {
		resp.ReclaimedEstimateBytes += p.EstimatedBytes
	}
	resp.FreeBytesBefore, resp.FreeBytesAfter, resp.FreeBytesKnown = free.delta()
	return resp, nil
}

type pruneRow struct {
	repoID   string
	instance *session.Instance
}

func (m *Manager) pruneRows(req PruneSessionsRequest) []pruneRow {
	var rows []pruneRow
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, instance := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		if req.RepoID != "" && repoID != req.RepoID {
			continue
		}
		rows = append(rows, pruneRow{repoID: repoID, instance: instance})
	}
	return rows
}

// pruneApplyOne deletes one candidate: lock, claim, re-judge with the dry run's
// own verdict, then the numbered steps.
func (m *Manager) pruneApplyOne(c PruneCandidate, cutoff time.Time) (*PrunedEntry, *PruneSkippedEntry, *PruneIncompleteEntry) {
	skip := func(reason string) (*PrunedEntry, *PruneSkippedEntry, *PruneIncompleteEntry) {
		return nil, &PruneSkippedEntry{ID: c.ID, Title: c.Title, RepoID: c.RepoID, Reason: reason}, nil
	}
	key := daemonInstanceKey(c.RepoID, c.Title)
	m.mu.Lock()
	instance := m.instances[key]
	m.mu.Unlock()
	if instance == nil || instance.ID != c.ID {
		return skip("session changed before it could be pruned")
	}

	release, err := m.lockForPrune(c.RepoID, key, c.Title, "prune", instance, true)
	if err != nil {
		return skip(err.Error())
	}
	defer release()
	if err := m.pruneAbandonedGuard(c.RepoID, instance, nil); err != nil {
		return skip(err.Error())
	}

	// The second judgment IS the first: the same verdict, under the claim.
	fresh, verdictSkip, _ := m.pruneVerdict(c.RepoID, instance, cutoff, true)
	if verdictSkip != nil {
		verdictSkip.ID = c.ID
		return nil, verdictSkip, nil
	}
	if fresh.ID != c.ID || fresh.Branch != c.Branch || fresh.head != c.head {
		return skip("the worktree changed since the plan was made — rerun to see the current state")
	}

	data := instance.ToInstanceData()
	wt := data.Worktree.WorktreePath
	intent := session.PruneIntentData{
		WorktreePath: wt, Head: fresh.head, Branch: fresh.Branch,
		Nonce: instance.ArchiveNonce(), At: time.Now(),
	}
	if fresh.head != "" { // an out-of-band-deleted tree has nothing to rename
		// Stamp the registration while the occupant is still the verified one; the
		// intent carries the stamp so resume can prove which leaf is ours.
		leafName, leafNonce, err := sessiongit.StampPruneNonce(wt)
		if err != nil {
			return skip(fmt.Sprintf("could not mark the worktree's registration, so nothing was touched: %v", err))
		}
		intent.LeafName, intent.LeafNonce = leafName, leafNonce
		intent.TombstonePath = filepath.Join(filepath.Dir(wt), ".prune-"+shortPruneID(data.ID)+"-"+randomSuffix())
	}

	// 1. intent
	if err := pruneStep("intent"); err != nil {
		return nil, nil, &PruneIncompleteEntry{ID: c.ID, Title: c.Title, RepoID: c.RepoID, Step: "intent", Reason: err.Error()}
	}
	if err := instance.SetPruneIntent(intent); err != nil {
		return skip(err.Error())
	}
	if err := m.persistPruneRow(c.RepoID, instance); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// The write may still land: the outcome is UNKNOWN, not "failed". The
			// intent therefore STAYS in memory (every lifecycle operation refuses on
			// it, and the abandoned write is registered), and the row reports
			// incomplete. Clearing it would let a restore succeed and then be
			// overwritten by the delayed write carrying the intent.
			return nil, nil, &PruneIncompleteEntry{ID: c.ID, Title: c.Title, RepoID: c.RepoID, Step: "intent", Reason: err.Error()}
		}
		instance.ClearPruneIntent()
		return skip(fmt.Sprintf("could not record the prune intent, so nothing was touched: %v", err))
	}
	// 2. rename
	if err := pruneStep("rename"); err != nil {
		return nil, nil, &PruneIncompleteEntry{ID: c.ID, Title: c.Title, RepoID: c.RepoID, Step: "rename", Reason: err.Error()}
	}
	if intent.TombstonePath != "" {
		if err := sessiongit.BoundedRename(wt, intent.TombstonePath); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				// A rename that has not answered may still complete: its outcome is
				// UNKNOWN, so the intent stays and the row reports incomplete; the
				// next run decides from the filesystem, never from this guess.
				return nil, nil, &PruneIncompleteEntry{ID: c.ID, Title: c.Title, RepoID: c.RepoID, Step: "rename", Reason: err.Error()}
			}
			instance.ClearPruneIntent()
			if perr := m.persistPruneRow(c.RepoID, instance); perr != nil {
				m.warn().Printf("prune %q: could not clear the intent after a failed rename: %v", c.Title, perr)
			}
			return skip(fmt.Sprintf("could not move the worktree aside, so nothing was deleted: %v", err))
		}
		_ = instance.MarkPruneRenamed()
		_ = m.persistPruneRow(c.RepoID, instance) // best effort: the filesystem is the authority on resume
	}
	step, ferr := m.pruneFinish(c.RepoID, instance, instance.PruneIntent())
	if ferr != nil {
		return nil, nil, &PruneIncompleteEntry{ID: c.ID, Title: c.Title, RepoID: c.RepoID, Step: step, Reason: ferr.Error()}
	}
	m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
	return &PrunedEntry{ID: c.ID, Title: c.Title, RepoID: c.RepoID, Branch: c.Branch,
		EstimatedBytes: c.ReclaimableBytes, BranchPushed: c.BranchPushed}, nil, nil
}

// pruneFinish carries a recorded intent to the tombstone: leaf, removal, record.
// It is the ONLY code that deletes, for a fresh apply and for every resume, and it
// deletes only intent.TombstonePath. On failure it returns the numbered step that
// did not finish; the intent stays on the row.
func (m *Manager) pruneFinish(repoID string, instance *session.Instance, intent *session.PruneIntentData) (string, error) {
	if intent == nil {
		return "intent", fmt.Errorf("session %q has no prune intent", instance.Title)
	}
	data := instance.ToInstanceData()
	repoPath := data.Worktree.RepoPath
	if repoPath == "" {
		repoPath = data.Path
	}
	if intent.TombstonePath != "" {
		if err := validatePruneTombstonePath(intent); err != nil {
			return "remove", err
		}
	}
	if err := pruneStep("leaf"); err != nil {
		return "leaf", err
	}
	if intent.LeafName != "" { // none when the tree was already gone out of band
		if err := sessiongit.RemoveArchivedRegistrationLeaf(repoPath, intent.LeafName, intent.LeafNonce); err != nil {
			return "leaf", fmt.Errorf("could not remove the worktree registration: %w", err)
		}
	}
	if err := pruneStep("remove"); err != nil {
		return "remove", err
	}
	if intent.TombstonePath != "" {
		if err := pruneRemoveTree(intent.TombstonePath); err != nil {
			return "remove", err
		}
	}
	if err := pruneStep("tombstone"); err != nil {
		return "tombstone", err
	}
	// The tombstone is written FIRST and memory follows it, and the two are ONE
	// critical section under the repository ordering lock: any other writer (the
	// worktree-missing poll, a status checkpoint) snapshots the row only after
	// acquiring that same lock, so it can never see the durable tombstone beside an
	// in-memory row that still carries the intent with its worktree gone. If the
	// write fails the row keeps its intent in memory as well as on disk, so the next
	// apply (or the start-up resume) in this daemon finishes it and the row reports
	// incomplete instead of reading as pruned until a restart repairs it.
	if err := m.commitPruneTombstone(repoID, instance, time.Now()); err != nil {
		return "tombstone", err
	}
	return "", nil
}

// validatePruneTombstonePath refuses an intent whose tombstone is not exactly a
// ".prune-" sibling of the worktree: a hand-edited record cannot point the
// deletion anywhere else.
func validatePruneTombstonePath(intent *session.PruneIntentData) error {
	tomb, wt := filepath.Clean(intent.TombstonePath), filepath.Clean(intent.WorktreePath)
	if filepath.Dir(tomb) != filepath.Dir(wt) || !strings.HasPrefix(filepath.Base(tomb), ".prune-") || tomb == wt {
		return fmt.Errorf("prune tombstone %q is not a .prune- sibling of %q; refusing to delete it", intent.TombstonePath, intent.WorktreePath)
	}
	return nil
}

// pruneRemoveTree deletes the renamed tree, bounded. A tree already gone is done.
// It carries no identity evidence that a partial removal could destroy: the path is
// proven by its structure (validatePruneTombstonePath: a .prune- sibling named by
// the durable intent), not by a marker inside it, so a RemoveAll that fails
// partway is simply re-run. (The registration leaf, which does carry a nonce, is
// removed identity-last: see sessiongit.RemoveArchivedRegistrationLeaf.)
func pruneRemoveTree(path string) error {
	info, err := sessiongit.BoundedLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("could not inspect %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a plain directory; refusing to delete it", path)
	}
	// A removal that times out is still running with an UNKNOWN outcome; it is
	// registered, and nothing acts on this path again until it finishes.
	if err := sessiongit.BoundedMutationWithin(pruneRemoveTimeout, "deleting "+path, []string{path}, func() error { return os.RemoveAll(path) }); err != nil {
		return fmt.Errorf("deleting %s: %w", path, err)
	}
	return nil
}

// resumePruneIntent finishes (or cleanly abandons) a deletion an earlier run
// started, from the row's intent alone. It never re-judges a partly deleted tree.
func (m *Manager) resumePruneIntent(repoID string, instance *session.Instance) (*PrunedEntry, *PruneIncompleteEntry) {
	data := instance.ToInstanceData()
	intent := instance.PruneIntent()
	inc := func(step string, err error) (*PrunedEntry, *PruneIncompleteEntry) {
		return nil, &PruneIncompleteEntry{ID: data.ID, Title: data.Title, RepoID: repoID, Step: step, Reason: err.Error()}
	}
	if intent == nil {
		return nil, nil
	}
	key := daemonInstanceKey(repoID, data.Title)
	release, err := m.lockForPrune(repoID, key, data.Title, "prune resume", instance, false)
	if err != nil {
		return inc("intent", err)
	}
	defer release()
	intent = instance.PruneIntent() // re-read under the claim
	if intent == nil {
		return nil, nil
	}
	if err := m.pruneAbandonedGuard(repoID, instance, intent); err != nil {
		return inc("intent", err)
	}

	// Resume acts only on a positive answer: ENOENT is "absent", any other error
	// (an unreadable parent, a stalled mount) is "unknown", and unknown leaves the
	// intent in place and the row incomplete — the verdict's fail-closed rule.
	tombExists, err := pruneExists(intent.TombstonePath)
	if err != nil {
		return inc("intent", fmt.Errorf("cannot tell whether the renamed worktree is still there: %w", err))
	}
	origExists, err := pruneExists(intent.WorktreePath)
	if err != nil {
		return inc("intent", fmt.Errorf("cannot tell whether the original worktree path is occupied: %w", err))
	}
	// The rename never happened and the original is intact: abandon. The row is an
	// ordinary archive again and the next verdict judges it from scratch.
	if intent.TombstonePath != "" && !tombExists && origExists && !intent.Renamed {
		instance.ClearPruneIntent()
		if err := m.persistPruneRow(repoID, instance); err != nil {
			return inc("intent", fmt.Errorf("could not clear the abandoned prune intent: %w", err))
		}
		return nil, nil
	}
	step, ferr := m.pruneFinish(repoID, instance, intent)
	if ferr != nil {
		return inc(step, ferr)
	}
	m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
	return &PrunedEntry{ID: data.ID, Title: data.Title, RepoID: repoID, Branch: intent.Branch, BranchPushed: !data.BranchUnpushed, Resumed: true}, nil
}

func shortPruneID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "row"
	}
	return id
}

func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// pruneResumeBudget bounds the whole start-up resume pass.
const pruneResumeBudget = 2 * time.Minute

// launchPruneIntentResume finishes prune deletions a previous daemon began. It runs
// on its own goroutine AFTER readiness, is bounded as a whole, logs each intent it
// resumes by session ID and step, and can never delay start-up (#5189).
func launchPruneIntentResume(m *Manager, stopCh <-chan struct{}, wg interface {
	Add(int)
	Done()
}) {
	rows := m.pruneRows(PruneSessionsRequest{All: true})
	var pending []pruneRow
	for _, r := range rows {
		if r.instance.PruneIntent() != nil {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.NewTimer(pruneResumeBudget)
		defer deadline.Stop()
		for _, r := range pending {
			select {
			case <-stopCh:
				return
			case <-deadline.C:
				m.warn().Printf("prune resume: budget of %s spent; remaining intents are finished by the next `af sessions prune --apply`", pruneResumeBudget)
				return
			default:
			}
			id := r.instance.ID
			intent := r.instance.PruneIntent()
			if intent == nil {
				continue
			}
			m.info().Printf("prune resume: session %s resuming (renamed=%t)", id, intent.Renamed)
			entry, inc := m.resumePruneIntent(r.repoID, r.instance)
			switch {
			case inc != nil:
				m.warn().Printf("prune resume: session %s stopped at step %s: %s", id, inc.Step, inc.Reason)
			case entry != nil:
				m.info().Printf("prune resume: session %s finished (tombstoned, branch %s kept)", id, entry.Branch)
			default:
				m.info().Printf("prune resume: session %s abandoned (worktree was never moved)", id)
			}
		}
	}()
}

// freeSpaceProbe measures the free space of the filesystem(s) the archives live
// on, before and after, so the response can show the MEASURED outcome next to
// the estimate: on a reflink filesystem the gap is real and must not be hidden.
type freeSpaceProbe struct {
	dirs   []string
	before uint64
	known  bool
}

func newFreeSpaceProbe(dirs []string) *freeSpaceProbe {
	p := &freeSpaceProbe{dirs: dirs}
	p.before, p.known = sumFree(dirs)
	return p
}

func (p *freeSpaceProbe) delta() (before, after uint64, known bool) {
	if !p.known {
		return 0, 0, false
	}
	after, ok := sumFree(p.dirs)
	if !ok {
		return 0, 0, false
	}
	return p.before, after, true
}

func sumFree(dirs []string) (uint64, bool) {
	var total uint64
	seen := map[uint64]bool{}
	for _, d := range dirs {
		free, dev, ok := freeBytesAt(d)
		if !ok {
			return 0, false
		}
		// One count per FILESYSTEM (device), not per path: many archive roots share
		// a mount, and summing each would multiply its free space.
		if seen[dev] {
			continue
		}
		seen[dev] = true
		total += free
	}
	return total, len(seen) > 0
}

// pruneArchiveRoots are the directories the in-scope archived worktrees sit in.
func (m *Manager) pruneArchiveRoots(req PruneSessionsRequest) []string {
	var dirs []string
	for _, r := range m.pruneRows(req) {
		d := r.instance.ToInstanceData()
		if d.Worktree.WorktreePath != "" && r.instance.GetLiveness() == session.LiveArchived {
			dirs = append(dirs, filepath.Dir(d.Worktree.WorktreePath))
		}
	}
	return dirs
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pruneExists answers whether path exists. An empty path is absent. ENOENT is a
// definite "no"; any other failure is returned, never read as absence.
func pruneExists(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	return sessiongit.BoundedPathExists(path)
}

// lockForPrune takes everything one prune step on a row needs, in the daemon's
// global lock order (see startLockForRepo): the session operation lock, then the
// repository start lock, then worktree admission (apply only), then the claim. A
// create takes the start lock before admission, so prune must too — taking them in
// the opposite order deadlocks a prune overlapping a create in the same repository.
// Every wait is bounded. While the start lock is held, persist ONLY with the
// lock-free persistPruneRow (persistInstanceErr would take it again).
func (m *Manager) lockForPrune(repoID, key, title, operation string, instance *session.Instance, withAdmission bool) (func(), error) {
	deleteSeq := m.projectDeleteSeqNow()
	opLock, waited, err := m.lockSessionOperationWithin(key, operation, title)
	if err != nil {
		return nil, err
	}
	var unlocks []func()
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
		opLock.Unlock()
	}
	startLock := m.startLockForRepo(repoID)
	if ok, w := lockWithin(startLock, opLockTimeout); !ok {
		release()
		return nil, fmt.Errorf("%s of session %q timed out after %s waiting for another operation in this repository (a create or a persist); the requested %s did not start and made no changes — retry", operation, title, w, operation)
	}
	unlocks = append(unlocks, startLock.Unlock)
	if withAdmission {
		admission, err := m.lockLocalWorktreeAdmissionWithin(repoID, title, operation, instance)
		if err != nil {
			release()
			return nil, err
		}
		if admission != nil {
			unlocks = append(unlocks, func() { unlockWorktreeAdmission(admission) })
		}
	}
	if err := m.claimPruneOperation(repoID, key, title, waited, deleteSeq); err != nil {
		release()
		return nil, err
	}
	unlocks = append(unlocks, func() { m.releasePruneOperation(key) })
	return release, nil
}

// persistPruneRow persists the row without taking the repository start lock, which
// the caller already holds (lockForPrune). It is persistInstanceErr minus the lock.
func (m *Manager) persistPruneRow(repoID string, instance *session.Instance) error {
	return boundedPrunePersist(repoID, instance.ToInstanceData())
}

// prunePersistTimeout bounds one write of the session records (a stalled AF-home
// mount must not wedge prune under its locks). A var for tests.
var prunePersistTimeout = 30 * time.Second

func prunePersistKey(repoID string) string { return "instances\x00" + repoID }

// boundedPrunePersist writes the repo's records with a deadline. A timeout is an
// UNKNOWN outcome (the write may still land): it is registered under the records'
// key, and nothing in prune acts on the repo's rows again until it finishes.
func boundedPrunePersist(repoID string, data session.InstanceData) error {
	write := prunePersistImpl // captured by value: an abandoned write reads no package variable
	return sessiongit.BoundedMutationWithin(prunePersistTimeout, "saving the session records",
		[]string{prunePersistKey(repoID)}, func() error { return write(repoID, data) })
}

// prunePersistImpl is the record writer; a package var so a test can delay it.
var prunePersistImpl = persistInstanceData

// pruneAbandonedGuard refuses to act on a row while a bounded filesystem mutation
// of an earlier step has timed out and may still be running: its outcome is
// unknown, and acting on the row again (a retry that clears the intent, say) could
// be followed by the late mutation. intent is nil for a fresh apply.
func (m *Manager) pruneAbandonedGuard(repoID string, instance *session.Instance, intent *session.PruneIntentData) error {
	data := instance.ToInstanceData()
	repoPath := data.Worktree.RepoPath
	if repoPath == "" {
		repoPath = data.Path
	}
	keys := []string{data.Worktree.WorktreePath, prunePersistKey(repoID)}
	if intent != nil {
		keys = append(keys, intent.WorktreePath, intent.TombstonePath)
		if intent.LeafName != "" {
			keys = append(keys, sessiongit.LeafMutationKey(repoPath, intent.LeafName))
		}
	}
	return sessiongit.MutationInFlight(keys...)
}

// pruneRowFence is THE check every lifecycle operation on a row makes before it
// acts (restore, kill, rename and title reuse, a new prune): the row is fenced
// while it has a prune intent in memory OR a pending abandoned filesystem/record
// mutation that touches it. No caller consults only half: an intent cleared after a
// timed-out write still leaves the abandoned write registered, and an intent in
// memory fences the row whatever the registry says (#5189).
func (m *Manager) pruneRowFence(repoID string, instance *session.Instance) error {
	if instance.PruneIntent() != nil {
		return fmt.Errorf("a prune of its archived worktree is pending or interrupted — run `af sessions prune --apply` to finish it first")
	}
	if err := m.pruneAbandonedGuard(repoID, instance, nil); err != nil {
		return fmt.Errorf("a prune step on it timed out and may still be running: %w", err)
	}
	return nil
}
