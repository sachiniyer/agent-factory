package git

import (
	"fmt"
	"os/exec"
)

// runGitCommand executes a git command and returns any error.
// Only stdout is returned on success so callers parsing the output (e.g. SHAs
// or porcelain status) are not corrupted by warnings git emits on stderr.
// On error, stderr is folded into the returned error for diagnostics.
func (g *GitWorktree) runGitCommand(path string, args ...string) (string, error) {
	baseArgs := []string{"-C", path}
	cmd := exec.Command("git", append(baseArgs, args...)...)

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(output), fmt.Errorf("git command failed: %s (%w)", string(exitErr.Stderr), err)
		}
		return string(output), fmt.Errorf("git command failed: %w", err)
	}

	return string(output), nil
}
