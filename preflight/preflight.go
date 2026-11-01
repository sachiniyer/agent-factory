package preflight

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/envcommand"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Program-check outcome sentinels let ProgramError classify a CheckCommand
// failure by kind instead of string-matching. A missing binary is "not
// installed"; a present-but-unrunnable one needs chmod, not a reinstall —
// collapsing the two sends the user to fix the wrong thing (#2010).
var (
	// errProgramNotFound: the executable could not be located — no such file, or
	// absent from PATH.
	errProgramNotFound = errors.New("not installed or not on PATH")
	// errProgramNotExecutable: the executable exists but cannot be run (its
	// permission bits are clear).
	errProgramNotExecutable = errors.New("found but not executable")
)

// ProgramCheck describes the executable a configured agent command resolves to.
type ProgramCheck struct {
	Agent      string
	Command    string
	Executable string
	Path       string
}

// CheckTmux verifies that tmux is present and can report its version.
func CheckTmux() (string, error) {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return "", fmt.Errorf("tmux is not installed or not on PATH; install tmux (macOS: brew install tmux; Debian/Ubuntu: sudo apt install tmux) and retry")
	}
	out, err := exec.Command(path, "-V").Output()
	if err != nil {
		return "", fmt.Errorf("tmux is installed at %s but `tmux -V` failed: %w", path, err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		version = path
	}
	return version, nil
}

// CheckProgram resolves agent through cfg.program_overrides and verifies the
// command's executable exists. It does not run the command.
func CheckProgram(cfg *config.Config, agent string) (*ProgramCheck, error) {
	command := config.ResolveProgram(cfg, agent)
	check, err := CheckCommand(command)
	if check != nil {
		check.Agent = agent
	}
	if err != nil {
		return check, err
	}
	return check, nil
}

// CheckCommand verifies the shell executable and, when detectable, the agent it
// launches. It handles common shell shapes used in program_overrides: quotes,
// explicit paths, leading VAR=value assignments, env wrappers, and opaque
// wrappers whose own executable is the only command af can prove.
func CheckCommand(command string) (*ProgramCheck, error) {
	return checkCommand(command, "")
}

// CheckCommandAt verifies command in the same cwd and effective environment its
// tmux launch will use. Handoff calls this before stopping the outgoing agent,
// so wrappers, detected agent executables, relative paths, env -C, and PATH
// assignments must be resolved exactly as the incoming process will see them.
func CheckCommandAt(command, workingDir string) (*ProgramCheck, error) {
	return checkCommand(command, workingDir)
}

func checkCommand(command, workingDir string) (*ProgramCheck, error) {
	command = strings.TrimSpace(command)
	check := &ProgramCheck{Command: command}
	if command == "" {
		return check, fmt.Errorf("command is empty")
	}
	words, err := shellWords(command, 12)
	if err != nil {
		return check, err
	}
	shellExe := shellExecutable(words)
	if shellExe == "" {
		return check, fmt.Errorf("could not find an executable in command %q", command)
	}
	launchDir := workingDir
	if launchDir == "" {
		launchDir, err = os.Getwd()
		if err != nil {
			return check, fmt.Errorf("cannot determine launch directory for command preflight: %w", err)
		}
	}
	launch, err := tmux.CommandEnvironmentFromCommand(command, launchDir)
	if err != nil {
		return check, err
	}
	info, statErr := os.Stat(launch.WorkingDir)
	if statErr != nil {
		return check, fmt.Errorf("launch directory %q cannot be used: %w", launch.WorkingDir, statErr)
	}
	if !info.IsDir() {
		return check, fmt.Errorf("launch directory %q is not a directory", launch.WorkingDir)
	}
	exe := launch.Executable
	check.Executable = exe
	// The shell command and the detected agent are independent executable
	// obligations. For `ionice ... codex`, approving codex alone can still lose
	// the handoff to a missing ionice; approving ionice alone repeats the shipped
	// bug and discovers a missing codex only after teardown. Validate both, using
	// the shell prefix for the wrapper and the shared env/cwd model for the target.
	if shellExe != exe {
		if _, shellErr := resolveShellExecutableAt(words, shellExe, launchDir); shellErr != nil {
			kind := "command wrapper"
			if isEnvExecutable(shellExe) {
				kind = "env wrapper"
			}
			return check, fmt.Errorf("%s %q cannot be executed: %w", kind, shellExe, shellErr)
		}
	}
	pathValue, pathSet := os.LookupEnv("PATH")
	if override := launch.Override("PATH"); override.Present {
		pathValue, pathSet = override.Value, override.Set
	}
	path, err := resolveExecutableAt(exe, launch.WorkingDir, pathValue, pathSet)
	if err != nil {
		return check, err
	}
	check.Path = path
	return check, nil
}

