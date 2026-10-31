package tmux

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
	// Executable is the exact token whose launch context was resolved. When a
	// supported agent token is present it is that agent executable; otherwise it
	// is the first command that can be proven through leading env wrappers.
	Executable string
	// Agent is the canonical supported agent name when Executable was selected
	// from positive agent-token detection, or empty for an opaque command.
	Agent string
	// WorkingDir is the modeled cwd at Executable. Call WorkingDirKnown before
	// using it as process identity: arbitrary prefixes are token-detectable but
	// can change cwd through shell syntax or wrapper behavior af does not model.
	WorkingDir      string
	workingDirKnown bool
	// preExecDirKnown is the narrower fact config-root resolution needs: the
	// directory the launch STARTS the process in is established. It stays true
	// under Codex's own -C/--cd, which only moves the working root after Codex
	// has resolved its home, and goes false for anything that may chdir before
	// exec. workingDirKnown additionally goes false under -C because receipt
	// correlation needs the cwd the agent ends up in.
	preExecDirKnown bool
	clearInherited  bool
	overrides       map[string]CommandEnvOverride
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

// WorkingDirKnown reports whether the target cwd was established entirely
// through the launch directory and modeled env -C options. An arbitrary prefix
// can change cwd before exec (for example, `cd /tmp && codex` or a wrapper that
// calls chdir), and Codex's own -C/--cd option changes the working root after
// exec, so the inherited directory is not evidence for receipt routing.
func (e CommandEnvironment) WorkingDirKnown() bool {
	return e.workingDirKnown
}

// LaunchDirKnown reports whether the directory the agent process starts in is
// established. Relative environment paths (a config root) resolve against it,
// and Codex's -C/--cd does not disturb it: the flag is applied after Codex
// resolves CODEX_HOME. See preExecDirKnown.
func (e CommandEnvironment) LaunchDirKnown() bool {
	return e.preExecDirKnown
}

// CommandEnvironmentFromCommand resolves the environment and cwd inherited by
// the first detected agent token. When there is no supported agent token, it
// resolves only the first executable provable through shell assignments and env
// wrappers; it never guesses through an arbitrary wrapper's operand grammar.
// Every GNU env invocation is parsed by envcommand.Parse, the closed-set parser
// af uses everywhere. Receipt routing models literal assignments
// (Policy{AllowAssignments: true}) because they can alter which executable runs
// and where its receipts land.
//
// Unknown options, split-string, dynamic values/chdirs, misplaced assignments,
// and an agent token consumed as an env operand return an error. An arbitrary
// prefix leaves the modeled directory in WorkingDir but makes WorkingDirKnown
// false, so receipt callers can preserve their non-correlating fallback rather
// than treating that inherited base as the agent process's proven cwd.
func CommandEnvironmentFromCommand(command, workingDir string) (CommandEnvironment, error) {
	tokens, _ := SplitShellTokens(command)
	targetIdx, agent, findErr := findAgentTokenStrict(tokens)
	result := CommandEnvironment{
		WorkingDir:      filepath.Clean(workingDir),
		workingDirKnown: true,
		preExecDirKnown: true,
		overrides:       make(map[string]CommandEnvOverride),
	}
	if !filepath.IsAbs(workingDir) {
		return result, fmt.Errorf("launch directory %q is not absolute; cannot resolve relative receipt paths", workingDir)
	}
	if findErr != nil {
		return result, fmt.Errorf("cannot resolve env invocation before agent: %w", findErr)
	}
	if agent == "" {
		targetIdx, findErr = firstCommandTokenStrict(tokens)
		if findErr != nil {
			return result, fmt.Errorf("cannot resolve command through env wrapper: %w", findErr)
		}
		if targetIdx < 0 {
			return result, fmt.Errorf("could not find an executable in command")
		}
	}
	result.Executable = tokens[targetIdx]
	result.Agent = agent
	targetName := result.Executable

	atShellPrefix := true
	for idx := 0; idx < targetIdx; {
		tok := tokens[idx]
		if strings.EqualFold(baseCommand(tok), "env") {
			invocation, err := envcommand.Parse(tokens[idx+1:], envcommand.Policy{AllowAssignments: true})
			if err != nil {
				return result, fmt.Errorf("cannot resolve env invocation before %s: %w", targetName, err)
			}
			if invocation.CommandIndex < 0 {
				return result, fmt.Errorf("cannot resolve env invocation before %s: env has no command", targetName)
			}
			commandIdx := idx + 1 + invocation.CommandIndex
			if commandIdx > targetIdx {
				return result, fmt.Errorf("cannot resolve env invocation before %s: the target token is an option operand, not env's command", targetName)
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
		result.workingDirKnown = false
		result.preExecDirKnown = false
		idx++
	}
	if result.Agent == ProgramCodex && codexWorkingDirOptionPresent(tokens[targetIdx+1:]) {
		// Do not duplicate Codex's evolving argument parser here. Its own cwd
		// option is enough to disprove the inherited launch directory; dropping
		// correlation preserves exactly-one capture and keeps concurrent starts
		// fail-closed until the rollout header itself can identify them safely.
		// preExecDirKnown is untouched: the option does not move where Codex
		// resolves its home.
		result.workingDirKnown = false
	}
	return result, nil
}

func codexWorkingDirOptionPresent(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-C" || arg == "--cd" || strings.HasPrefix(arg, "-C") || strings.HasPrefix(arg, "--cd=") {
			return true
		}
	}
	return false
}

