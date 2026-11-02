package doctor

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
)

// checkProgramCommand resolves command the way the session launch will,
// anchored to repoWorkspace when the doctor runs inside a git repository,
// mirroring the daemon-side #5779 fix (preflight.LocalSessionPrereqsAt takes
// repo.WorkspacePath()): a relative program_overrides path resolves against the
// session's launch directory, never the doctor's incidental cwd. The returned
// outsideRepo flag is true only when the doctor is genuinely outside any git
// repository (config.ErrNotGitRepository): there the launch directory cannot be
// determined, so today's preflight.CheckCommand (resolved against the doctor's
// own cwd) is kept and the caller reports any failure with an honest message
// instead of install or reinstall guidance (#6487). A failed repo probe for any
// other reason (git missing from PATH, an unreadable .git, …) is not "outside a
// repo" — the doctor may be inside the session's repo but could not prove it,
// so today's CheckCommand is kept with its normal classified remediation.
func checkProgramCommand(repoWorkspace string, repoErr error, command string) (*preflight.ProgramCheck, error, bool) {
	if repoErr == nil {
		check, err := preflight.CheckCommandAt(command, repoWorkspace)
		return check, err, false
	}
	check, err := preflight.CheckCommand(command)
	return check, err, errors.Is(repoErr, config.ErrNotGitRepository)
}

// noAnchorMessage is the honest failure message for a program check the doctor
// ran from outside any git repository: af doctor resolved the command against
// its own working directory, which is not the directory any session launches in,
// so the result is not authoritative for a relative program_overrides path. It
// points the user at the session's repo rather than at installing or
// reinstalling a binary (#6487).
func noAnchorMessage(command string) string {
	return fmt.Sprintf("%q did not resolve from this directory — `af doctor` is not inside a git repository, "+
		"so it checked the command against its own working directory, which is not where a session launches. "+
		"A relative program_overrides path resolves in each session's workspace; run `af doctor` from inside "+
		"the session's repo to verify it against the directory the session will launch in.", command)
}
