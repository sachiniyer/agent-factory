package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// CommandEnvOverride describes how a shell command changes one environment
// variable before the detected agent executable. Present=false means the child
// inherits the daemon value. Present=true with Set=false means env -u/-i removes
// it. Values returned from a successful parse are always literal.
type CommandEnvOverride struct {
	Value   string
	Present bool
	Set     bool
	Literal bool
}

// CommandEnvironment is the statically resolved launch context at the agent
// executable. It carries environment changes and cwd together because GNU env
// can change both in one invocation; resolving either in isolation recreates
// the receipt-routing drift this model exists to prevent.
type CommandEnvironment struct {
	WorkingDir     string
	clearInherited bool
	overrides      map[string]CommandEnvOverride
}

// Override reports the final command-local state of name.
func (e CommandEnvironment) Override(name string) CommandEnvOverride {
	if override, ok := e.overrides[name]; ok {
		return override
	}
	if e.clearInherited {
		return CommandEnvOverride{Present: true, Literal: true}
	}
	return CommandEnvOverride{}
}

// CommandEnvironmentFromCommand resolves the environment and cwd inherited by
// the first detected agent token. Every GNU env invocation is parsed by the
// same closed-set parser used by internal/tmuxguard. The only policy difference
// is explicit: receipt routing models literal assignments, while the guard
// rejects all assignments because they may alter executable resolution.
//
// Unknown options, split-string, dynamic values/chdirs, misplaced assignments,
// and an agent token consumed as an env operand return an error. Receipt callers
// must surface that error and abort rather than polling a guessed path.
func CommandEnvironmentFromCommand(command, workingDir string) (CommandEnvironment, error) {
	tokens, _ := splitShellTokens(command)
	agentIdx, agent, findErr := findAgentTokenStrict(tokens)
	result := CommandEnvironment{
		WorkingDir: filepath.Clean(workingDir),
		overrides:  make(map[string]CommandEnvOverride),
	}
	if !filepath.IsAbs(workingDir) {
		return result, fmt.Errorf("launch directory %q is not absolute; cannot resolve relative receipt paths", workingDir)
	}
	if findErr != nil {
		return result, fmt.Errorf("cannot resolve env invocation before agent: %w", findErr)
	}
	if agent == "" {
		return result, fmt.Errorf("could not find a supported agent executable")
	}

	atShellPrefix := true
	for idx := 0; idx < agentIdx; {
		tok := tokens[idx]
		if strings.EqualFold(baseCommand(tok), "env") {
			invocation, err := envcommand.Parse(tokens[idx+1:], envcommand.Policy{AllowAssignments: true})
			if err != nil {
				return result, fmt.Errorf("cannot resolve env invocation before %s: %w", agent, err)
			}
			if invocation.CommandIndex < 0 {
				return result, fmt.Errorf("cannot resolve env invocation before %s: env has no command", agent)
			}
			commandIdx := idx + 1 + invocation.CommandIndex
			if commandIdx > agentIdx {
				return result, fmt.Errorf("cannot resolve env invocation before %s: the agent token is an option operand, not env's command", agent)
			}
			if invocation.ClearEnvironment {
				result.clearInherited = true
				clear(result.overrides)
			}
			for _, mutation := range invocation.Mutations {
				if mutation.Unset {
					result.overrides[mutation.Name] = CommandEnvOverride{Present: true, Literal: true}
				} else {
					result.overrides[mutation.Name] = CommandEnvOverride{
						Value: mutation.Value, Present: true, Set: true, Literal: true,
					}
				}
			}
			if invocation.Chdir != "" {
				resolved, err := resolveCommandDir(result.WorkingDir, invocation.Chdir)
				if err != nil {
					return result, err
				}
				result.WorkingDir = resolved
			}
			idx = commandIdx
			atShellPrefix = false
			continue
		}

		if name, value, ok := shellAssignment(tok); ok {
			if !atShellPrefix {
				return result, fmt.Errorf("cannot resolve assignment %q after a command wrapper; use env with literal options and assignments", tok)
			}
			if !envcommand.IsLiteral(value) {
				return result, fmt.Errorf("%s uses shell expansion; use a literal value so receipt verification can follow it", name)
			}
			result.overrides[name] = CommandEnvOverride{Value: value, Present: true, Set: true, Literal: true}
			idx++
			continue
		}

		atShellPrefix = false
		idx++
	}
	return result, nil
}

// CodexHomeFromCommand resolves the rollout store the launched command will
// actually use. CODEX_HOME and HOME are interpreted in the same environment +
// cwd model as GNU env itself, so receipt/capture callers never silently watch
// the daemon's store while a wrapped Codex writes somewhere else.
func CodexHomeFromCommand(command, workingDir string) (string, error) {
	launch, err := CommandEnvironmentFromCommand(command, workingDir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve Codex environment: %w", err)
	}
	effective := func(name string) (string, bool, error) {
		override := launch.Override(name)
		if !override.Present {
			value, set := os.LookupEnv(name)
			return value, set, nil
		}
		if !override.Literal {
			return "", false, fmt.Errorf("%s uses shell expansion; use a literal path so Codex storage can be followed", name)
		}
		return override.Value, override.Set, nil
	}
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Clean(filepath.Join(launch.WorkingDir, path))
	}

	if codexHome, set, err := effective("CODEX_HOME"); err != nil {
		return "", err
	} else if set && strings.TrimSpace(codexHome) != "" {
		return resolve(codexHome), nil
	}
	home, set, err := effective("HOME")
	if err != nil {
		return "", err
	}
	if !set || strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("CODEX_HOME is unset and the launched command has no literal HOME fallback")
	}
	return filepath.Join(resolve(home), ".codex"), nil
}

func resolveCommandDir(current, requested string) (string, error) {
	if filepath.IsAbs(requested) {
		return filepath.Clean(requested), nil
	}
	if strings.TrimSpace(current) == "" || current == "." {
		return "", fmt.Errorf("cannot resolve relative env chdir %q without an absolute launch directory", requested)
	}
	return filepath.Clean(filepath.Join(current, requested)), nil
}

func shellAssignment(token string) (name, value string, ok bool) {
	name, value, ok = envcommand.SplitAssignment(token)
	if !ok {
		return "", "", false
	}
	for idx, r := range name {
		if (idx == 0 && !unicode.IsLetter(r) && r != '_') ||
			(idx > 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_') {
			return "", "", false
		}
	}
	return name, value, true
}

func baseCommand(token string) string {
	if slash := strings.LastIndexAny(token, `/\`); slash >= 0 {
		return token[slash+1:]
	}
	return token
}
