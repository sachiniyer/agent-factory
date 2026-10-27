package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/git"
)

// Instance-level wrappers for the worktree identity proofs in
// session/git/worktree_prove.go (#5102). The daemon uses them where a
// worktree-missing row meets a directory at its recorded path: an archive or
// restore that may adopt the user's own checkout put back, and the restore
// tail that must not record or start an agent in a directory swapped in after
// af placed it.

// VerifyClaimedWorktree proves the directory an active archive or restore
// claim resolved is this session's own worktree; see
// git.VerifyClaimedWorktree.
func (i *Instance) VerifyClaimedWorktree(claim git.RelocationClaim) error {
	gw, err := i.worktreeForProof()
	if err != nil {
		return err
	}
	return gw.VerifyClaimedWorktree(claim)
}

// ReconfirmPlacedWorktree checks that the directory af last proved and pinned
// at the recorded path — an adopted move, a rebuild — is still the one there
// (git.ReconfirmAdoptedWorktree). A failure has already installed the fence
// the git layer chooses, and wraps os.ErrNotExist when the directory vanished;
// the caller records that as missing and persists before reporting.
func (i *Instance) ReconfirmPlacedWorktree() error {
	gw, err := i.worktreeForProof()
	if err != nil {
		return err
	}
	return gw.ReconfirmAdoptedWorktree()
}

// RecordedBranchExists reports the worktree's recorded branch and whether it
// still exists in its repo, with git.LocalBranchExists's tri-state: an error
// means the question could not be answered and nothing may be concluded.
func (i *Instance) RecordedBranchExists() (branch string, exists bool, err error) {
	gw, err := i.worktreeForProof()
	if err != nil {
		return "", false, err
	}
	branch = gw.GetBranchName()
	exists, err = git.LocalBranchExists(gw.GetRepoPath(), branch)
	return branch, exists, err
}

func (i *Instance) worktreeForProof() (*git.GitWorktree, error) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return nil, fmt.Errorf("session %q has no worktree", i.Title)
	}
	return gw, nil
}