// resolveShellExecutableAt models lookup of the command the shell itself starts.
// Only leading shell assignments affect that lookup; an env-internal PATH= value
// applies later to env's child, and an arbitrary wrapper's later operands cannot
// retroactively change how the wrapper was found.
func resolveShellExecutableAt(words []string, executable, workingDir string) (string, error) {
	pathValue, pathSet := os.LookupEnv("PATH")
	for _, word := range words {
		name, value, assignment := strings.Cut(word, "=")
		if !assignment || !isAssignment(word) {
			break
		}
		if name == "PATH" {
			if !envcommand.IsLiteral(value) {
				return "", fmt.Errorf("PATH uses shell expansion; use a literal value for launch preflight")
			}
			pathValue, pathSet = value, true
		}
	}
	return resolveExecutableAt(executable, workingDir, pathValue, pathSet)
}

func resolveExecutableAt(exe, workingDir, pathValue string, pathSet bool) (string, error) {
	if strings.ContainsRune(exe, filepath.Separator) || strings.HasPrefix(exe, "~") {
		path := config.ExpandTilde(exe)
		if !filepath.IsAbs(path) {
			path = filepath.Join(workingDir, path)
		}
		return resolveExecutable(path)
	}
	if !pathSet {
		return "", fmt.Errorf("%w: executable %q was not found because PATH is unset", errProgramNotFound, exe)
	}
	var notExecutable string
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = workingDir
		} else if !filepath.IsAbs(dir) {
			dir = filepath.Join(workingDir, dir)
		}
		candidate := filepath.Join(dir, exe)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			if notExecutable == "" {
				notExecutable = candidate
			}
			continue
		}
		return candidate, nil
	}
	if notExecutable != "" {
		return "", fmt.Errorf("%w: executable %q is not executable; run: %s", errProgramNotExecutable, exe,
			shellsuggest.Command("chmod", "+x", notExecutable))
	}
	return "", fmt.Errorf("%w: executable %q was not found on PATH", errProgramNotFound, exe)
}

func resolveExecutable(exe string) (string, error) {
	if strings.ContainsRune(exe, filepath.Separator) || strings.HasPrefix(exe, "~") {
		path := config.ExpandTilde(exe)
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("%w: executable %q does not exist", errProgramNotFound, exe)
			}
			return "", fmt.Errorf("executable %q could not be checked: %w", exe, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("executable %q is a directory, not a program", exe)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("%w: executable %q is not executable; run: %s", errProgramNotExecutable, exe, shellsuggest.Command("chmod", "+x", path))
		}
		return path, nil
	}
	path, err := exec.LookPath(exe)
	if err != nil {
		return "", fmt.Errorf("%w: executable %q was not found on PATH", errProgramNotFound, exe)
	}
	return path, nil
}

// LocalSessionPrereqs verifies the prerequisites needed before creating a
// local tmux-backed session. Remote hook sessions intentionally skip this.
func LocalSessionPrereqs(cfg *config.Config, agent string) error {
	if _, err := CheckTmux(); err != nil {
		return err
	}
	if _, err := CheckProgram(cfg, agent); err != nil {
		return ProgramError(agent, config.ResolveProgram(cfg, agent), err)
	}
	return nil
}

// LocalSessionPrereqsAt is LocalSessionPrereqs for a caller that is not the
// process the session launches from — the daemon validating a remote create. A
// relative program_overrides path resolves against the directory the launch
// uses (the session's repo or workspace), never the checking process's own
// working directory (#5779). An empty workingDir keeps LocalSessionPrereqs's
// behaviour.
func LocalSessionPrereqsAt(cfg *config.Config, agent, workingDir string) error {
	if workingDir == "" {
		return LocalSessionPrereqs(cfg, agent)
	}
	if _, err := CheckTmux(); err != nil {
		return err
	}
	command := config.ResolveProgram(cfg, agent)
	if _, err := CheckCommandAt(command, workingDir); err != nil {
		return ProgramError(agent, command, err)
	}
	return nil
}

