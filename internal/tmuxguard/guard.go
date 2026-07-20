// Package tmuxguard implements the Claude PreToolUse policy that prevents an
// af-managed agent from tearing down the host's shared tmux server.
package tmuxguard

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"unicode"
)

const maxHookInput = 4 << 20

type hookInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

type hookDecision struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// Run reads one Claude PreToolUse payload and emits a structured denial when
// its Bash command would broadly kill tmux. Malformed input fails closed: a
// broken guard must not turn into permission to destroy the shared server.
func Run(r io.Reader, w io.Writer) error {
	var input hookInput
	if err := json.NewDecoder(io.LimitReader(r, maxHookInput)).Decode(&input); err != nil {
		return writeDenial(w, "Agent Factory could not validate this Bash command, so it was blocked to protect shared tmux sessions.")
	}
	if input.ToolName != "Bash" {
		return nil
	}
	if reason := DenialReason(input.ToolInput.Command); reason != "" {
		return writeDenial(w, reason)
	}
	return nil
}

func writeDenial(w io.Writer, reason string) error {
	var decision hookDecision
	decision.HookSpecificOutput.HookEventName = "PreToolUse"
	decision.HookSpecificOutput.PermissionDecision = "deny"
	decision.HookSpecificOutput.PermissionDecisionReason = reason
	return json.NewEncoder(w).Encode(decision)
}

// DenialReason returns an actionable reason when command contains a direct,
// host-wide tmux teardown. An empty result means the command is allowed.
func DenialReason(command string) string {
	for _, words := range shellCommandWords(command) {
		executable, args := unwrapInvocation(removeRedirections(words))
		switch strings.ToLower(filepath.Base(executable)) {
		case "tmux":
			if tmuxKillServerIsBroad(args) {
				return "Agent Factory blocked a host-wide tmux kill-server. Target an isolated server explicitly with 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
			}
		case "pkill":
			if pkillTargetsTmux(args) {
				return "Agent Factory blocked pkill targeting tmux because pkill cannot be scoped to one tmux socket. Use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server' instead."
			}
		case "sh", "bash", "dash", "zsh", "ksh":
			if nested := shellCommandArgument(args); nested != "" {
				if reason := DenialReason(nested); reason != "" {
					return reason
				}
			}
		case "eval":
			if reason := DenialReason(strings.Join(args, " ")); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func tmuxKillServerIsBroad(args []string) bool {
	socketScoped := false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "-L" || arg == "-S":
			if i+1 < len(args) && args[i+1] != "" {
				socketScoped = true
			}
			i++
		case (strings.HasPrefix(arg, "-L") || strings.HasPrefix(arg, "-S")) && len(arg) > 2:
			socketScoped = true
		case arg == "kill-server":
			return !socketScoped
		}
	}
	return false
}

func pkillTargetsTmux(args []string) bool {
	if len(args) == 0 {
		return false
	}
	// pkill's one required positional argument is its pattern. In the normal
	// and documented forms it is the final argument; matching "tmux" inside
	// that regex also catches anchored spellings such as ^tmux$.
	return strings.Contains(strings.ToLower(args[len(args)-1]), "tmux")
}

func shellCommandArgument(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "-c") && len(args[i]) > 2 {
			return args[i][2:]
		}
	}
	return ""
}

const redirectionToken = "\x00af-redirection"

func removeRedirections(words []string) []string {
	clean := make([]string, 0, len(words))
	for i := 0; i < len(words); i++ {
		if words[i] == redirectionToken {
			if i+1 < len(words) {
				i++
			}
			continue
		}
		clean = append(clean, words[i])
	}
	return clean
}

// unwrapInvocation returns the executable and arguments for one shell command
// segment. It recognizes the common wrappers agents use around commands, while
// leaving arbitrary argument text (for example "echo tmux kill-server") alone.
func unwrapInvocation(words []string) (string, []string) {
	for len(words) > 0 {
		for len(words) > 0 && (isAssignment(words[0]) || isControlPrefix(words[0])) {
			words = words[1:]
		}
		if len(words) == 0 {
			return "", nil
		}

		switch strings.ToLower(filepath.Base(words[0])) {
		case "command", "exec", "nohup":
			words = skipFlagOnlyPrefix(words[1:])
		case "env":
			words = skipEnvPrefix(words[1:])
		case "sudo":
			words = skipSudoPrefix(words[1:])
		case "nice":
			words = skipNicePrefix(words[1:])
		case "timeout":
			words = skipTimeoutPrefix(words[1:])
		default:
			return words[0], words[1:]
		}
	}
	return "", nil
}

