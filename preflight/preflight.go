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
//   - a relative or empty PATH entry used by a bare executable, whether the entry
//     is introduced by the command (PATH=bin codex, env PATH=bin codex) or
//     inherited from the doctor's own environment: resolveExecutableAt joins such
//     entries against the launch directory when looking up a bare name, so the
//     lookup flips with the doctor's incidental cwd. Absolute and relative-path
//     executables ignore PATH, so a relative PATH entry does not anchor them, or
//   - a relative env -C chdir (env -C subdir codex, env -C ../tools codex): the
//     launch working directory itself becomes cwd-relative, and the existence
//     check (plus any relative PATH resolution) flips with it.
//
// Absolute executables, tilde paths, absolute PATH entries, and absolute chdirs
// resolve the same way regardless of the launch directory, so the doctor
// outside any repo may still verify them directly. The detected context is
// resolved through tmux.CommandEnvironmentFromCommand, so the same env-wrapper
// and opaque-wrapper handling as the launch path applies. The classification is
// conservative: a command that cannot be parsed (unterminated quotes, unknown
// env options) returns false so CheckCommand surfaces the real error rather
// than doctor emitting a different advisory for it.
func RelativeExecutable(command string) bool {
	launch, err := tmux.CommandEnvironmentFromCommand(command, relativeExecutableAnchorA)
	if err != nil {
		return false
	}
	if usesRelativeExecutable(launch.Executable) {
		return true
	}
	// A relative shell wrapper (the first non-assignment token, the same token
	// CheckCommand resolves via resolveShellExecutableAt) makes the command's
	// launch depend on cwd even when the detected agent executable is a bare
	// name. The detected executable and the shell wrapper are independent
	// obligations; a relative wrapper resolves against the launch directory.
	if wrapper := relativeShellWrapper(command); wrapper != "" && wrapper != launch.Executable && usesRelativeExecutable(wrapper) {
		return true
	}
	// A bare executable is resolved through PATH, so a relative or empty PATH
	// entry — whether introduced by the command or inherited from the doctor's
	// environment — makes its lookup depend on the launch directory. Absolute
	// and relative-path executables ignore PATH, so a relative PATH entry does
	// not anchor them (PATH=bin /missing/codex resolves the absolute executable
	// directly and never consults PATH).
	if isBareExecutable(launch.Executable) {
		if path, ok := effectivePathValue(launch); ok && pathHasRelativeEntry(path) {
			return true
		}
	}
	// A relative env -C chdir makes the launch working directory depend on the
	// doctor's cwd. CommandEnvironmentFromCommand sets WorkingDir to the input
	// anchor when no chdir is applied, to the (absolute) chdir when it is
	// absolute, and to join(anchor, chdir) when it is relative. So a chdir
	// happened iff WorkingDir differs from the anchor, and it is relative iff
	// re-resolving under a second, distinct anchor yields a different WorkingDir.
	if launch.WorkingDir != relativeExecutableAnchorA {
		if other, err := tmux.CommandEnvironmentFromCommand(command, relativeExecutableAnchorB); err == nil && other.WorkingDir != launch.WorkingDir {
			return true
		}
	}
	return false
}

// relativeExecutableAnchorA and relativeExecutableAnchorB are two distinct
// absolute working directories used to detect a relative env -C chdir by
// comparison. They need not exist on disk; CommandEnvironmentFromCommand
// never stats them. Their parents differ, so a parent-relative chdir such as
// `env -C ../tools` resolves to distinct paths under each anchor instead of
// collapsing to the same root-relative path.
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
// resolveExecutableAt resolves against the launch directory: an empty entry
// (which becomes the launch directory) or a non-absolute entry (which is joined
// with it).
func pathHasRelativeEntry(pathValue string) bool {
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" || !filepath.IsAbs(dir) {
			return true
		}
	}
	return false
}

// usesRelativeExecutable mirrors the relative-path branch of
// resolveExecutableAt: a token with a path separator that is not absolute and
// not a tilde is the one executable shape whose resolution depends on the
// launch directory. Bare names depend on PATH entries' relativity (handled by
// RelativeExecutable via the PATH override), tilde expands to an absolute
// home, and absolute paths ignore the working directory entirely.
func usesRelativeExecutable(exe string) bool {
	if exe == "" {
		return false
	}
	if strings.HasPrefix(exe, "~") {
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