// RelativeExecutable reports whether command's resolution depends on the
// launch working directory — so the doctor, which may run outside any git
// repository, cannot verify it without anchoring to the directory the session
// will launch in. The dependency arises from any of:
//   - a relative executable (./venv/bin/claude, bin/claude, env -C d ./codex),
//   - a relative shell wrapper that CheckCommand resolves separately from the
//     detected agent executable (./bin/launcher codex): the wrapper is looked up
//     against the launch directory even though the agent token is a bare name, or
//     a bare wrapper (env) looked up through a relative or empty shell-prefix
//     PATH entry (PATH=bin env /usr/bin/codex): resolveShellExecutableAt joins
//     such entries against the doctor's cwd, so the wrapper lookup flips with
//     it regardless of any chdir, or
//   - a relative or empty PATH entry used by a bare executable, whether the entry
//     is introduced by the command (PATH=bin codex, env PATH=bin codex) or
//     inherited from the doctor's own environment: resolveExecutableAt joins such
//     entries against the launch directory when looking up a bare name, so the
//     lookup flips with the doctor's incidental cwd. Absolute and relative-path
//     executables ignore PATH, so a relative PATH entry does not anchor them, or
//     a bare env operand looked up through env's PATH (env PATH=bin launcher
//     /usr/bin/codex): the operand is a separate executable CheckCommand does
//     not resolve, so a relative entry anchors it as it anchors a bare
//     launch.Executable. PATH resolution stops at the first matching entry, so
//     an absolute entry that contains the executable before a relative one does
//     not anchor the command — the relative entry is never consulted.
//   - a relative env -C chdir (env -C subdir codex, env -C ../../tools codex):
//     the launch working directory itself becomes cwd-relative, and the
//     existence check (plus any relative PATH resolution) flips with it. A
//     chdir with enough parent components to climb above the filesystem root is
//     detected directly rather than by probe-anchor comparison.
//
// Absolute executables, tilde paths, absolute PATH entries, and absolute chdirs
// resolve the same way regardless of the launch directory, so the doctor
// outside any repo may still verify them directly. In particular, an absolute
// env -C chdir fixes the launch working directory independent of the doctor's
// cwd, so a relative executable that resolves against it (env -C /opt ./codex
// → /opt/codex) is anchor-independent and CheckCommand can verify it. The
// detected context is resolved through tmux.CommandEnvironmentFromCommand, so
// the same env-wrapper and opaque-wrapper handling as the launch path applies.
// The classification is conservative: a command that cannot be parsed
// (unterminated quotes, unknown env options) returns false so CheckCommand
// surfaces the real error rather than doctor emitting a different advisory for
// it.
func RelativeExecutable(command string) bool {
	launch, err := tmux.CommandEnvironmentFromCommand(command, relativeExecutableAnchorA)
	if err != nil {
		return false
	}
	// A command shellWords cannot parse (unterminated quote) is one
	// CheckCommand reports as malformed. Classify it as anchor-independent so
	// the doctor runs CheckCommand and surfaces that error instead of
	// downgrading a relative override to a non-problem "unverified" advisory
	// (CommandEnvironmentFromCommand's tokenizer tolerates unclosed quotes, so
	// the launch parse above does not catch it).
	if _, err := shellWords(command, 0); err != nil {
		return false
	}
	// The shell wrapper (the first non-assignment token, the same token
	// CheckCommand resolves via resolveShellExecutableAt) is looked up against
	// the doctor's own cwd (launchDir), not against launch.WorkingDir, so an
	// env -C chdir does not anchor it. A relative wrapper therefore anchors
	// the command regardless of chdir even when the detected agent executable
	// is a bare name. A bare wrapper is looked up through the shell-prefix PATH
	// (leading assignments, then the doctor's inherited PATH), which
	// resolveShellExecutableAt joins against the doctor's cwd, so a relative or
	// empty PATH entry anchors it too — `PATH=bin env /usr/bin/codex` resolves
	// env from <doctor-cwd>/bin even though the agent token is an absolute path
	// a chdir would otherwise fix.
	if wrapper := relativeShellWrapper(command); wrapper != "" && wrapper != launch.Executable {
		if usesRelativeExecutable(wrapper) {
			return true
		}
		if isBareExecutable(wrapper) {
			if pathValue, pathSet := shellWrapperPathValue(command); pathSet && pathHasRelativeEntry(wrapper, pathValue) {
				return true
			}
		}
	}
	// GNU env's command operand (the token env execs) is a separate executable
	// the session launches from the working directory, but CheckCommand does
	// not resolve it when a supported agent token is nested behind it
	// (`env ./bin/launcher codex` resolves env and codex, not the intervening
	// ./bin/launcher). A relative operand therefore anchors the command
	// regardless of chdir, since CheckCommand never verifies it. A bare
	// operand is looked up through env's PATH against launch.WorkingDir, so a
	// relative or empty PATH entry anchors it just as it anchors a bare
	// launch.Executable (`env PATH=bin launcher /usr/bin/codex` resolves env
	// and the absolute codex but never launcher, whose lookup flips with the
	// launch directory through PATH=bin); an absolute chdir fixes that
	// lookup dir, so the bare-operand PATH check is gated on the working
	// directory resolving against the doctor's cwd, like launch.Executable.
	// The working directory in effect is the one the parent env execs the
	// operand in (the parent envs' chdirs, not the operand's own env -C,
	// which applies to its command), so the gate is per-operand: a nested
	// env looked up through a relative PATH under a relative outer chdir
	// that a later inner chdir resets to absolute is still gated by the
	// relative outer working directory and anchors the command.
	// The PATH active when the operand is resolved is the environment the
	// parent env execs it in: each env wrapper applies its own PATH
	// assignments before running its command operand, so a nested env
	// (`env PATH=bin env PATH=/usr/bin /usr/bin/codex`) is looked up through
	// the outer env's relative PATH=bin even though the final launch PATH is
	// /usr/bin — using the final PATH would let the doctor verify /usr/bin/env
	// and falsely pass while the session resolves the inner env from
	// <launch-dir>/bin. envCommandOperand therefore reports the PATH in
	// effect at the operand alongside the operand itself. Every nested env
	// reached by unwrapping is collected, and the walk continues past an
	// anchor-independent nested env (an absolute /usr/bin/env) to classify
	// the operands it in turn execs: a relative command operand hidden
	// behind an absolute nested env (`env /usr/bin/env ./bin/launcher
	// /usr/bin/codex` runs ./bin/launcher, which CheckCommand never
	// verifies) is otherwise missed and the doctor can PASS while the
	// session cannot start.
	for _, op := range envCommandOperand(command) {
		if op.operand == "" || op.operand == launch.Executable {
			continue
		}
		if usesRelativeExecutable(op.operand) {
			return true
		}
		if op.workingDirResolvesAgainstCwd && isBareExecutable(op.operand) {
			if op.pathSet && pathHasRelativeEntry(op.operand, op.pathValue) {
				return true
			}
		}
	}
	// The remaining resolutions — launch.Executable, a bare executable's PATH
	// lookup, and the existence check on launch.WorkingDir — all go through
	// launch.WorkingDir. An absolute env -C chdir fixes WorkingDir independent
	// of the doctor's cwd, so those resolutions are anchor-independent and
	// CheckCommand can verify them directly. Only when WorkingDir itself
	// resolves against the doctor's cwd (no chdir, or a relative chdir) can
	// they flip with the doctor's invocation directory.
	if workingDirResolvesAgainstCwd(command, launch) {
		if usesRelativeExecutable(launch.Executable) {
			return true
		}
		if isBareExecutable(launch.Executable) {
			if path, ok := effectivePathValue(launch); ok && pathHasRelativeEntry(launch.Executable, path) {
				return true
			}
		}
	}
	// A relative env -C chdir makes launch.WorkingDir itself depend on the
	// doctor's cwd: CheckCommand stats it before resolving the executable, and
	// that stat flips with the doctor's invocation directory even for an
	// absolute executable with an absolute PATH. No chdir means WorkingDir is
	// the doctor's own cwd, which always exists, so it does not add a
	// dependency; an absolute chdir fixes it independent of the cwd.
	return relativeChdir(command, launch)
}

