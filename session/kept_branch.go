package session

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// KeptBranch is what a delete leaves in the repo besides af's own refs (#6233).
// af never removes a ref it cannot prove it owns, so a branch the user checked
// out inside the worktree survives the delete, together with its commits. Every
// surface that confirms or reports a delete says so from this one fact, read
// from the worktree's HEAD at that moment — never from the recorded branch.
type KeptBranch struct {
	// Name is the branch the worktree's HEAD is on when af does not own it.
	Name string `json:"name,omitempty"`
	// Unreadable is set when the HEAD could not be read, so nothing is claimed.
	Unreadable bool `json:"unreadable,omitempty"`
}

// Any reports whether there is something to tell the user.
func (k KeptBranch) Any() bool { return k.Name != "" || k.Unreadable }

// Note is the sentence-case copy shared by the TUI, web and CLI.
func (k KeptBranch) Note() string {
	switch {
	case k.Name != "":
		return "Branch '" + k.Name + "' is checked out in the worktree and af does not own it · the branch and its commits are kept in the repo."
	case k.Unreadable:
		return "Could not read the worktree's current branch · a branch you created there, and its commits, would be kept in the repo."
	}
	return ""
}

const keptBranchGitTimeout = 5 * time.Second

// KeptBranchAtDelete reads the worktree's HEAD now and reports a branch af does
// not own that a delete would leave behind. Nothing to say for a session with no
// af-owned worktree (the checkout itself survives), a detached HEAD, or a HEAD on
// the branch af recorded (the existing owned/pre-existing copy covers it).
func (i *Instance) KeptBranchAtDelete() KeptBranch {
	impact, ok := i.GetWorktreeCleanupImpact()
	if !ok || !impact.RemoveWorktree || impact.Path == "" {
		return KeptBranch{}
	}
	return keptBranchFor(impact.Path, impact.Branch)
}

// KeptBranchAt is KeptBranchAtDelete for a bare worktree path and the branch af
// recorded for it, for callers that already hold an impact snapshot (the TUI).
func KeptBranchAt(path, recorded string) KeptBranch { return keptBranchFor(path, recorded) }

func keptBranchFor(path, recorded string) KeptBranch {
	ctx, cancel := context.WithTimeout(context.Background(), keptBranchGitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", path, "symbolic-ref", "--quiet", "HEAD").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return KeptBranch{} // detached HEAD: no branch to keep
		}
		return KeptBranch{Unreadable: true}
	}
	// The full ref, with exactly refs/heads/ stripped: --short can answer
	// heads/dev/beta when a tag shares the branch's name.
	head := strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/heads/")
	if head == "" || head == recorded {
		return KeptBranch{}
	}
	return KeptBranch{Name: head}
}
