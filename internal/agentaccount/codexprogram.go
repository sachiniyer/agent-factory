package agentaccount

import (
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// codexProgramValueOptions take their value as the next argv word; the
// attached forms (--name=value, -xvalue, -x=value) are matched separately.
// The grammar is the union of codex's root, exec, resume, and fork option
// surfaces (codex-rs CLI definitions) and mirrors the option set
// session/tmux/resume.go already models for resume insertion. The three
// value options that decide the effective sandbox story — -c/--config,
// -s/--sandbox, and -p/--profile — are deliberately absent and handled
// before this table.
var codexProgramValueOptions = map[string]bool{
	"-m": true, "--model": true,
	"-i": true, "--image": true,
	"-C": true, "--cd": true,
	"-a": true, "--ask-for-approval": true,
	"--local-provider":        true,
	"--add-dir":               true,
	"--remote":                true,
	"--remote-auth-token-env": true,
	"--enable":                true,
	"--disable":               true,
	"-o":                      true, "--output-last-message": true,
	"--output-schema": true,
	"--color":         true,
}

// codexProgramBooleanOptions are flag tokens that consume no value. Sandbox
// mode setters are deliberately absent — the walk reads those.
var codexProgramBooleanOptions = map[string]bool{
	"--strict-config":                 true,
	"--oss":                           true,
	"--dangerously-bypass-hook-trust": true,
	"--search":                        true,
	"--no-alt-screen":                 true,
	"--no-daemon":                     true,
	"--worktree":                      true,
	"-h":                              true,
	"--help":                          true,
	"-V":                              true,
	"--version":                       true,
	"--last":                          true,
	"--all":                           true,
	"--include-non-interactive":       true,
	"--skip-git-repo-check":           true,
	"--json":                          true,
	"--ephemeral":                     true,
	"--ignore-user-config":            true,
	"--ignore-rules":                  true,
}

// codexProgramAgentSubcommands launch the interactive or exec runtime and
// share the sandbox option grammar above — the only subcommand shapes an af
// agent session can actually be running. af itself records
// "codex [-c check_for_update_on_startup=false] resume <id>".
var codexProgramAgentSubcommands = map[string]bool{
	"resume": true, "fork": true, "exec": true, "e": true,
}

// codexProgramOtherSubcommands are recognized codex subcommands whose option
// grammar af does not model. They launch no agent runtime; a flag after one
// is not provably a codex sandbox flag, so the program answers nothing.
var codexProgramOtherSubcommands = map[string]bool{
	"review": true, "login": true, "logout": true, "mcp": true,
	"plugin": true, "app-server": true, "remote-control": true,
	"app": true, "completion": true, "update": true, "doctor": true,
	"sandbox": true, "debug": true, "execpolicy": true,
	"apply": true, "a": true, "cloud": true, "cloud-tasks": true,
	"agents": true, "queue": true, "archive": true, "delete": true,
	"unarchive": true, "migrate-rollouts": true, "migrate-nfs-db": true,
	"features": true, "responses-api-proxy": true, "stdio-to-uds": true,
	"exec-server": true, "tcp-tunnel": true,
}

// codexProgramSandboxOverride resolves the sandbox mode the session's launch
// program forces on codex's argv, for the drift compare's effective-mode
// question (#6902). The recorded program is what the runtime launches, and a
// command-line sandbox override applies before config.toml's sandbox_mode —
// so the config alone cannot answer whether sandbox_workspace_write is inert.
//
// ("", true): the program is one provable codex invocation carrying no
// sandbox override; the homes' configs decide the effective mode.
// (mode, true): every mode-setting flag in the argv agreed on mode, so codex's
// precedence order never had to be modeled.
// (_, false): the program cannot be reduced to a provable codex argv — a
// wrapper af does not model, a compound command, an unknown flag, an
// unresolvable -p profile selection, a malformed -c override, or disagreeing
// mode flags. No effective-mode claim may be made; the caller fails closed.
func codexProgramSandboxOverride(program string) (mode string, ok bool) {
	argv, ok := sessionenv.LiteralInvocationArgv(program)
	if !ok || len(argv) == 0 || !strings.EqualFold(filepath.Base(argv[0]), "codex") {
		return "", false
	}

	workspaceWrite := false
	other := ""
	inSubcommand := false
	positionals := 0

	recordMode := func(v string) {
		if v == "workspace-write" {
			workspaceWrite = true
		} else if other == "" {
			other = v
		}
	}

	args := argv[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// Option parsing ends; everything left is positional.
			positionals += len(args) - i - 1
			i = len(args)
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			// A positional word. Only the first positional can be a
			// subcommand name; codex matches it there even when flags
			// precede it.
			if !inSubcommand && positionals == 0 {
				if codexProgramOtherSubcommands[arg] {
					return "", false
				}
				if codexProgramAgentSubcommands[arg] {
					inSubcommand = true
					continue
				}
			}
			positionals++
		case arg == "-p" || arg == "--profile" ||
			strings.HasPrefix(arg, "--profile=") || strings.HasPrefix(arg, "-p"):
			// -p selects a config profile or (newer codex) a profile file;
			// which profile table it lands in is per-home state the command
			// string cannot resolve.
			return "", false
		case arg == "-s" || arg == "--sandbox":
			if i+1 >= len(args) {
				return "", false
			}
			i++
			recordMode(args[i])
		case strings.HasPrefix(arg, "--sandbox="):
			recordMode(strings.TrimPrefix(arg, "--sandbox="))
		case strings.HasPrefix(arg, "-s") && len(arg) > 2:
			recordMode(strings.TrimPrefix(arg[2:], "="))
		case arg == "--dangerously-bypass-approvals-and-sandbox" || arg == "--yolo":
			recordMode("danger-full-access")
		case arg == "--full-auto" || arg == "--approve-for-me" || arg == "--not-so-yolo":
			// --full-auto was removed upstream, but the session under a
			// drift compare ran an older codex that accepted it; both forms
			// mean workspace-write with an unattended approval policy.
			recordMode("workspace-write")
		case arg == "-c" || arg == "--config":
			if i+1 >= len(args) {
				return "", false
			}
			i++
			v, has, ok := codexConfigOverrideMode(args[i])
			if !ok {
				return "", false
			}
			if has {
				recordMode(v)
			}
		case strings.HasPrefix(arg, "--config="):
			v, has, ok := codexConfigOverrideMode(strings.TrimPrefix(arg, "--config="))
			if !ok {
				return "", false
			}
			if has {
				recordMode(v)
			}
		case strings.HasPrefix(arg, "-c") && len(arg) > 2:
			v, has, ok := codexConfigOverrideMode(strings.TrimPrefix(arg[2:], "="))
			if !ok {
				return "", false
			}
			if has {
				recordMode(v)
			}
		case codexProgramValueOptions[arg]:
			if i+1 >= len(args) {
				return "", false
			}
			i++
		case attachedValueOption(arg):
			// --name=value or -xvalue/-x=value for a known value option.
		case codexProgramBooleanOptions[arg]:
		default:
			return "", false
		}
		if positionals > codexProgramMaxPositionals(inSubcommand) {
			return "", false
		}
	}
	if workspaceWrite && other != "" {
		// Two mode signals disagree on the question that matters; codex's
		// precedence would decide, but af does not model it — fail closed.
		return "", false
	}
	if workspaceWrite {
		return "workspace-write", true
	}
	if other != "" {
		return other, true
	}
	return "", true
}

