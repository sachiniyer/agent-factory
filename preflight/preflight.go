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