func skipFlagOnlyPrefix(words []string) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		if words[0] == "--" {
			return words[1:]
		}
		words = words[1:]
	}
	return words
}

func skipEnvPrefix(words []string) []string {
	for len(words) > 0 {
		switch {
		case words[0] == "--":
			return words[1:]
		case words[0] == "-u" || words[0] == "--unset":
			if len(words) < 2 {
				return nil
			}
			words = words[2:]
		case strings.HasPrefix(words[0], "-") || isAssignment(words[0]):
			words = words[1:]
		default:
			return words
		}
	}
	return words
}

func skipSudoPrefix(words []string) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		if words[0] == "--" {
			return words[1:]
		}
		if sudoOptionTakesValue(words[0]) {
			if len(words) < 2 {
				return nil
			}
			words = words[2:]
			continue
		}
		words = words[1:]
	}
	return words
}

func sudoOptionTakesValue(option string) bool {
	switch option {
	case "-u", "--user", "-g", "--group", "-h", "--host", "-p", "--prompt", "-C", "--close-from", "-R", "--chroot", "-T", "--command-timeout":
		return true
	default:
		return false
	}
}

func skipNicePrefix(words []string) []string {
	if len(words) >= 2 && (words[0] == "-n" || words[0] == "--adjustment") {
		return words[2:]
	}
	if len(words) > 0 && strings.HasPrefix(words[0], "-") {
		return words[1:]
	}
	return words
}

func skipTimeoutPrefix(words []string) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		if words[0] == "--" {
			words = words[1:]
			break
		}
		if words[0] == "-k" || words[0] == "--kill-after" || words[0] == "-s" || words[0] == "--signal" {
			if len(words) < 2 {
				return nil
			}
			words = words[2:]
			continue
		}
		words = words[1:]
	}
	if len(words) > 0 { // duration
		words = words[1:]
	}
	return words
}

func isControlPrefix(word string) bool {
	switch word {
	case "!", "if", "then", "elif", "else", "while", "until", "do", "time", "coproc":
		return true
	default:
		return false
	}
}

func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range word[:eq] {
		if (i == 0 && !unicode.IsLetter(r) && r != '_') || (i > 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_') {
			return false
		}
	}
	return true
}

// shellCommandWords performs the small amount of shell lexing the policy
// needs: quotes and backslash escapes are decoded, while list/pipeline/control
// operators split independent command segments. It deliberately does not
// expand variables; a dynamic socket flag is not an explicit -L/-S and is
// therefore denied.
func shellCommandWords(command string) [][]string {
	var commands [][]string
	var words []string
	var word strings.Builder
	inWord := false
	quote := byte(0)

	flushWord := func() {
		if !inWord {
			return
		}
		words = append(words, word.String())
		word.Reset()
		inWord = false
	}
	flushCommand := func() {
		flushWord()
		if len(words) != 0 {
			commands = append(commands, words)
			words = nil
		}
	}

	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if quote == '"' {
			switch c {
			case '"':
				quote = 0
			case '\\':
				if i+1 < len(command) {
					i++
					word.WriteByte(command[i])
				}
			default:
				word.WriteByte(c)
			}
			continue
		}

		switch c {
		case ' ', '\t', '\r':
			flushWord()
		case '\n', ';', '|', '&', '(', ')', '{', '}':
			flushCommand()
			if i+1 < len(command) && command[i+1] == c && (c == '|' || c == '&') {
				i++
			}
		case '<', '>':
			if inWord && allDigits(word.String()) {
				word.Reset()
				inWord = false
			} else {
				flushWord()
			}
			words = append(words, redirectionToken)
			if i+1 < len(command) && (command[i+1] == c || command[i+1] == '&') {
				i++
			}
		case '\\':
			inWord = true
			if i+1 < len(command) {
				i++
				word.WriteByte(command[i])
			}
		case '\'', '"':
			inWord = true
			quote = c
		case '#':
			if !inWord {
				for i+1 < len(command) && command[i+1] != '\n' {
					i++
				}
			} else {
				word.WriteByte(c)
			}
		default:
			inWord = true
			word.WriteByte(c)
		}
	}
	flushCommand()
	return commands
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