// relativeExecutableAnchorA and relativeExecutableAnchorB are two distinct
// absolute working directories used to detect a relative env -C chdir by
// comparison. They need not exist on disk; CommandEnvironmentFromCommand
// never stats them. Their parents differ, so a single-level parent-relative
// chdir such as `env -C ../tools` resolves to distinct paths under each anchor
// instead of collapsing to the same root-relative path. A chdir with enough
// parent components to climb above both anchors (env -C ../../tools) collapses
// the comparison; relativeChdirOperand backs that case up by detecting a
// relative chdir directly.
const (
	relativeExecutableAnchorA = "/__af-relative-executable-anchor-a__"
	relativeExecutableAnchorB = "/__af-relative-executable-anchor-b__/probe"
)

// relativeShellWrapper returns the shell-wrapper token CheckCommand resolves
// separately from the detected agent executable: the first non-assignment
// token of the command, the same token CheckCommand feeds to
// resolveShellExecutableAt. An empty string means the command could not be
// tokenized; a value equal to the agent executable means there is no separate
// wrapper (e.g. `PATH=bin codex`).
func relativeShellWrapper(command string) string {
	words, err := shellWords(command, 12)
	if err != nil {
		return ""
	}
	return shellExecutable(words)
}

// envOperand is a single executable token GNU env execs (env's command
// operand or a nested env reached by unwrapping), paired with the PATH in
// effect when that token is resolved and whether the working directory in
// effect at that resolution flips with the doctor's cwd. A bare operand is
// looked up through env's PATH against the working directory its parent env
// execs it in — the directory after the parent env wrappers' chdirs but
// before this operand's own env -C chdir (a chdir applies to env's command,
// not to env itself) — so the cwd-dependence of that lookup turns on the
// parent envs' chdirs, not the final launch working directory. An absolute
// chdir among the parent envs fixes that directory independent of the
// doctor's cwd; no chdir, a cwd-neutral chdir, or an all-relative chain
// leaves it resolving against the doctor's cwd. The final command operand's
// working directory is the accumulation of every env wrapper's chdir, so its
// flag matches workingDirResolvesAgainstCwd(command, launch).
type envOperand struct {
	operand                      string
	pathValue                    string
	pathSet                      bool
	workingDirResolvesAgainstCwd bool
}

