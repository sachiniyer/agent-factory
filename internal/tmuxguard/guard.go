// Package tmuxguard implements a best-effort safety interlock against an agent
// accidentally tearing down the host's shared tmux server. It is not a
// security boundary: permitted developer tools can execute programs through
// files, configuration, plugins, and other surfaces this package cannot model.
// Host containment tracked in #2194 must provide that boundary.
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
	sortReason         = "Agent Factory blocked GNU sort's external compressor because --compress-program runs another host program. Run sort without --compress-program, or compress and decompress in an isolated runner."
	unknownShellReason = "Agent Factory's best-effort tmux guard did not recognize this command as an approved shape, so it was blocked. This hook reduces accidental host-wide teardown; it is not containment and cannot prove an arbitrary developer command safe. Rewrite it as supported literal simple commands: keep executables, wrapper options, subcommands, and command-bearing arguments literal; put dynamic data after '--' where the program supports it. Remove assignment prefixes from command-dispatching programs, run inner commands directly instead of through eval or opaque builders, and use a dedicated non-shell tool for an unmodeled program. Move inline interpreter input to a literal script that you have reviewed, put GNU sed '--sandbox' before every script or -e/-f option, invoke Make with bare literal targets, and omit Go executor selectors such as -exec, -toolexec, and -vettool. Git commands must use a literal built-in subcommand without -c/--config-env. Docker inspection may use commands such as 'docker ps' or 'docker inspect' with literal, non-SSH global-option operands; container workloads need an isolated runner. Run gh without --web/-w or --editor/-e and consume its terminal output; supply authentication through a preconfigured token instead of browser-based auth login/refresh, and use a dedicated browser tool instead of a browser-launching CLI. Use af for session orchestration. For tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server' directly."
	opaqueInputReason  = "Agent Factory blocked an unmodeled here-document or stdin consumer because it cannot inspect the supplied code or data. For data, write it to a literal file and pass that literal path (Git commit messages may use 'git commit -F -'); for Python code, create and review a literal script file, then run Python with that literal path. Put shell code directly in this Bash request as literal simple commands instead of feeding a shell or script file. For tmux teardown, use 'tmux -L <socket> kill-server' or 'tmux -S <path> kill-server'."
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
// tmux teardown or a shell/program dispatch path covered by this best-effort
// model. An empty result means no modeled hazard was found; it is not proof
// that an arbitrary program, file, plugin, or inherited configuration is safe.
// The parser and program policy are agent-neutral so #2184 can reuse one speed
// bump everywhere, while containment in #2194 supplies the security boundary.
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
	states := make(map[int]shellExecutionState)
	for _, parsed := range commands {
		command := parsed
		// A child execution scope inherits state from each ancestor. Inspect
		// before updating the local state so mutations flow only forward; child
		// state is stored under its own ID and therefore cannot leak upward.
		inherited := inheritedShellExecutionState(command.scopePath, states)
		command.directoryChanged = command.directoryChanged || inherited.directoryChanged
		command.environmentAssigned = command.environmentAssigned || inherited.environmentChanged
		if reason := inspectCommand(command, depth); reason != "" {
			return reason
		}
		scope := command.scopePath[len(command.scopePath)-1]
		state := states[scope]
		state.directoryChanged = state.directoryChanged || changesShellDirectory(parsed)
		state.environmentChanged = state.environmentChanged || changesShellEnvironment(parsed)
		states[scope] = state
	}
	return ""
}

type shellExecutionState struct {
	directoryChanged   bool
	environmentChanged bool
}

func inheritedShellExecutionState(scopePath []int, states map[int]shellExecutionState) shellExecutionState {
	var inherited shellExecutionState
	for _, scope := range scopePath {
		state := states[scope]
		inherited.directoryChanged = inherited.directoryChanged || state.directoryChanged
		inherited.environmentChanged = inherited.environmentChanged || state.environmentChanged
	}
	return inherited
}

func changesShellDirectory(command shellCommand) bool {
	words := command.words
	for len(words) > 0 && words[0].resolved && words[0].literal == "command" {
		target, noCommand, err := commandTarget(words[1:])
		if err != nil || noCommand {
			return false
		}
		words = target
	}
	return len(words) > 0 && words[0].resolved && words[0].literal == "cd"
}

func changesShellEnvironment(command shellCommand) bool {
	return (len(command.words) == 0 && len(command.assignments) > 0) || command.declaration != nil
}