// codexProgramMaxPositionals bounds positional words af can prove belong to
// the runtime grammar: a bare prompt at the root, or a session selector plus
// prompt inside a modeled subcommand. More than that and a "flag" af read
// could instead be a positional the real parser rejected — unprovable.
func codexProgramMaxPositionals(inSubcommand bool) int {
	if inSubcommand {
		return 2
	}
	return 1
}

// attachedValueOption reports whether arg is a known value option in an
// attached form: --name=value for any long value option, or -xvalue/-x=value
// for the value-taking shorts. The sandbox-story shorts are intercepted
// earlier in the walk.
func attachedValueOption(arg string) bool {
	if i := strings.Index(arg, "="); i > 0 {
		if codexProgramValueOptions[arg[:i]] {
			return true
		}
	}
	if len(arg) > 2 && arg[0] == '-' && arg[1] != '-' && arg[1] != '=' {
		switch arg[1] {
		case 'i', 'm', 'C', 'a', 'o':
			return true
		}
	}
	return false
}

// codexConfigOverrideMode parses one -c/--config operand the way codex's
// CliConfigOverrides does: a TOML `key = value` line. hasMode reports the
// operand set sandbox_mode; mode is its string. ok=false means the operand
// cannot be proven — an unparseable line, an empty document, or a
// sandbox_mode whose value is not a string — so no claim may be made from it.
func codexConfigOverrideMode(operand string) (mode string, hasMode, ok bool) {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(operand), &doc); err != nil || len(doc) == 0 {
		return "", false, false
	}
	value, exists := doc["sandbox_mode"]
	if !exists {
		return "", false, true
	}
	mode, isString := value.(string)
	if !isString {
		return "", false, false
	}
	return mode, true, true
}
