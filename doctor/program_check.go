package doctor

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
)

// checkProgramCommand resolves command the way the session launch will,
// anchored to repoWorkspace when the doctor runs inside a git repository,
// mirroring the daemon-side #5779 fix (preflight.LocalSessionPrereqsAt takes
// repo.WorkspacePath()): a relative program_overrides path resolves against the
// session's launch directory, never the doctor's incidental cwd. The returned
// outsideRepo flag is true only when the doctor is genuinely outside any git
// repository (config.ErrNotGitRepository) AND the failed command's resolution
// depends on the working directory: there the launch directory cannot be
// determined, so today's preflight.CheckCommand (resolved against the doctor's
// own cwd) is kept and the caller reports any failure with an honest message
// instead of install or reinstall guidance (#6487). A cwd-independent command
// (a bare name resolved through PATH, or an absolute path) resolves the same
// way regardless of where the doctor was run, so a failure there is a genuine
// install/permission problem and keeps today's classified ProgramError
// remediation — the honest message is reserved for the relative paths the
// doctor can actually misresolve. A failed repo probe for any other reason
// (git missing from PATH, an unreadable .git, …) is not "outside a repo" — the
// doctor may be inside the session's repo but could not prove it, so today's
// CheckCommand is kept with its normal classified remediation.
func checkProgramCommand(repoWorkspace string, repoErr error, command string) (*preflight.ProgramCheck, error, bool) {
	if repoErr == nil {
		check, err := preflight.CheckCommandAt(command, repoWorkspace)
		return check, err, false
	}
	check, err := preflight.CheckCommand(command)
	if errors.Is(repoErr, config.ErrNotGitRepository) && commandDependsOnCwd(command) {
		return check, err, true
	}
	return check, err, false
}

// commandDependsOnCwd reports whether command's resolution depends on the
// working directory the doctor happened to be run from. Only a relative
// executable — a path containing a separator that is not absolute, e.g.
// "bin/claude" or "./venv/bin/claude" — is cwd-dependent: a bare name like
// "claude" resolves through PATH and an absolute path like "/usr/bin/codex"
// resolves the same way regardless of cwd. Leading VAR=value assignments and
// an `env` wrapper do not change the launched executable — preflight.CheckCommand
// resolves the agent past both — so commandToken skips them before the test,
// otherwise `FOO=x bin/claude` or `env bin/claude` would read only the first
// token and report cwd-independence while the resolved target is still relative
// (#6499). Outside a repo the doctor can only misresolve the cwd-dependent
// case, so the honest no-anchor message is reserved for it (#6487).
func commandDependsOnCwd(command string) bool {
	first := commandToken(command)
	if first == "" || filepath.IsAbs(first) {
		return false
	}
	return strings.ContainsAny(first, `/\`)
}

// commandToken returns the executable token command launches — the first shell
// word after any leading VAR=value assignments and an optional `env` wrapper,
// which may itself be followed by further assignments. It is a deliberately
// coarse split on whitespace, matching the resolution model the doctor already
// assumes for program_overrides; the untracked-relative check is advisory, not
// a correctness-critical gate, so quoted values with interior spaces are out
// of scope. Mirrors preflight.shellExecutable's leading-assignment skip.
func commandToken(command string) string {
	fields := strings.Fields(command)
	i := 0
	for i < len(fields) && isShellAssignment(fields[i]) {
		i++
	}
	if i < len(fields) && filepath.Base(fields[i]) == "env" {
		i++
		for i < len(fields) && isShellAssignment(fields[i]) {
			i++
		}
	}
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

// isShellAssignment reports whether word is a leading VAR=value shell
// assignment, the same shape preflight.command_parse skips before the
// executable. Mirrored here so doctor's advisory check stays self-contained.
func isShellAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for pos, r := range name {
		if pos == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
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

// relativeOverrideUntracked reports whether command is a relative path whose
// resolved executable is present in the main checkout but untracked — and so
// absent from the clean-checkout linked worktree a session launches in. Only
// cwd-dependent commands (relative paths with a separator) are affected: a
// bare name resolves through PATH and an absolute path resolves the same way
// in any worktree, so neither is sensitive to the clean-checkout difference.
// Returns false when the command is not cwd-dependent, the resolved path is
// not inside the workspace, or the tracked-status probe cannot answer — so a
// git failure never downgrades a PASS on its own. The repo-root anchor
// genuinely predicts the worktree for tracked relative paths, so the current
// PASS is kept only for those.
func relativeOverrideUntracked(workspace, command, resolvedPath string) bool {
	if workspace == "" || !commandDependsOnCwd(command) {
		return false
	}
	rel, err := filepath.Rel(workspace, resolvedPath)
	if err != nil || rel == "" || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false // resolved outside the repo; the worktree check does not apply
	}
	out, err := exec.Command("git", "-C", workspace, "ls-files", "--", rel).Output()
	if err != nil {
		return false // probe failed; be conservative and keep the PASS
	}
	return strings.TrimSpace(string(out)) == ""
}

// untrackedOverrideMessage is the advisory warning for a relative
// program_overrides path that resolves in the main checkout but is untracked:
// session worktrees are clean checkouts of the committed tree, so the override
// is absent at launch unless it is provisioned into each session (via
// setup_commands / the setup overlay, which applies after `git worktree add`).
// It names the two supported remedies (commit the file, or provision it in
// each session) rather than claiming the command is broken — the file does run
// in this checkout, it just will not be in the worktree the session launches in
// unless one of those remedies provides it.
func untrackedOverrideMessage(command, resolvedPath string) string {
	return fmt.Sprintf("%q resolves to %s in this checkout, but that path is untracked — "+
		"session worktrees are clean checkouts of the committed tree and will not contain it "+
		"unless setup_commands provisions it there, so the override will be absent at launch "+
		"without that provisioning", command, resolvedPath)
}

const untrackedOverrideRemediation = "commit the file so it is part of the tree the session checks out, " +
	"or provision it in each session via setup_commands or the setup overlay"
