package api

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
)

// requireGitRepoWorkspace is the check a session command runs on a resolved
// workspace before it commits to using it, and the one place the answer is
// worded.
//
// It exists because the check has two failing outcomes that are not the same
// claim (#3504). git answering "this is not a repository" is a fact about the
// path and is reported as one. A probe that was killed, could not be started,
// or was abandoned mid-read establishes nothing about the path — the bool
// git.IsGitRepo returns cannot hold that difference, which is how a transient
// failure came to tell a user, in the moment, that the path they typed was bad.
func requireGitRepoWorkspace(workspace string) error {
	err := git.CheckGitRepo(workspace)
	switch {
	case err == nil:
		return nil
	case config.RepoProbeUnanswered(err):
		return fmt.Errorf("%s — retry: %w", config.RepoProbeUnansweredClaim("path", workspace), err)
	default:
		return fmt.Errorf("path %s is not a git repository", workspace)
	}
}
