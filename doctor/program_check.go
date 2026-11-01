package doctor

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
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
	// incomplete is true when the doctor could not determine whether it is
	// inside a git repository (the repo probe failed for a reason other than
	// "not inside a git repository"), so a relative command can be neither
	// anchored (CheckCommandAt needs a workspace) nor confirmed unanchored
	// (the outside-repo advisory presumes the doctor verified it is outside
	// any repo). probeErr carries the probe error so the report can name it
	// instead of claiming "not inside a git repository". Like advisory, the
	// doctor cannot verify the command, so it must NOT drive the exit code.
	incomplete bool
	probeErr   error
}

// checkProgramCommand resolves command the way the session launch will,
// anchored to repoWorkspace when the doctor runs inside a repository, mirroring
// the daemon-side #5779 fix: a relative program_overrides path resolves against
// the session's launch directory (the repo or a linked worktree), never the
// doctor's incidental cwd.
//
// When repoErr is config.ErrNotGitRepository the doctor is outside any git
// repository: a relative override genuinely has no anchor there, so it is
// reported as advisory rather than checked, since the doctor's own cwd is not
// the directory any session will launch in. Bare names, absolute paths, and
// tilde paths do not depend on the launch directory and are checked directly
// through CheckCommand.
//
// When repoErr is a different error the repo probe itself failed (git could not
// start, metadata was unreadable, …). The doctor cannot tell whether it is
// inside the session's repo, so it can neither anchor a relative command nor
// confirm there is no anchor. A relative command is reported as incomplete
// (carrying the probe error) rather than resolved against the doctor's
// incidental cwd (false pass/failure) or downgraded to the outside-repo
// advisory (which would claim the doctor confirmed it is outside any repo).
// Non-relative commands do not depend on the anchor and are still checked
// directly.
func checkProgramCommand(repoWorkspace string, repoErr error, command string) programCheckOutcome {
	if repoErr == nil {
		check, err := preflight.CheckCommandAt(command, repoWorkspace)
		return programCheckOutcome{check: check, err: err}
	}
	if errors.Is(repoErr, config.ErrNotGitRepository) {
		if preflight.RelativeExecutable(command) {
			return programCheckOutcome{advisory: true}
		}
		check, err := preflight.CheckCommand(command)
		return programCheckOutcome{check: check, err: err}
	}
	if preflight.RelativeExecutable(command) {
		return programCheckOutcome{incomplete: true, probeErr: repoErr}
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
		"determined — run `af doctor` from inside the session's repo to verify "+
		"it against the directory the session will launch in",
		command)
}

// relativeOverrideProbeFailedDetail is the incomplete detail for a relative
// program_overrides command when the doctor's git repo probe failed for a
// reason other than "not inside a git repository": the doctor cannot tell
// whether it is inside the session's repo, so it can neither anchor the
// command (CheckCommandAt) nor confirm there is no anchor (the outside-repo
// advisory). It names the probe error so the report does not claim the doctor
// confirmed it is outside any repo.
func relativeOverrideProbeFailedDetail(command string, probeErr error) string {
	return fmt.Sprintf("%q is a relative command but the doctor could not determine "+
		"whether it is inside a git repository (%v), so the launch directory it "+
		"would resolve against cannot be determined — fix the repo probe (for "+
		"example, run `af doctor` from inside the session's repo) and rerun to "+
		"verify it against the directory the session will launch in",
		command, probeErr)
}
