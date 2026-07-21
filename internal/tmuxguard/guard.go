// Package tmuxguard implements a best-effort guardrail against an agent
// accidentally tearing down the host's shared tmux server. It is not a
// security boundary: permitted shells and developer tools expose execution
// surfaces this string-level policy cannot completely model. Host containment
// tracked in #2194 must provide that boundary.
package tmuxguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
)

const (
	maxHookInput   = 4 << 20
	maxNestedShell = 32

	broadTmuxReason    = "Agent Factory blocked a host-wide tmux kill-server. Target an isolated server explicitly with 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
	patternKillReason  = "Agent Factory blocked a pattern-based process kill because it cannot prove the shared tmux server will be spared. Resolve the one intended PID and use 'kill -- <pid>'; for tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
	unknownShellReason = "Agent Factory could not prove this shell command safe, so it was blocked. Rewrite it as literal simple commands: replace variables, substitutions, globs, and brace or tilde expansions with literal values, and run inner commands directly instead of through eval or opaque command-building wrappers. Use af for session orchestration. For tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server' directly."
	opaqueInputReason  = "Agent Factory blocked an unmodeled here-document or stdin consumer because it cannot inspect the supplied code or data. Keep this as a two-tool operation: use the agent's non-shell file tool (Claude Write or Codex apply_patch) to create a reviewable literal file, then pass that literal path to the consumer. For Python, use 'python3 /tmp/task.py'; for GitHub text, use 'gh pr comment <number> --body-file /tmp/reply.md'. Do not pipe or heredoc interpreter code. Put shell code directly in the Bash request as literal simple commands. For tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
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
// tmux teardown or uses shell syntax the guard cannot prove safe. An empty
// result means no modeled hazard was found; it is not proof that arbitrary
// shell or developer-tool execution is safe. The parser is agent-neutral so
// #2184 can reuse one guardrail everywhere.
func DenialReason(command string) string {
	return denialReason(command, 0)
}

func denialReason(command string, depth int) string {
	if strings.TrimSpace(command) == "" || depth > maxNestedShell {
		return unknownShellReason
	}
	commands, err := parseShellCommands(command)
	if err != nil {
		if errors.Is(err, errOpaqueStdin) {
			return opaqueInputReason
		}
		return unknownShellReason
	}
	for _, words := range commands {
		if reason := inspectWords(words, depth); reason != "" {
			return reason
		}
	}
	return ""
}

func inspectWords(words []string, depth int) string {
	for i, word := range words {
		name := strings.ToLower(filepath.Base(word))
		switch name {
		case "tmux":
			if reason := inspectTmux(words[i+1:]); reason != "" {
				return reason
			}
		case "pkill", "killall":
			if i+1 < len(words) {
				return patternKillReason
			}
		case "env":
			if err := validateEnvPrefix(words[i+1:]); err != nil {
				return unknownShellReason
			}
		case "sh", "ash", "bash", "dash", "fish", "ksh", "mksh", "yash", "zsh":
			payload, found, err := shellCommandPayload(words[i+1:])
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
				switch arg {
				case "-exec", "-execdir", "-ok", "-okdir":
					return unknownShellReason
				}
			}
		}
	}
	return ""
}
