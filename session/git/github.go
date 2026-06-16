package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ghNetworkTimeout bounds the `gh` API call in FetchPRInfo. Like the git fetch
// in resolveOriginHead (#896), this is a network operation with no timeout of
// its own: a stalled GitHub API request would otherwise block the sidebar's PR
// refresh goroutine indefinitely. gh runs as the leaf of its own short-lived
// invocation here, so a plain context deadline (no process-group teardown) is
// sufficient.
const ghNetworkTimeout = 30 * time.Second

// PRInfo holds information about a GitHub pull request associated with a branch.
type PRInfo struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

// FetchPRInfo runs `gh pr list --head <branch>` to look up a PR for the
// given branch. `--head` always treats its argument as a branch name, unlike
// `gh pr view <arg>` which disambiguates an all-numeric argument as a PR
// NUMBER — so a branch named "123" used to resolve to PR #123 (#740).
//
// `--state all` plus the open-first selection in parsePRList mirrors how
// `gh pr view <branch>` resolves a branch: prefer the open PR, but still
// surface a merged/closed one so the sidebar keeps showing a session's PR
// after it merges.
//
// It returns (nil, nil) — not an error — whenever there is no PR to look up.
// That covers three by-design cases:
//   - an empty branch name (a detached-HEAD worktree has no branch to query),
//   - the `gh` binary not being installed,
//   - no PR existing for the branch (`gh pr list` prints an empty array).
//
// A non-nil error is reserved for genuine failures (e.g. a transient `gh`
// error or malformed output) so callers can preserve previously cached PR
// info instead of clearing it.
func FetchPRInfo(repoPath, branchName string) (*PRInfo, error) {
	if branchName == "" {
		return nil, nil
	}

	if _, err := exec.LookPath("gh"); err != nil {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), ghNetworkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--head", branchName, "--state", "all",
		"--json", "number,title,url,state", "--limit", "10")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("gh pr list timed out after %s (GitHub unreachable or stalled): %w", ghNetworkTimeout, ctx.Err())
		}
		return nil, fmt.Errorf("failed to fetch PR info: %w", err)
	}

	return parsePRList(out)
}

// parsePRList parses the JSON array output from `gh pr list` and selects the
// PR to display: the first open PR if one exists, otherwise the first entry
// (gh orders by creation date, newest first). Returns (nil, nil) when the
// array is empty, i.e. the branch has no PR. On malformed JSON it returns an
// error so that callers preserve previously cached PR info rather than
// clearing it.
func parsePRList(out []byte) (*PRInfo, error) {
	var prs []PRInfo
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("failed to parse PR info JSON: %w", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	selected := prs[0]
	for _, pr := range prs {
		if strings.EqualFold(pr.State, "OPEN") {
			selected = pr
			break
		}
	}
	if selected.Number == 0 {
		return nil, nil
	}
	return &selected, nil
}
