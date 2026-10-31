package session

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Fallback committer identity for a workspace whose operator has none
// configured, so a first commit never dies on "Please tell me who you are".
const (
	defaultGitUserName  = "Agent Factory"
	defaultGitUserEmail = "af@agent-factory.local"
)

// operatorGitIdentity returns the committer identity git resolves for repoRoot
// on the daemon host — the operator's own user.name/user.email — so commits made
// off-box are attributed to them rather than to a placeholder. Either value is ""
// when unset; the caller falls back to the defaults (#5571 W2.7).
func operatorGitIdentity(repoRoot string) (name, email string) {
	read := func(key string) string {
		ctx, cancel := context.WithTimeout(context.Background(), dockerShortStepTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "config", "--get", key)
		cmd.WaitDelay = dockerWaitDelay
		out, err := cmd.Output()
		if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return read("user.name"), read("user.email")
}

// gitIdentityCommand writes the committer identity into the workspace repo's
// LOCAL config only. It never touches ~/.gitconfig and never sets
// safe.directory on the remote: the
// workspace is cloned by the user that runs af there, so neither is needed, and
// a host's own git configuration must be byte-identical before and after a
// session (#5571 W2.7). Linked worktrees share the repo's local config, so the
// agent-server's worktree inherits it.
func gitIdentityCommand(repoPath, name, email string) string {
	if strings.TrimSpace(name) == "" {
		name = defaultGitUserName
	}
	if strings.TrimSpace(email) == "" {
		email = defaultGitUserEmail
	}
	repo := shellQuote(repoPath)
	return "git -C " + repo + " config --local user.email " + shellQuote(email) +
		" && git -C " + repo + " config --local user.name " + shellQuote(name)
}
