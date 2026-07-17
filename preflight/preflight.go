package preflight

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session/tmux"
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

// CheckCommand verifies the first executable in command. It handles common
// shell shapes used in program_overrides: quotes, explicit paths, leading
// VAR=value assignments, and env VAR=value wrappers.
func CheckCommand(command string) (*ProgramCheck, error) {
	command = strings.TrimSpace(command)
	check := &ProgramCheck{Command: command}
	if command == "" {
		return check, fmt.Errorf("command is empty")
	}
	words, err := shellWords(command, 12)
	if err != nil {
		return check, err
	}
	exe := firstExecutable(words)
	check.Executable = exe
	if exe == "" {
		return check, fmt.Errorf("could not find an executable in command %q", command)
	}
	if strings.ContainsRune(exe, filepath.Separator) || strings.HasPrefix(exe, "~") {
		path := config.ExpandTilde(exe)
		info, err := os.Stat(path)
		if err != nil {
			return check, fmt.Errorf("executable %q does not exist", exe)
		}
		if info.IsDir() {
			return check, fmt.Errorf("executable %q is a directory", exe)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return check, fmt.Errorf("executable %q is not executable; run: %s", exe, shellsuggest.Command("chmod", "+x", path))
		}
		check.Path = path
		return check, nil
	}
	path, err := exec.LookPath(exe)
	if err != nil {
		return check, fmt.Errorf("executable %q was not found on PATH", exe)
	}
	check.Path = path
	return check, nil
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

// ProgramError renders the actionable user-facing error for a missing agent.
func ProgramError(agent, command string, cause error) error {
	name := agentDisplayName(agent)
	if isSupportedAgent(agent) {
		return fmt.Errorf("%s is not installed or not on PATH (resolved command: %q). Install %s, choose another agent, or set program_overrides.%s in ~/.agent-factory/config.toml. Details: %v",
			name, command, name, agent, cause)
	}
	return fmt.Errorf("program %q is not installed or not on PATH (resolved command: %q). Install it, choose another agent, or fix the command in config. Details: %v",
		agent, command, cause)
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

func firstExecutable(words []string) string {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	if words[0] != "env" {
		return words[0]
	}
	words = words[1:]
	for len(words) > 0 {
		switch {
		case words[0] == "--":
			words = words[1:]
		case isAssignment(words[0]):
			words = words[1:]
		case words[0] == "-u" || words[0] == "--unset" || words[0] == "-C" || words[0] == "--chdir":
			if len(words) < 2 {
				return ""
			}
			words = words[2:]
		case strings.HasPrefix(words[0], "-"):
			words = words[1:]
		default:
			return words[0]
		}
	}
	return ""
}

func isAssignment(word string) bool {
	i := strings.IndexRune(word, '=')
	if i <= 0 {
		return false
	}
	for pos, r := range word[:i] {
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
