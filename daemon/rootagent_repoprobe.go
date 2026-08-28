package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
)

// repoResolveClaim words what a failed config.RepoFromPath entitles a
// root-agent sweep to SAY about a candidate path. It exists as one function
// because both sweeps narrate the same repoErr, and before #3500 both said the
// same wrong thing: every resolution failure became "does not resolve to a git
// repository", including the ones where git never answered.
//
// "git answered, and the answer is no" and "we could not ask git" are different
// states, and only the first is a claim about the path. The second is a claim
// about a subprocess — killed, unstartable, or abandoned when its 100ms
// WaitDelay expired on a loaded box — and it establishes nothing about the
// configuration the reader is about to go and audit. The report that opened
// #3500 sent a maintainer to a root_agents entry naming a directory
// `git rev-parse --show-toplevel` resolves perfectly well.
//
// This is the rule #3371 (an exec failure fabricating a definite "no -N
// support") and #3478 (an unconfirmed teardown reported as confirmed) already
// applied in their own subsystems: never convert "could not establish" into a
// verdict. subject names the thing being resolved ("root_agents entry",
// "project <id> root") so one wording serves both sites.
func repoResolveClaim(subject, path string, err error) string {
	if errors.Is(err, config.ErrRepoProbeUnanswered) {
		return fmt.Sprintf("%s %q could not be checked: git never answered the probe (the subprocess was killed, could not be started, or was abandoned mid-read), so whether the path is a git repository is unknown", subject, path)
	}
	return fmt.Sprintf("%s %q does not resolve to a git repository", subject, path)
}
