// Package tmuxguard implements the agent hook policy that prevents an
// af-managed agent from tearing down the host's shared tmux server.
package tmuxguard

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
)

const (
	maxHookInput   = 4 << 20
	maxNestedShell = 32

	broadTmuxReason    = "Agent Factory blocked a host-wide tmux kill-server. Target an isolated server explicitly with 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
	patternKillReason  = "Agent Factory blocked a pattern-based process kill because it cannot prove the shared tmux server will be spared. Resolve the one intended PID and use 'kill -- <pid>'; for tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
	unknownShellReason = "Agent Factory could not resolve an execution-sensitive part of this shell command, so it was blocked. Rewrite it as literal simple commands: keep executables, wrapper options, and tmux/process-kill arguments literal; move dynamic values into ordinary data arguments or separate commands; and run inner commands directly instead of through eval or opaque command builders. Use af for session orchestration. For tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server' directly."
	opaqueInputReason  = "Agent Factory blocked an unmodeled here-document or stdin consumer because it cannot inspect the supplied code or data. For data, write it to a literal file and pass that literal path (Git commit messages may use 'git commit -F -'); for interpreter code, create and review a literal script file, then run the interpreter with its literal path. For tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
	findReason         = "Agent Factory blocked a find command whose operands could become command-building syntax. Rewrite a dynamic root as 'cd \"$root\" && find . <literal predicates>', and replace -exec/-execdir/-ok/-okdir with a separate literal command over the results. For tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
)

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

// Run reads one PreToolUse payload and emits a structured denial when its
// shell command is unsafe or cannot be validated. The hook fails closed: a
// broken or drifted payload must not grant permission to destroy shared state.
func Run(r io.Reader, w io.Writer) error {
	input, ok := decodeHookInput(r)
	if !ok {
		return writeDenial(w, unknownShellReason)
	}
	if input.ToolName == "" {
		return writeDenial(w, unknownShellReason)
	}
	if input.ToolName != "Bash" {
		return nil
	}
	if reason := DenialReason(input.ToolInput.Command); reason != "" {
		return writeDenial(w, reason)
	}
	return nil
}

func decodeHookInput(r io.Reader) (hookInput, bool) {
	var input hookInput
	raw, err := io.ReadAll(io.LimitReader(r, maxHookInput+1))
	if err != nil || len(raw) > maxHookInput {
		return input, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&input); err != nil {
		return input, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return input, false
	}
	return input, true
}

func writeDenial(w io.Writer, reason string) error {
	var decision hookDecision
	decision.HookSpecificOutput.HookEventName = "PreToolUse"
	decision.HookSpecificOutput.PermissionDecision = "deny"
	decision.HookSpecificOutput.PermissionDecisionReason = reason
	return json.NewEncoder(w).Encode(decision)
}

// DenialReason returns an actionable reason when command contains a broad
// tmux teardown or uses shell dispatch syntax the guard cannot prove safe. An
// empty result means executable positions and modeled wrapper/tmux semantics
// were resolved; dynamic expansions appeared only in audited data positions.
// This is a shell-dispatch guard, not a sandbox for arbitrary executable code.
// The parser is agent-neutral so #2184 can reuse one policy everywhere.
func DenialReason(command string) string {
	return denialReason(command, 0)
}

func denialReason(command string, depth int) string {
	if strings.TrimSpace(command) == "" || depth > maxNestedShell {
		return unknownShellReason
	}
	commands, err := parseShellCommands(command)
	if err != nil {
		return unknownShellReason
	}
	for _, command := range commands {
		if reason := inspectCommand(command, depth); reason != "" {
			return reason
		}
	}
	return ""
}