// firstCommandTokenStrict returns the first executable that can be established
// without understanding arbitrary wrapper grammars. Leading shell assignments
// and nested GNU env invocations are modeled; the first other token is an opaque
// command boundary and is deliberately not guessed through.
func firstCommandTokenStrict(tokens []string) (int, error) {
	idx := 0
	for idx < len(tokens) {
		if _, _, assignment := shellAssignment(tokens[idx]); !assignment {
			break
		}
		idx++
	}
	for idx < len(tokens) && strings.EqualFold(baseCommand(tokens[idx]), "env") {
		invocation, err := envcommand.Parse(tokens[idx+1:], envcommand.Policy{AllowAssignments: true})
		if err != nil {
			return -1, err
		}
		if invocation.CommandIndex < 0 {
			return -1, nil
		}
		idx += 1 + invocation.CommandIndex
	}
	if idx >= len(tokens) {
		return -1, nil
	}
	return idx, nil
}

// agentConfigRoot says where one agent reads its config root: the variable that
// relocates it, and the directory under HOME it defaults to.
type agentConfigRoot struct {
	label      string
	variable   string
	homeSubdir string
	// blankHomeFallsBack is whether a HOME that is set but empty falls through to
	// the OS account home. Codex resolves its home through Rust's home_dir, which
	// ignores an empty HOME; Gemini's os.homedir() returns the empty string
	// as-is, so an empty HOME there is a path af cannot name.
	blankHomeFallsBack bool
}

// osAccountHome is the home directory of the account this process runs as: what
// an agent falls back to when HOME is absent (Rust's home_dir and libuv's
// uv_os_homedir both read the passwd entry). The launched agent is a child of
// this process, so it is the same account. A var so tests can name a home.
//
// It looks the uid up in the account database and nowhere else. user.Current()
// would do, except that a CGO-off build synthesizes a user from the daemon's own
// HOME and USER when the uid has no passwd entry, and that home is exactly what
// the agent cannot read once HOME is cleared: Node's os.homedir() throws there.
// user.LookupId errors instead, and the caller reports the root unknowable.
var osAccountHome = func() (string, error) {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// agentConfigRoots lists the agents whose config root af follows through a
// launch command. CODEX_HOME names the config directory itself, so its default is
// HOME/.codex. GEMINI_CLI_HOME is a HOME-like root that the CLI appends .gemini/
// to (`process.env.GEMINI_CLI_HOME || os.homedir()`, #3387), so its default is
// HOME itself. Both treat an empty value as unset.
var agentConfigRoots = map[string]agentConfigRoot{
	ProgramCodex:  {label: "Codex", variable: "CODEX_HOME", homeSubdir: ".codex", blankHomeFallsBack: true},
	ProgramGemini: {label: "Gemini", variable: "GEMINI_CLI_HOME"},
}

// ConfigRootFromCommand resolves the config root that agent reads when command
// is launched from workingDir. It is the one answer to "where does this launch
// read its config" for every caller: Codex conversation capture reads its rollout
// store under it, and af places its skill under it (#4501).
//
// The agent's variable and HOME are interpreted in the same environment and cwd
// model as GNU env itself. A variable the command does not touch is read from
// this process's environment, which the launch inherits. A value af cannot read
// statically, such as a shell expansion, is an error and never a guess, so a
// caller cannot quietly fall back to the daemon's root while the agent reads
// somewhere else.
func ConfigRootFromCommand(agent, command, workingDir string) (string, error) {
	spec, ok := agentConfigRoots[agent]
	if !ok {
		return "", fmt.Errorf("af does not follow a config root for %q", agent)
	}
	launch, err := CommandEnvironmentFromCommand(command, workingDir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s environment: %w", spec.label, err)
	}
	effective := func(name string) (string, bool, error) {
		override := launch.Override(name)
		if !override.Present {
			value, set := os.LookupEnv(name)
			return value, set, nil
		}
		if !override.Literal {
			return "", false, fmt.Errorf("%s uses shell expansion; use a literal path so af can follow where %s reads its config", name, spec.label)
		}
		return override.Value, override.Set, nil
	}
	// A relative path resolves against the launch cwd. When af could not
	// establish that cwd (a wrapper may chdir before exec) the path has no
	// location af can name, so it is an error, never the worktree.
	resolve := func(name, path string) (string, error) {
		if filepath.IsAbs(path) {
			return filepath.Clean(path), nil
		}
		if !launch.LaunchDirKnown() {
			return "", fmt.Errorf("%s is the relative path %q and af cannot tell which directory the launched command runs in; use an absolute path so af can follow where %s reads its config", name, path, spec.label)
		}
		return filepath.Clean(filepath.Join(launch.WorkingDir, path)), nil
	}

	if root, set, err := effective(spec.variable); err != nil {
		return "", err
	} else if set && root != "" {
		return resolve(spec.variable, root)
	}
	home, set, err := effective("HOME")
	if err != nil {
		return "", err
	}
	blank := set && home == ""
	switch {
	case !set || (blank && spec.blankHomeFallsBack):
		// No HOME reaches the agent (`env -i`, `env -u HOME`, a daemon without
		// one): it reads the OS account home, exactly as it will at launch.
		osHome, herr := osAccountHome()
		if herr != nil || osHome == "" {
			return "", fmt.Errorf("%s and HOME are unset and af could not look up the account home %s falls back to: %v", spec.variable, spec.label, herr)
		}
		home = osHome
	case blank:
		return "", fmt.Errorf("%s is unset and HOME is empty, which %s reads as a relative path af cannot name", spec.variable, spec.label)
	}
	resolved, err := resolve("HOME", home)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, spec.homeSubdir), nil
}

// CodexHomeFromCommand resolves the rollout store the launched command will
// actually use, so receipt/capture callers never silently watch the daemon's
// store while a wrapped Codex writes somewhere else. See ConfigRootFromCommand.
func CodexHomeFromCommand(command, workingDir string) (string, error) {
	return ConfigRootFromCommand(ProgramCodex, command, workingDir)
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
