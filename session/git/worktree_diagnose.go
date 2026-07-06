package git

import (
	"os"
	"path/filepath"
	"strings"
)

// MissingWorktreeDiagnosis is a cheap filesystem/git snapshot captured when a
// live session points at a worktree directory that no longer exists (#1303).
type MissingWorktreeDiagnosis struct {
	RepoPath     string
	WorktreePath string
	BranchName   string
	ParentPath   string

	RepoExists    bool
	RepoStatError string

	ParentExists    bool
	ParentStatError string

	WorktreeRegistrationKnown bool
	WorktreeRegistered        bool
	WorktreeListError         string

	BranchKnown  bool
	BranchExists bool
	BranchError  string

	ExternalWorktree  bool
	BranchCreatedByUs bool
}

// DiagnoseMissingWorktree captures low-cost context that helps distinguish a
// directory removed out from under a live session from a teardown that also
// cleaned up git's worktree registration. It is intentionally local-only: stat,
// git worktree list, and git show-ref.
func (g *GitWorktree) DiagnoseMissingWorktree() MissingWorktreeDiagnosis {
	d := MissingWorktreeDiagnosis{
		RepoPath:          g.repoPath,
		WorktreePath:      g.worktreePath,
		BranchName:        g.branchName,
		ParentPath:        filepath.Dir(g.worktreePath),
		ExternalWorktree:  g.externalWorktree,
		BranchCreatedByUs: g.branchCreatedByUs,
	}

	if g.repoPath != "" {
		if _, err := os.Stat(g.repoPath); err == nil {
			d.RepoExists = true
		} else {
			d.RepoStatError = err.Error()
		}
	}

	if g.worktreePath != "" {
		if _, err := os.Stat(d.ParentPath); err == nil {
			d.ParentExists = true
		} else {
			d.ParentStatError = err.Error()
		}
	}

	if registered, err := g.isWorktreeRegistered(); err == nil {
		d.WorktreeRegistrationKnown = true
		d.WorktreeRegistered = registered
	} else {
		d.WorktreeListError = err.Error()
	}

	if strings.TrimSpace(g.branchName) != "" {
		if _, err := g.runGitCommand(g.repoPath, "show-ref", "--verify", "refs/heads/"+g.branchName); err == nil {
			d.BranchKnown = true
			d.BranchExists = true
		} else {
			d.BranchKnown = true
			d.BranchError = err.Error()
		}
	}

	return d
}