// envCommandOperand returns the executable tokens GNU env execs (env's
// command operand chain) when the command begins with one or more env
// wrappers, in the order the session execs them, or nil if the command has
// no env wrapper or the wrapper's command cannot be found. af models env as
// exec'ing its command operand; when a supported agent token is nested
// behind that operand (`env ./bin/launcher codex`), CheckCommand resolves
// env (the shell wrapper) and codex (the detected agent) but not the
// intervening operand, so a relative operand is a launch-directory-dependent
// executable the doctor cannot verify from outside a repo. Leading shell
// assignments are skipped. The leading env wrapper is unwrapped to its
// command operand; a nested env reached by unwrapping is itself an
// executable obligation the session launches but CheckCommand does not
// resolve (`env ./bin/env /usr/bin/codex` runs ./bin/env, which CheckCommand
// never verifies), so it is collected for classification rather than
// unwrapped silently to the final agent token. The full command is
// tokenized (no word cap) so a long env prefix of literal assignments
// cannot truncate before env's command operand and leave a relative
// launcher undetected.
//
// The walk continues past an anchor-independent nested env (an absolute
// /usr/bin/env, or a bare env looked up through an absolute PATH): such a
// wrapper is itself verified by the launch anchor, but the operand it in
// turn execs is still a separate executable CheckCommand does not resolve,
// so a relative operand hidden behind it (`env /usr/bin/env
// ./bin/launcher /usr/bin/codex` runs ./bin/launcher) is collected too
// rather than the walk stopping at the first nested wrapper.
//
// The returned pathValue/pathSet is the PATH in effect when the returned
// operand is resolved. A bare operand is looked up through the environment
// its parent env execs it in, which is the inherited PATH after leading
// shell assignments and the parent env's own PATH mutations are applied —
// not the final launch PATH, which a later nested env may have changed
// (`env PATH=bin env PATH=/usr/bin /usr/bin/codex` looks up the inner env
// through the outer env's relative PATH=bin). For a single env's command
// operand (or a non-env first token) this equals effectivePathValue(launch);
// for a nested env reached by unwrapping it is the parent env's PATH, so a
// relative entry there anchors the bare nested operand.
//
// The returned workingDirResolvesAgainstCwd flag is the cwd-dependence of the
// working directory in effect when the operand is resolved. A bare operand's
// PATH lookup happens against the working directory its parent env execs it
// in — the accumulation of the chdirs of the env wrappers up to but not
// including the operand's own env -C — so an absolute chdir among those
// parent envs fixes the lookup directory and the flag is false, while no
// chdir, a cwd-neutral chdir, or an all-relative chain leaves it resolving
// against the doctor's cwd and the flag is true. The final command
// operand's working directory accumulates every env wrapper's chdir, so its
// flag matches workingDirResolvesAgainstCwd(command, launch); without this
// per-operand flag, a nested env looked up through a relative PATH under a
// relative outer chdir that a later inner chdir resets to absolute
// (`env -C rel PATH=bin env -C /abs PATH=/usr/bin /usr/bin/codex`) would be
// gated by the final absolute working directory and the doctor would
// falsely PASS while the session resolves the inner env from
// <launch-dir>/rel/bin.
func envCommandOperand(command string) []envOperand {
	words, err := shellWords(command, 0)
	if err != nil {
		return nil
	}
	var ops []envOperand
	pathValue, pathSet := os.LookupEnv("PATH")
	idx := 0
	for idx < len(words) && isAssignment(words[idx]) {
		pathValue, pathSet = applyPathAssignment(words[idx], pathValue, pathSet)
		idx++
	}
	// chdirs accumulates the -C/--chdir operands of the env wrappers
	// processed so far, in order. workingDirResolvesAgainstCwd reports
	// whether the working directory these chdirs establish — the directory
	// a bare operand is looked up through — flips with the doctor's cwd:
	// an absolute chdir fixes it (false), and no chdir, a cwd-neutral
	// chdir, or an all-relative chain resolves against the doctor's cwd
	// (true). It is evaluated before the current env's own chdir is
	// applied, since env -C changes the directory for env's command, not
	// for env itself.
	var chdirs []string
	workingDirResolvesAgainstCwd := func() bool {
		for _, c := range chdirs {
			if filepath.IsAbs(c) {
				return false
			}
		}
		return true
	}
	unwrapped := false
	for idx < len(words) && isEnvExecutable(words[idx]) {
		// A nested env reached by unwrapping (not the leading shell
		// wrapper, which CheckCommand resolves via resolveShellExecutableAt)
		// is env's command operand — an executable the session execs but
		// CheckCommand does not resolve. Collect it for classification so
		// a relative nested env (env ./bin/env /usr/bin/codex) or a bare
		// nested env looked up through a relative PATH anchors the command
		// instead of being silently unwrapped to the final agent token.
		// pathValue/pathSet already holds the parent env's environment,
		// which is the PATH the nested env is looked up through, and
		// workingDirResolvesAgainstCwd() reports the cwd-dependence of
		// the parent env's working directory, which is where a bare
		// nested env's PATH lookup happens. The walk then continues
		// through the nested env to collect the operands it execs, so a
		// relative operand behind an anchor-independent nested env is
		// not missed.
		if unwrapped {
			ops = append(ops, envOperand{words[idx], pathValue, pathSet, workingDirResolvesAgainstCwd()})
		}
		invocation, perr := envcommand.Parse(words[idx+1:], envcommand.Policy{AllowAssignments: true})
		if perr != nil || invocation.CommandIndex < 0 {
			return ops
		}
		// Apply this env's mutations to obtain the environment it execs
		// its command operand in; the operand is looked up through that
		// environment, so a nested env reached on the next iteration is
		// classified against this PATH rather than the final launch PATH.
		pathValue, pathSet = applyEnvPathMutations(invocation, pathValue, pathSet)
		if invocation.Chdir != "" {
			chdirs = append(chdirs, invocation.Chdir)
		}
		idx += 1 + invocation.CommandIndex
		unwrapped = true
	}
	if unwrapped && idx < len(words) {
		ops = append(ops, envOperand{words[idx], pathValue, pathSet, workingDirResolvesAgainstCwd()})
	}
	return ops
}

