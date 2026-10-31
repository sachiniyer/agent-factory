package session

import (
	"fmt"
	"time"
)

// This file is the session-domain half of `af sessions prune --apply` (#5189):
// the durable intent, the tombstone, and the refusals that keep `list` and
// restore honest about a row whose worktree was deleted on purpose.

// PruneIntentData is written to the row BEFORE prune's first irreversible step
// (the rename of the worktree to TombstonePath). It carries everything a later
// run needs to finish — or cleanly abandon — the deletion without judging a
// partly-deleted tree again: the worktree and tombstone paths, the object ids the
// verdict proved equal, and the occupant nonce it checked.
type PruneIntentData struct {
	// WorktreePath is where the archived tree lived; TombstonePath is where the
	// rename put it and the ONLY path the deletion ever removes.
	WorktreePath  string `json:"worktree_path"`
	TombstonePath string `json:"tombstone_path"`
	Head          string `json:"head"`
	Branch        string `json:"branch"`
	Nonce         string `json:"nonce,omitempty"`
	// LeafName and LeafNonce identify the registration prune removes: the leaf's
	// directory name and the nonce stamped into it before the intent was written.
	// Resume removes a leaf only if it still carries LeafNonce.
	LeafName  string `json:"leaf_name,omitempty"`
	LeafNonce string `json:"leaf_nonce,omitempty"`
	// Renamed is set once the rename happened and was persisted, so a resume knows
	// which side of step 5 it is on without guessing from the filesystem alone.
	Renamed bool      `json:"renamed,omitempty"`
	At      time.Time `json:"at"`
}

// pruneState is the prune bookkeeping an Instance carries (see InstanceData).
type pruneState struct {
	nonce    string
	prunedAt time.Time
	intent   *PruneIntentData
}

func clonePruneIntent(in *PruneIntentData) *PruneIntentData {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// PrunedAt is when prune deleted this row's worktree; zero when it did not.
func (i *Instance) PrunedAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.prune.prunedAt
}

// IsPruned reports whether the row is a prune tombstone.
func (i *Instance) IsPruned() bool { return !i.PrunedAt().IsZero() }

// PruneIntent returns a copy of the durable intent, or nil.
func (i *Instance) PruneIntent() *PruneIntentData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return clonePruneIntent(i.prune.intent)
}

// ArchiveNonce is the occupant nonce recorded at archive time ("" before it).
func (i *Instance) ArchiveNonce() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.prune.nonce
}

// None of these bookkeeping methods touches UpdatedAt: for a row archived before
// archived_at existed, UpdatedAt IS the archive time (ArchiveTimeFor), and a prune
// that moved it would make its own interrupted work look too recent to resume or
// re-judge.
//
// SetPruneIntent records the intent. It refuses a row that is not archived,
// already carries an intent, or is already a tombstone, so two runs cannot both
// own the deletion.
func (i *Instance) SetPruneIntent(intent PruneIntentData) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	switch {
	case i.liveness != LiveArchived:
		return fmt.Errorf("session %q is not archived", i.Title)
	case !i.prune.prunedAt.IsZero():
		return fmt.Errorf("session %q is already pruned", i.Title)
	case i.prune.intent != nil:
		return fmt.Errorf("session %q already has a prune in progress", i.Title)
	}
	i.prune.intent = clonePruneIntent(&intent)
	return nil
}

// MarkPruneRenamed notes that the rename of the worktree to the tombstone path
// happened.
func (i *Instance) MarkPruneRenamed() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.prune.intent == nil {
		return fmt.Errorf("session %q has no prune in progress", i.Title)
	}
	i.prune.intent.Renamed = true
	return nil
}

// ClearPruneIntent abandons an intent whose worktree was never moved.
func (i *Instance) ClearPruneIntent() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.prune.intent != nil {
		i.prune.intent = nil
	}
}

// CompletePrune turns the intent into the tombstone.
func (i *Instance) CompletePrune(at time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.prune.intent == nil && i.prune.prunedAt.IsZero() {
		return fmt.Errorf("session %q has no prune in progress", i.Title)
	}
	if i.prune.prunedAt.IsZero() {
		i.prune.prunedAt = at
	}
	i.prune.intent = nil
	return nil
}

// PrunedRestoreError is restore's refusal for a tombstone: it names the branch
// the work survives on.
func PrunedRestoreError(title, branch string, at time.Time) error {
	return fmt.Errorf("session %q was pruned at %s: its archived worktree was deleted. The work is on branch %q — check it out in the repository (git worktree add <path> %s) or recreate the session from it",
		title, at.Format(time.RFC3339), branch, branch)
}

// KeptBranchOf is the branch prune keeps and names back to the user: the
// record-level Branch, else the worktree's own copy (rows written before the
// top-level field).
func KeptBranchOf(data InstanceData) string {
	if data.Branch != "" {
		return data.Branch
	}
	return data.Worktree.BranchName
}

// KeptBranch is KeptBranchOf for a live Instance.
func (i *Instance) KeptBranch() string { return KeptBranchOf(i.ToInstanceData()) }