func inspectCommand(command shellCommand, depth int) string {
	if command.declaration != nil {
		if safeShellDeclaration(command.declaration) {
			return ""
		}
		return unknownShellReason
	}
	if len(command.words) == 0 {
		for _, assignment := range command.assignments {
			if !assignment.simple {
				return unknownShellReason
			}
		}
		// A scalar shell-local assignment does not select a program. If a
		// later command expands it into an execution-sensitive position, that
		// word remains tainted and the selected program policy rejects it.
		return ""
	}
	if !command.words[0].resolved {
		return unknownShellReason
	}
	words := command.words
	policy := classifyProgram(words[0].literal)
	name := strings.ToLower(filepath.Base(words[0].literal))
	if command.hasHeredoc && !safeHeredocCommand(words) {
		return opaqueInputReason
	}

	// Pattern kills and broad tmux teardown remain the most specific denial,
	// even when a wrapper also changed the child's environment.
	if policy.role == rolePatternKill {
		return patternKillReason
	}
	if policy.role == roleTmux {
		suffix, ok := literalWords(words[1:])
		if !ok {
			return unknownShellReason
		}
		if reason := inspectTmux(suffix); reason != "" {
			return reason
		}
		if len(command.assignments) > 0 || command.environmentAssigned || command.directoryChanged {
			return unknownShellReason
		}
		return ""
	}

	// Prefix assignments are never globally safe: their meaning belongs to
	// the program that consumes the resulting environment. No current program
	// policy proves arbitrary assignments inert, so they fail closed instead of
	// relying on an inevitably incomplete variable-name denylist.
	if len(command.assignments) > 0 || command.environmentAssigned {
		return unknownShellReason
	}
	if command.directoryChanged && !policy.directoryInert {
		return unknownShellReason
	}

	switch policy.dispatch {
	case dispatchNone:
		// Nested shell substitutions are separate AST commands and are still
		// inspected by parseShellCommands.
		return ""
	case dispatchTrusted:
		return ""
	case dispatchOpaque:
		return unknownShellReason
	case dispatchAudited:
		// Continue into the role-specific grammar below.
	default:
		return unknownShellReason
	}

	switch policy.role {
	case roleShell:
		payload, found, err := shellCommandPayloadWords(words[1:])
		if err != nil || !found {
			return unknownShellReason
		}
		return denialReason(payload, depth+1)
	case roleCommand:
		target, noCommand, err := commandTarget(words[1:])
		if err != nil {
			return unknownShellReason
		}
		if noCommand {
			return ""
		}
		return inspectCommand(shellCommand{words: target}, depth)
	case roleEnv:
		target, noCommand, effects, err := envTarget(words[1:])
		if err != nil {
			return unknownShellReason
		}
		if noCommand {
			return ""
		}
		return inspectCommand(shellCommand{
			words:               target,
			environmentAssigned: effects.assigned,
			directoryChanged:    effects.chdir,
		}, depth)
	case roleFind:
		return inspectFind(words[1:])
	case rolePIDKill:
		return inspectPIDKill(words[1:])
	case roleTimeout:
		target, noCommand, err := timeoutTarget(words[1:])
		if err != nil {
			return unknownShellReason
		}
		if noCommand {
			return ""
		}
		return inspectCommand(shellCommand{words: target}, depth)
	case rolePrintf:
		return inspectPrintf(words[1:])
	case roleTest:
		return inspectTest(name, words[1:])
	case roleGit:
		return inspectGit(words[1:])
	case roleDocker:
		return inspectDocker(words[1:])
	case roleRipgrep:
		return inspectRipgrep(words[1:])
	case roleSort:
		return inspectSort(words[1:])
	case roleGitHub:
		return inspectGitHub(words[1:])
	case roleGo:
		return inspectGo(words[1:])
	case rolePython:
		return inspectPython(words[1:])
	case roleSed:
		return inspectSed(words[1:])
	case roleJournalctl:
		return inspectJournalctl(words[1:])
	case roleMake:
		return inspectMake(words[1:])
	case roleFile:
		return inspectFile(words[1:])
	default:
		return unknownShellReason
	}
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
		"GEMINI_CLI_HOME":
		return true
	default:
		return false
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

func safeHeredocCommand(words []shellWord) bool {
	name := strings.ToLower(filepath.Base(words[0].literal))
	switch name {
	case "cat", "grep", "head", "sort", "tail", "tr", "wc":
		return true
	}
	literals, ok := literalWords(words)
	return ok && name == "git" && gitCommitReadsMessageFromStdin(literals[1:])
}
