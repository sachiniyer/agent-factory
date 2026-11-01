package doctor

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/preflight"
)

// programCheckOutcome is the classified result of one doctor program preflight.
type programCheckOutcome struct {
	check *preflight.ProgramCheck
	err   error
	// advisory is true when the command could not be verified from the
	// doctor's invocation directory: a relative program_overrides command has
	// no anchor when the doctor runs outside any git repository, so resolving
	// it against the doctor's own cwd would produce both false failures (the
	// cwd lacks it) and false passes (a decoy cwd contains it). The finding is
	// advisory: it must NOT drive the exit code.
	advisory bool
}

// checkProgramCommand resolves command the way the session launch will,
// anchored to repoWorkspace when the doctor runs inside a repository, mirroring
// the daemon-side #5779 fix: a relative program_overrides path resolves against
// the session's launch directory (the repo or a linked worktree), never the
// doctor's incidental cwd.
//
// When repoWorkspace is empty the doctor is outside any git repository. A
// relative override genuinely has no anchor there, so it is reported as
// advisory rather than checked: the doctor's own cwd is not the directory any
// session will launch in. Bare names, absolute paths, and tilde paths do not
// depend on the launch directory and are checked directly through CheckCommand
// when outside a repo.
func checkProgramCommand(repoWorkspace, command string) programCheckOutcome {
	if repoWorkspace != "" {
		check, err := preflight.CheckCommandAt(command, repoWorkspace)
		return programCheckOutcome{check: check, err: err}
	}
	if preflight.RelativeExecutable(command) {
		return programCheckOutcome{advisory: true}
	}
	check, err := preflight.CheckCommand(command)
	return programCheckOutcome{check: check, err: err}
}

// relativeOverrideNoAnchorDetail is the advisory detail for a relative
// program_overrides command the doctor could not anchor against any session
// launch directory. The fragment starts with the command rather than the
// field/agent so it reads correctly whether the row name is the field (the
// setup path's "agent-program" finding) or the agent (the full sweep's
// per-agent row), neither of which should be repeated inside the detail.
func relativeOverrideNoAnchorDetail(command string) string {
	return fmt.Sprintf("%q is a relative command but `af doctor` is not inside a git "+
		"repository, so the launch directory it would resolve against cannot be "+
		"determined — run `af doctor` from inside the session's repo (or pass "+
		"--repo) to verify it against the directory the session will launch in",
		command)
}
