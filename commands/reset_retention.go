package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session"
)

// This file holds the factory reset's "keep what did not finish" half: the
// record retention that makes a partial reset honestly recoverable, and the
// guarded residue removal that never deletes what a failed step left behind.

// removeArchivedDirs removes each per-repo archived-worktree tree under
// <AF_HOME>/archived/<repoID>/, EXCEPT for repos in preserve (the corrupt/
// unreadable ones). Those repos' records survive the reset and still point at
// their archives, so removing the archives would leave a dangling reference.
// Archived dirs for deleted repos — and orphaned dirs with no record at all —
// are removed. Returns any per-dir removal errors (best-effort, non-fatal).
func removeArchivedDirs(configDir string, preserve []string) []error {
	archivedRoot := filepath.Join(configDir, "archived")
	entries, err := os.ReadDir(archivedRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{fmt.Errorf("read %s: %w", archivedRoot, err)}
	}

	keep := make(map[string]struct{}, len(preserve))
	for _, id := range preserve {
		keep[id] = struct{}{}
	}

	var errs []error
	for _, e := range entries {
		if _, preserved := keep[e.Name()]; preserved {
			continue // preserved repo — keep its archives consistent with its record
		}
		p := filepath.Join(archivedRoot, e.Name())
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	// If nothing is preserved, the archived/ root is now empty — drop it too so a
	// clean reset leaves no stray dir behind.
	if len(preserve) == 0 {
		if err := os.RemoveAll(archivedRoot); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", archivedRoot, err))
		}
	}
	return errs
}

// pruneWorktreeResidue empties the AF worktrees/ tree entry-by-entry, keeping
// any top-level entry that holds a worktree the reset deliberately left in place
// (#2110). It is the guarded form of the wholesale `os.RemoveAll(worktrees/)`.
//
// Keeping is TOP-LEVEL: an entry that merely contains a blocked worktree is kept
// whole, siblings included. Over-preserving is the safe direction — the residue
// is one directory that the recovery re-run removes once the worktree is gone,
// whereas under-preserving destroys a checkout git still owns.
func pruneWorktreeResidue(root string, keep []string) []error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{fmt.Errorf("read %s: %w", root, err)}
	}
	var errs []error
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if holdsAnyPath(p, keep) {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	return errs
}

// holdsAnyPath reports whether dir IS, or contains, any of paths.
//
// Both sides go through pathutil.ResolveForCompare so a record stored with one
// spelling matches a scan that walked another — and, critically, so a blocked
// worktree whose DIRECTORY is already gone still matches: a plain EvalSymlinks
// cannot resolve a missing leaf, which on a symlinked root (macOS /var ->
// /private/var) silently turns "keep this" into "delete this" (#2110).
func holdsAnyPath(dir string, paths []string) bool {
	dir = pathutil.ResolveForCompare(dir)
	for _, p := range paths {
		p = pathutil.ResolveForCompare(p)
		if p == dir || pathutil.IsStrictlyInside(p, dir) {
			return true
		}
	}
	return false
}

// retainIncompleteInstances rewrites repoID's record set down to ONLY the
// sessions whose cleanup did not finish — the worktree git refused to release
// (#2110), or the branch that could not be confirmed gone because its existence
// probe or deletion failed (#3243) — dropping every record the reset actually
// completed. It returns how many records were kept, by category, so the summary
// counts only what was really removed.
//
// This is what makes the printed recovery honest. `af reset` deletes records
// even on partial failure, so the old "re-run `af reset` to finish" planned
// nothing on the second run and the blocked branch was stuck forever. Keeping
// the incomplete sessions' records — and only those — means the re-run plans
// exactly the leftover work. The branch match applies the same gates as
// planFactoryReset (non-external, branchCreatedByAF), so a record that merely
// shares the branch NAME without having planned its deletion is not retained.
func retainIncompleteInstances(repoID string, blockedPaths, unverifiedBranches []string) (resetSessionCounts, error) {
	keepPaths := make(map[string]struct{}, len(blockedPaths))
	for _, p := range blockedPaths {
		keepPaths[filepath.Clean(p)] = struct{}{}
	}
	keepBranches := make(map[string]struct{}, len(unverifiedBranches))
	for _, b := range unverifiedBranches {
		keepBranches[b] = struct{}{}
	}
	var keptCounts resetSessionCounts
	err := config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var recs []session.InstanceData
		if err := json.Unmarshal(raw, &recs); err != nil {
			return nil, fmt.Errorf("read instances: %w", err)
		}
		keptCounts = resetSessionCounts{}
		kept := make([]session.InstanceData, 0, len(blockedPaths)+len(unverifiedBranches))
		for _, r := range recs {
			retain := false
			if r.Worktree.WorktreePath != "" {
				if _, blocked := keepPaths[filepath.Clean(r.Worktree.WorktreePath)]; blocked {
					retain = true
				}
			}
			if !retain && !r.Worktree.ExternalWorktree && branchCreatedByAF(r.Worktree) {
				if _, unverified := keepBranches[r.Worktree.BranchName]; unverified {
					retain = true
				}
			}
			if !retain {
				continue
			}
			kept = append(kept, r)
			if session.IsArchivedData(r) {
				keptCounts.archived++
			} else {
				keptCounts.sessions++
			}
		}
		return json.Marshal(kept)
	})
	if err != nil {
		return resetSessionCounts{}, err
	}
	return keptCounts, nil
}