// applyPathAssignment applies a leading shell PATH assignment to the inherited
// (pathValue, pathSet) state envCommandOperand tracks. Shell assignments reach
// RelativeExecutable only after CommandEnvironmentFromCommand has accepted
// them, so a non-literal value would have turned the whole classification
// false earlier; the IsLiteral guard keeps this helper self-consistent if it
// is ever reused on an unvetted command.
func applyPathAssignment(word, pathValue string, pathSet bool) (string, bool) {
	name, value, assignment := strings.Cut(word, "=")
	if !assignment || !isAssignment(word) || name != "PATH" {
		return pathValue, pathSet
	}
	if !envcommand.IsLiteral(value) {
		return "", false
	}
	return value, true
}

// applyEnvPathMutations applies an env invocation's PATH-affecting mutations to
// the tracked (pathValue, pathSet) state: --ignore-environment/-i and -u PATH
// clear PATH, and a PATH assignment sets it. envcommand.Parse rejects dynamic
// values, so mutation values are literal.
func applyEnvPathMutations(inv envcommand.Invocation, pathValue string, pathSet bool) (string, bool) {
	if inv.ClearEnvironment {
		pathValue, pathSet = "", false
	}
	for _, m := range inv.Mutations {
		if m.Name != "PATH" {
			continue
		}
		if m.Unset {
			pathValue, pathSet = "", false
		} else {
			pathValue, pathSet = m.Value, true
		}
	}
	return pathValue, pathSet
}

// workingDirResolvesAgainstCwd reports whether launch.WorkingDir is resolved
// against the doctor's cwd, and so a relative executable or a bare executable
// with a relative PATH entry resolved through it flips with where af doctor
// runs. It is true when no env -C chdir was applied (WorkingDir is the doctor's
// own cwd) or when a relative chdir was applied (WorkingDir is join(cwd,
// chdir)); an absolute chdir fixes WorkingDir independent of the cwd and
// returns false. The anchor comparison misses a relative chdir whose parent
// components climb above both probe anchors to the filesystem root — both
// WorkingDirs collapse to the same root-relative path even though a real
// launch resolves the chdir against the doctor's cwd — so a direct relative
// chdir check backs it up.
func workingDirResolvesAgainstCwd(command string, launch tmux.CommandEnvironment) bool {
	if launch.WorkingDir == relativeExecutableAnchorA {
		return true
	}
	other, err := tmux.CommandEnvironmentFromCommand(command, relativeExecutableAnchorB)
	if err != nil {
		return false
	}
	if other.WorkingDir != launch.WorkingDir {
		return true
	}
	return relativeChdirOperand(command)
}

// relativeChdir reports whether an env -C chdir made launch.WorkingDir itself
// depend on the doctor's cwd: CheckCommand stats WorkingDir before resolving
// the executable, and that stat flips with the doctor's invocation directory
// even for an otherwise anchor-independent executable. No chdir (WorkingDir is
// the doctor's own cwd, which always exists) and an absolute chdir both return
// false; only a relative chdir returns true. The anchor comparison misses a
// parent-relative chdir that collapses both probe anchors (env -C ../../tools),
// and a chain of relative chdirs that cancels back to the anchor (env -C rel
// env -C .. /bin/true), so a direct relative chdir check backs both up. A
// cwd-neutral chdir such as -C . does not move the directory and is excluded,
// so it falls through to the anchor-equality short-circuit.
func relativeChdir(command string, launch tmux.CommandEnvironment) bool {
	if launch.WorkingDir == relativeExecutableAnchorA && !relativeChdirOperand(command) {
		return false
	}
	other, err := tmux.CommandEnvironmentFromCommand(command, relativeExecutableAnchorB)
	if err != nil {
		return false
	}
	if other.WorkingDir != launch.WorkingDir {
		return true
	}
	return relativeChdirOperand(command)
}

// relativeChdirOperand reports whether command's env prefix applies a relative
// -C/--chdir that actually moves the working directory before any absolute
// chdir resets it. Such a chain resolves launch.WorkingDir against the
// doctor's cwd (join(cwd, chdir)) even when the two probe anchors collapse to
// the same root-relative path, which the anchor comparison in
// workingDirResolvesAgainstCwd and relativeChdir cannot distinguish from an
// absolute chdir. A cwd-neutral chdir (env -C . codex) does not move the
// directory, so it does not anchor the command and is skipped. An absolute
// chdir fixes the working directory for operands after it, but a moving
// relative chdir earlier in the chain already anchored the command: reaching
// the absolute chdir requires first entering the relative directory from the
// doctor's cwd (env -C rel env -C /tmp /bin/true must enter <launch-dir>/rel
// before the inner -C /tmp takes effect), so the dependency is retained
// rather than discarded. Relative chdirs after the first absolute chdir
// resolve against it, not the doctor's cwd, so they do not anchor.
func relativeChdirOperand(command string) bool {
	chdirs := envChdirs(command)
	if len(chdirs) == 0 {
		return false
	}
	hasMoving := false
	for _, c := range chdirs {
		if filepath.IsAbs(c) {
			return hasMoving
		}
		if filepath.Clean(c) == "." {
			continue
		}
		hasMoving = true
	}
	return hasMoving
}