func inspectCommand(command shellCommand, depth int) string {
	if command.declaration != nil {
		if safeShellDeclaration(command.declaration) {
			return ""
		}
		return unknownShellReason
	}
	if !safeShellAssignments(command.assignments) {
		return unknownShellReason
	}
	if len(command.words) == 0 {
		return "" // A modeled scalar assignment does not execute a command.
	}
	if !command.words[0].resolved {
		return unknownShellReason
	}
	words := command.words
	name := strings.ToLower(filepath.Base(words[0].literal))
	if command.hasHeredoc && !safeHeredocCommand(words) {
		return opaqueInputReason
	}
	// These commands consume their arguments as data rather than as another
	// host command. Command/process substitutions within their arguments are
	// separate AST commands and are still inspected below in the command list.
	if safeTerminalCommand(name) {
		return ""
	}
	if shellExecutable(name) {
		payload, found, err := shellCommandPayloadWords(words[1:])
		if err != nil || !found {
			return unknownShellReason
		}
		return denialReason(payload, depth+1)
	}
	switch name {
	case "command":
		target, noCommand, err := commandTarget(words[1:])
		if err != nil {
			return unknownShellReason
		}
		if noCommand {
			return ""
		}
		return inspectCommand(shellCommand{words: target}, depth)
	case "env":
		target, noCommand, err := envTarget(words[1:])
		if err != nil {
			return unknownShellReason
		}
		if noCommand {
			return ""
		}
		return inspectCommand(shellCommand{words: target}, depth)
	case "find":
		return inspectFind(words[1:])
	case "timeout":
		target, noCommand, err := timeoutTarget(words[1:])
		if err != nil {
			return unknownShellReason
		}
		if noCommand {
			return ""
		}
		return inspectCommand(shellCommand{words: target}, depth)
	}
	if unmodeledWrapper(name) && len(words) > 1 {
		return unknownShellReason
	}
	for i, word := range words {
		if !word.resolved {
			continue
		}
		name := strings.ToLower(filepath.Base(word.literal))
		switch name {
		case "tmux":
			suffix, ok := literalWords(words[i+1:])
			if !ok {
				return unknownShellReason
			}
			if reason := inspectTmux(suffix); reason != "" {
				return reason
			}
		case "pkill", "killall":
			if i+1 < len(words) {
				return patternKillReason
			}
		case "env":
			suffix, ok := literalWords(words[i+1:])
			if !ok || validateEnvPrefix(suffix) != nil {
				return unknownShellReason
			}
		case "sh", "ash", "bash", "dash", "fish", "ksh", "mksh", "yash", "zsh":
			suffix, ok := literalWords(words[i+1:])
			if !ok {
				return unknownShellReason
			}
			payload, found, err := shellCommandPayload(suffix)
			if err != nil {
				return unknownShellReason
			}
			if found {
				if reason := denialReason(payload, depth+1); reason != "" {
					return reason
				}
			} else {
				// A script path or stdin-fed shell is opaque to this hook. The
				// caller can put a literal command in this tool call instead.
				return unknownShellReason
			}
		case ".", "alias", "autoload", "bind", "builtin", "declare", "enable", "eval", "export", "fc", "hash", "local", "mapfile", "parallel", "readarray", "readonly", "set", "shopt", "source", "trap", "typeset", "unalias", "unset", "xargs":
			if i+1 < len(words) {
				return unknownShellReason
			}
		case "find":
			for _, arg := range words[i+1:] {
				if !arg.resolved {
					return unknownShellReason
				}
				switch arg.literal {
				case "-exec", "-execdir", "-ok", "-okdir":
					return unknownShellReason
				}
			}
		}
	}
	if hasDynamicWord(words) {
		return unknownShellReason
	}
	return ""
}

func inspectFind(args []shellWord) string {
	for _, arg := range args {
		if !arg.resolved {
			return findReason
		}
		switch arg.literal {
		case "-exec", "-execdir", "-ok", "-okdir":
			return findReason
		}
	}
	return ""
}

func safeShellAssignments(assignments []shellAssignment) bool {
	for _, assignment := range assignments {
		if !assignment.simple || executionSensitiveVariable(assignment.name) {
			return false
		}
	}
	return true
}

func safeShellDeclaration(declaration *shellDeclaration) bool {
	if declaration.variant != "export" || len(declaration.assignments) == 0 {
		return false
	}
	for _, assignment := range declaration.assignments {
		if !assignment.simple || !safeExportVariable(assignment.name) {
			return false
		}
	}
	return true
}

func safeExportVariable(name string) bool {
	switch name {
	case "AF_PLAYTEST_NAME", "AGENT_FACTORY_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"GEMINI_CLI_HOME", "GOFLAGS":
		return true
	default:
		return false
	}
}

func executionSensitiveVariable(name string) bool {
	switch name {
	case "BASHOPTS", "BASH_ENV", "CDPATH", "ENV", "FPATH", "GLOBIGNORE", "IFS",
		"KSHENV", "LD_LIBRARY_PATH", "LD_PRELOAD", "PATH", "PROMPT_COMMAND", "SHELL",
		"SHELLOPTS", "TMUX", "TMUX_TMPDIR", "ZDOTDIR":
		return true
	default:
		return strings.HasPrefix(name, "DYLD_")
	}
}

func literalWords(words []shellWord) ([]string, bool) {
	literals := make([]string, 0, len(words))
	for _, word := range words {
		if !word.resolved {
			return nil, false
		}
		literals = append(literals, word.literal)
	}
	return literals, true
}

func hasDynamicWord(words []shellWord) bool {
	for _, word := range words {
		if !word.resolved {
			return true
		}
	}
	return false
}

func safeTerminalCommand(name string) bool {
	switch name {
	case ":", "[", "af", "agent-browser", "av", "basename", "cat", "cd", "chmod", "cmp", "comm",
		"cp", "curl", "cut", "date", "diff", "dirname", "docker", "du", "echo", "false",
		"file", "gcloud", "gh", "git", "go", "gofmt", "grep", "head", "jq", "ln", "ls", "mkdir",
		"paste", "printenv", "printf", "ps", "pwd", "readlink", "realpath", "rg", "rm", "rmdir",
		"shellcheck", "sort", "stat", "strings", "tail", "tee", "test", "touch", "tr", "true",
		"wc", "which":
		return true
	default:
		return false
	}
}

func safeHeredocCommand(words []shellWord) bool {
	name := strings.ToLower(filepath.Base(words[0].literal))
	switch name {
	case "cat", "grep", "head", "sort", "tail", "tr", "wc":
		return true
	}
	literals, ok := literalWords(words)
	return ok && name == "git" && gitCommitReadsMessageFromStdin(literals[1:])
}
