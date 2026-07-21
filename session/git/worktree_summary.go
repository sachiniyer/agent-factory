package git

import (
	"fmt"
	"strings"
)

// WorkSummary is the state of a session's branch at a point in time: what has
// been committed on top of the base, and what is still uncommitted. It exists
// for the agent-handoff mission brief (#2013), which has to tell an incoming
// agent what its predecessor already did without replaying a transcript it
// cannot read.
//
// HeadSHA doubles as the handoff's attribution boundary. af cannot mark the
// commits themselves — the agent authors them and af writes no trailers — so
// recording the tip at swap time is what makes "the outgoing agent wrote up to
// here, the incoming one wrote after" a verifiable git range rather than a
// claim.
type WorkSummary struct {
	HeadSHA    string
	BaseSHA    string
	Branch     string
	Commits    int
	DirtyFiles int
}

// Empty reports whether the branch carries no work at all — nothing committed
// on top of the base and a clean tree. A handoff at that point is legitimate
// (the agent may have hit its limit before writing anything), and the brief
// says so plainly instead of pointing the new agent at an empty diff.
func (s WorkSummary) Empty() bool {
	return s.Commits == 0 && s.DirtyFiles == 0
}

// WorkSummary reports what has accumulated on this worktree's branch.
//
// Best-effort by construction: every field degrades independently, and a
// failure to count is never a failure to hand off. A brief that says "4 commits"
// is better than one that says nothing, and one that says nothing is still far
// better than refusing the swap because `git rev-list` had a bad day. The
// caller renders whatever it gets.
//
// The commit count is measured against the RECORDED base commit rather than a
// branch name: the base is what af pinned when it cut the worktree, so the range
// stays correct even after master moves on underneath a long-lived session.
func (g *GitWorktree) WorkSummary() (WorkSummary, error) {
	summary := WorkSummary{
		BaseSHA: strings.TrimSpace(g.baseCommitSHA),
		Branch:  strings.TrimSpace(g.branchName),
	}
	path := g.worktreePath
	if strings.TrimSpace(path) == "" {
		return summary, fmt.Errorf("worktree path is empty")
	}

	head, err := g.runGitCommand(path, "rev-parse", "HEAD")
	if err == nil {
		summary.HeadSHA = strings.TrimSpace(head)

		if summary.BaseSHA != "" {
			if out, cErr := g.runGitCommand(path, "rev-list", "--count", summary.BaseSHA+"..HEAD"); cErr == nil {
				if n, sErr := parseCount(out); sErr == nil {
					summary.Commits = n
				}
			}
		}
	} else {
		// No HEAD means an unborn branch (a worktree cut but never committed to).
		// That is a legitimate state to hand off from, but it can still carry
		// staged, unstaged, or untracked work. Continue to status instead of
		// returning a fabricated clean summary merely because commit history is
		// unavailable.
	}

	if out, sErr := g.runGitCommand(path, "status", "--porcelain"); sErr == nil {
		summary.DirtyFiles = countNonEmptyLines(out)
	}

	return summary, nil
}

func parseCount(out string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func countNonEmptyLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