// envChdirs returns the ordered -C/--chdir operands from the GNU env wrappers
// in command's prefix (leading shell assignments, then nested env wrappers),
// mirroring the prefix walk tmux.CommandEnvironmentFromCommand performs before
// the first opaque token. It returns nil when command cannot be tokenized or an
// env invocation af cannot model, so RelativeExecutable defers to CheckCommand.
func envChdirs(command string) []string {
	words, err := shellWords(command, 0)
	if err != nil {
		return nil
	}
	idx := 0
	for idx < len(words) && isAssignment(words[idx]) {
		idx++
	}
	var chdirs []string
	for idx < len(words) && isEnvExecutable(words[idx]) {
		invocation, perr := envcommand.Parse(words[idx+1:], envcommand.Policy{AllowAssignments: true})
		if perr != nil || invocation.CommandIndex < 0 {
			return chdirs
		}
		if invocation.Chdir != "" {
			chdirs = append(chdirs, invocation.Chdir)
		}
		idx += 1 + invocation.CommandIndex
	}
	return chdirs
}

// shellWrapperPathValue returns the PATH value resolveShellExecutableAt uses to
// look up the shell wrapper — the value established by leading PATH
// assignments, or the doctor's inherited PATH when no assignment overrides it
// — and whether PATH is set. It mirrors the leading-assignment scan in
// resolveShellExecutableAt so a bare wrapper's cwd-dependence classification
// matches the lookup CheckCommand performs.
func shellWrapperPathValue(command string) (string, bool) {
	words, err := shellWords(command, 0)
	if err != nil {
		return "", false
	}
	pathValue, pathSet := os.LookupEnv("PATH")
	for _, word := range words {
		name, value, assignment := strings.Cut(word, "=")
		if !assignment || !isAssignment(word) {
			break
		}
		if name == "PATH" {
			if !envcommand.IsLiteral(value) {
				return "", false
			}
			pathValue, pathSet = value, true
		}
	}
	return pathValue, pathSet
}

// isBareExecutable reports whether exe is a bare name resolved through PATH
// (no path separator, not a tilde, non-empty) — the one executable shape whose
// lookup consults PATH and therefore flips with a relative PATH entry.
func isBareExecutable(exe string) bool {
	if exe == "" || strings.HasPrefix(exe, "~") {
		return false
	}
	return !strings.ContainsRune(exe, filepath.Separator)
}

// effectivePathValue returns the PATH value that resolveExecutableAt would use
// for launch, and whether PATH is set. A command-introduced override (set or
// unset, including env -i) replaces the inherited value; otherwise the doctor's
// inherited PATH is what the launch reads.
func effectivePathValue(launch tmux.CommandEnvironment) (string, bool) {
	if override := launch.Override("PATH"); override.Present {
		return override.Value, override.Set
	}
	return os.LookupEnv("PATH")
}

// pathHasRelativeEntry reports whether pathValue contains a PATH entry that
// resolveExecutableAt resolves against the launch directory when looking up
// the bare executable exe: an empty entry (which becomes the launch
// directory) or a non-absolute entry (which is joined with it). PATH
// resolution stops at the first matching entry, so an absolute entry that
// already contains exe before any relative entry makes the lookup
// anchor-independent — the relative entry is never consulted and the doctor
// can verify the command directly (`PATH=/opt/agents:bin codex` with
// /opt/agents/codex present resolves at the absolute entry and does not flip
// with the launch directory). Absolute entries are statted against the
// filesystem (they are the same path in any cwd); relative entries are not,
// because the launch directory is the very thing the doctor outside a repo
// does not know. A relative entry preceding a resolving absolute match still
// anchors the command (it may resolve first in the real launch cwd); when no
// entry resolves, a prior relative entry is treated as cwd-dependent (it may
// resolve in the real launch directory) and a PATH with no relative entry is
// not.
func pathHasRelativeEntry(exe, pathValue string) bool {
	seenRelative := false
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" || !filepath.IsAbs(dir) {
			seenRelative = true
			continue
		}
		if resolvesAbsolute(dir, exe) {
			return seenRelative
		}
	}
	return seenRelative
}

