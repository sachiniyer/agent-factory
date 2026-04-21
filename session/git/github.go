package git

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// PRInfo holds information about a GitHub pull request associated with a branch.
type PRInfo struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

// FetchPRInfo runs `gh pr view` to look up a PR for the given branch.
// Returns nil with no error if no PR exists or gh is not installed.
func FetchPRInfo(repoPath, branchName string) (*PRInfo, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, nil
	}

	cmd := exec.Command("gh", "pr", "view", branchName, "--json", "number,title,url,state")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := string(exitErr.Stderr)
			if strings.Contains(stderr, "no pull requests found") ||
				strings.Contains(stderr, "Could not resolve to a PullRequest") {
				return nil, nil // genuinely no PR
			}
		}
		return nil, fmt.Errorf("failed to fetch PR info: %w", err)
	}

	return parsePRInfo(out)
}

// parsePRInfo parses the JSON output from `gh pr view`.
// Returns (nil, nil) only when the output clearly indicates no PR exists
// (i.e. PR number of 0). On malformed JSON it returns an error so that
// callers preserve previously cached PR info rather than clearing it.
func parsePRInfo(out []byte) (*PRInfo, error) {
	var info PRInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("failed to parse PR info JSON: %w", err)
	}
	if info.Number == 0 {
		return nil, nil
	}
	return &info, nil
}
