package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyBranchExists reports whether `branch` is a local ref in repoRoot.
func legacyBranchExists(t *testing.T, repoRoot, branch string) bool {
	t.Helper()
	c := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return c.Run() == nil
}

// TestLegacyNilProvenance_CleanupPreservesReusedUserBranch is the archive/kill
// half of #1953, and it is a DIFFERENT legacy shape than the one the issue
// describes.
//
// The issue's shape is external_worktree=true + branch_created_by_us=nil, which
// Cleanup() is already immune to: it early-returns on externalWorktree before
// ever reaching the branch-delete gate.
//
// This shape is NOT external. `setupFromExistingBranch` (2025-07-23) sets
// branchCreatedByUs=false whenever a NORMAL, AF-created linked worktree is built
// on a branch the user already had — no external flag involved. Records written
// before branch_created_by_us landed (2026-04-17) persisted that provenance as
// nothing at all, so FromInstanceData's nil→true default resurrects them as
// "AF created this branch". Cleanup() has no externalWorktree escape hatch for
// them, so kill/archive force-deletes a branch the user owned.
//
// Drives the real restore path (FromInstanceData) into the real destruction
// path (GitWorktree.Cleanup, which teardownKill.handleWorktree calls) against a
// real git repo and a real pre-existing branch.
//
// The record loads as LiveArchived so FromInstanceData returns it inert (no tmux
// spawn) — this is the archived-then-killed path, and it is representative: the
// provenance binding happens in the shared backend branch BEFORE the archived
// early-return, so a live session's gitWorktree is built identically and reaches
// the same Cleanup() via the same teardownKill.
func TestLegacyNilProvenance_CleanupPreservesReusedUserBranch(t *testing.T) {
	repoRoot := initInPlaceRepo(t, "user-feature")
	// Park the repo back on the default branch so the linked worktree below can
	// hold user-feature (a branch cannot be checked out twice).
	gitOut(t, repoRoot, "checkout", "-q", "-")

	// A NORMAL AF linked worktree built on the user's PRE-EXISTING branch. This
	// is exactly what setupFromExistingBranch produces: not external, and the
	// branch is not ours.
	wt := filepath.Join(t.TempDir(), "wt")
	gitOut(t, repoRoot, "worktree", "add", "-q", wt, "user-feature")
	require.True(t, legacyBranchExists(t, repoRoot, "user-feature"))

	// A record written before 2026-04-17: the field simply is not there.
	data := InstanceData{
		Title:    "legacy-reused",
		Path:     repoRoot,
		Liveness: LiveArchived,
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      wt,
			SessionName:       "legacy-reused",
			BranchName:        "user-feature",
			ExternalWorktree:  false,
			BranchCreatedByUs: nil,
		},
	}

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.NotNil(t, restored.gitWorktree)

	// The kill/archive teardown step, verbatim (teardownKill.handleWorktree).
	require.NoError(t, restored.gitWorktree.Cleanup())

	assert.True(t, legacyBranchExists(t, repoRoot, "user-feature"),
		"#1953: kill/archive force-deleted the user's pre-existing branch %q "+
			"because a legacy record's missing branch_created_by_us defaulted to true",
		"user-feature")
}

// TestLegacyNilProvenance_RestoreDoesNotClaimBranchOwnership pins the shared
// default itself at the restore boundary: an absent branch_created_by_us must
// never be read as "AF created this branch". Unknown provenance means KEEP.
func TestLegacyNilProvenance_RestoreDoesNotClaimBranchOwnership(t *testing.T) {
	repoRoot := initInPlaceRepo(t, "user-feature")

	for _, tc := range []struct {
		name     string
		external bool
		flag     *bool
		want     bool
	}{
		{"legacy nil is not ours", false, nil, false},
		{"legacy nil external is not ours", true, nil, false},
		{"explicit true is honored", false, boolPtrForTest(true), true},
		{"explicit false is honored", false, boolPtrForTest(false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt := filepath.Join(t.TempDir(), "wt")
			require.NoError(t, os.MkdirAll(wt, 0755))
			restored, err := FromInstanceData(InstanceData{
				Title:    "t",
				Path:     repoRoot,
				Liveness: LiveArchived,
				Worktree: GitWorktreeData{
					RepoPath:          repoRoot,
					WorktreePath:      wt,
					SessionName:       "t",
					BranchName:        "user-feature",
					ExternalWorktree:  tc.external,
					BranchCreatedByUs: tc.flag,
				},
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, restored.gitWorktree.BranchCreatedByUs())
		})
	}
}

func boolPtrForTest(b bool) *bool { return &b }