// resolvesAbsolute reports whether dir/exe is a present, executable file — the
// condition under which resolveExecutableAt stops a bare executable's PATH
// walk at this entry. dir must be absolute; relative entries are
// cwd-dependent and handled by pathHasRelativeEntry's seenRelative flag.
func resolvesAbsolute(dir, exe string) bool {
	info, err := os.Stat(filepath.Join(dir, exe))
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// usesRelativeExecutable mirrors the relative-path branch of
// resolveExecutableAt: a token with a path separator that is not absolute is
// the one executable shape whose resolution depends on the launch directory.
// Bare names depend on PATH entries' relativity (handled by RelativeExecutable
// via the PATH override); a tilde expands to an absolute home only for "~" and
// "~/..." (config.ExpandTilde leaves "~user/..." unchanged), and the unchanged
// form is a relative path resolveExecutableAt joins against the working
// directory, so the tilde exemption applies only after expansion actually
// yields an absolute path; absolute paths ignore the working directory entirely.
func usesRelativeExecutable(exe string) bool {
	if exe == "" {
		return false
	}
	if !strings.ContainsRune(exe, filepath.Separator) {
		return false
	}
	return !filepath.IsAbs(config.ExpandTilde(exe))
}

// ProgramError renders the actionable user-facing error for an agent that
// failed its preflight check. It classifies the cause so the lead line points
// at the real fix: a missing binary says "not installed or not on PATH" (fix:
// install), a present-but-non-executable one says "found but not executable"
// with a chmod hint (fix: chmod +x, NOT reinstall), and anything else reports
// the actual error rather than a misleading not-installed message (#2010).
func ProgramError(agent, command string, cause error) error {
	name := agentDisplayName(agent)
	supported := isSupportedAgent(agent)

	// subject and remediation are the supported/unsupported dimension, which is
	// orthogonal to the failure kind below.
	subject := name
	remediation := fmt.Sprintf("set program_overrides.%s in ~/.agent-factory/config.toml", agent)
	if !supported {
		subject = fmt.Sprintf("program %q", agent)
		remediation = "fix the command in config"
	}

	switch {
	case isNotExecutable(cause):
		return fmt.Errorf("%s was found but is not executable (resolved command: %q). Make it executable (chmod +x), choose another agent, or %s. Details: %v",
			subject, command, remediation, cause)
	case isNotFound(cause):
		installWord := name
		if !supported {
			installWord = "it"
		}
		return fmt.Errorf("%s is not installed or not on PATH (resolved command: %q). Install %s, choose another agent, or %s. Details: %v",
			subject, command, installWord, remediation, cause)
	default:
		return fmt.Errorf("%s could not be started (resolved command: %q). Choose another agent, or %s. Details: %v",
			subject, command, remediation, cause)
	}
}

// isNotExecutable reports whether cause is a present-but-unrunnable binary — a
// permission problem that chmod, not a reinstall, fixes.
func isNotExecutable(cause error) bool {
	return errors.Is(cause, errProgramNotExecutable) || errors.Is(cause, fs.ErrPermission)
}

// isNotFound reports whether cause is a genuinely-absent binary, whether from a
// resolved path that does not exist or a bare name absent from PATH.
func isNotFound(cause error) bool {
	return errors.Is(cause, errProgramNotFound) ||
		errors.Is(cause, fs.ErrNotExist) ||
		errors.Is(cause, exec.ErrNotFound)
}

func agentDisplayName(agent string) string {
	switch agent {
	case tmux.ProgramClaude:
		return "Claude Code"
	case tmux.ProgramCodex:
		return "Codex"
	case tmux.ProgramAider:
		return "Aider"
	case tmux.ProgramGemini:
		return "Gemini"
	case tmux.ProgramAmp:
		return "Amp"
	case tmux.ProgramOpencode:
		// Lowercase on purpose: the project styles itself "opencode", and its
		// binary and docs render it that way. Listed explicitly even though the
		// default arm would return the same string, so this reads as a decision
		// rather than an omission a later reader "fixes" to "OpenCode".
		return "opencode"
	case tmux.ProgramDevin:
		return "Devin"
	default:
		if agent == "" {
			return "the configured agent"
		}
		return agent
	}
}

func isSupportedAgent(agent string) bool {
	for _, p := range tmux.SupportedPrograms {
		if agent == p {
			return true
		}
	}
	return false
}

func shellWords(input string, limit int) ([]string, error) {
	var words []string
	var b strings.Builder
	var quote rune
	escaped := false
	inWord := false

	flush := func() {
		if inWord {
			words = append(words, b.String())
			b.Reset()
			inWord = false
		}
	}

	for _, r := range input {
		if escaped {
			b.WriteRune(r)
			inWord = true
			escaped = false
			continue
		}
		switch quote {
		case '\'':
			if r == '\'' {
				quote = 0
			} else {
				b.WriteRune(r)
				inWord = true
			}
			continue
		case '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				b.WriteRune(r)
				inWord = true
			}
			continue
		}

		switch {
		case unicode.IsSpace(r):
			flush()
			if limit > 0 && len(words) >= limit {
				return words, nil
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '\\':
			escaped = true
		default:
			b.WriteRune(r)
			inWord = true
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in command %q", input)
	}
	flush()
	return words, nil
}
