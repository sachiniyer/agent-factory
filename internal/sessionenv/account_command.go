package sessionenv

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// commandOverridesName reports whether a resolved program could redirect a
// variable away from the value this package injects, and whether that answer is
// PROVABLE.
//
// It exists because the launch runs the program through `/bin/sh -c`, which
// applies command-local assignments AFTER the session environment is installed.
// program_overrides is reachable from a repository's checked-in config, so
// without this a repo could silently redirect whose quota a session spends
// (#2983).
//
// # It proves GOOD shapes; it does not hunt bad ones
//
// The first version enumerated the forms that could redirect a variable and
// treated everything else as safe. That is the wrong polarity for a boundary
// like this, and it leaked twice in review: `env --unset=NAME`, the attached
// `-uNAME`, and the bare `-` (which GNU env documents as implying `-i`) all
// walked straight through, and so did `sh -c '...'` — which parses as a
// perfectly ordinary call whose ARGUMENT is another whole program.
//
// A denylist here is a promise to have thought of every spelling, in a shell,
// forever. So this recognises exactly two shapes — a direct invocation of the
// agent, and a modelled `env` wrapper around one — and reports everything else
// as UNPROVABLE. New syntax then arrives as a refusal to scope rather than as a
// silent bypass, which is the direction this repo can afford to be wrong in.
func commandOverridesName(command, agent string, names map[string]struct{}) (overrides, provable bool) {
	if command == "" {
		return false, true
	}
	call, ok := singleSimpleCall(command)
	if !ok {
		return false, false
	}
	return callOverridesName(call, agent, names, 0)
}

func callOverridesName(call *syntax.CallExpr, agent string, names map[string]struct{}, depth int) (overrides, provable bool) {
	if depth > maxNestedProgramDepth {
		return false, false
	}
	// Shell-parsed assignment prefixes: `CODEX_HOME=/other codex`.
	for _, assign := range call.Assigns {
		// ANY assignment, not only the identity names. LD_PRELOAD=./steal.so codex
		// loads repository-controlled code into the real agent before it reads the
		// injected root, so it can read the account's credentials without ever
		// touching a variable this package classifies. An allowlist of "harmless"
		// variables is another standing promise to have thought of every one; a
		// scoped session's program simply carries no assignments (#2983 review).
		if assign != nil && assign.Name != nil {
			return true, true
		}
		_ = names
	}

	words := call.Args
	if len(words) > 0 && wordEquals(words[0], "exec") {
		words = words[1:]
		if len(words) > 0 && wordEquals(words[0], "--") {
			words = words[1:]
		}
	}
	if len(words) == 0 {
		return false, true
	}
	// After `exec`, an assignment is a leading WORD rather than a parsed
	// assignment: `exec CODEX_HOME=/other codex`.
	for _, word := range words {
		assigned, ok := shellWordAssignmentName(word)
		if !ok {
			break
		}
		_ = assigned
		return true, true
	}

	// A nested agent-server program carries its own command, and is the one
	// wrapper this package already models.
	if nested, ok := literalAgentServerProgram(call); ok {
		inner, ok := singleSimpleCall(nested)
		if !ok {
			return false, false
		}
		return callOverridesName(inner, agent, names, depth+1)
	}

	if wordBaseEquals(words[0], "env") {
		return envOverridesName(words[1:], agent, names)
	}

	// Anything else is unprovable, and that includes commands that look
	// completely ordinary. `sh -c '...'` parses as a single simple call whose
	// argument is another entire program this cannot see into; so does any
	// interpreter, any wrapper script, and any binary whose behaviour is not
	// modelled here. The caller refuses rather than assuming.
	//
	// A DIRECT invocation of a known agent is the one provable case: it consumes
	// its arguments itself and starts no second program construction.
	// The WHOLE call must be literal, not just the executable word. A command
	// substitution in an argument — `codex "$(unset CODEX_HOME; codex exec …)"` —
	// is evaluated by /bin/sh BEFORE the outer agent starts, so checking only
	// words[0] proves nothing about what runs first.
	if !callIsLiteral(call) {
		return false, false
	}
	if executable, ok := literalShellWord(words[0]); ok && executableIsAgent(executable, agent) {
		return false, true
	}
	return false, false
}

// envOverridesName decides an `env` wrapper through envcommand.Parse, the
// closed-set parser this repo already uses for exactly this problem, instead of
// re-deriving GNU env's option grammar here.
//
// Reusing it is the point: the forms that leaked past the hand-rolled version —
// `--unset=NAME`, `-uNAME`, a bare `-` — are ones that parser already models,
// and a second implementation of the same grammar is a second thing to keep in
// step. Anything Parse rejects is unprovable by definition, because Parse's own
// contract is to refuse rather than silently model a different environment.
func envOverridesName(args []*syntax.Word, agent string, names map[string]struct{}) (overrides, provable bool) {
	literals, ok := literalCommandArgs(args)
	if !ok {
		return false, false
	}
	invocation, err := envcommand.Parse(literals, envcommand.Policy{AllowAssignments: true})
	if err != nil {
		return false, false
	}
	// `-i`, and the bare `-` that GNU documents as implying it, drop the injected
	// root along with everything else.
	if invocation.ClearEnvironment {
		return true, true
	}
	for _, mutation := range invocation.Mutations {
		if _, refused := names[mutation.Name]; refused {
			// Set to something else, or unset outright — either way the injected
			// value is not what the agent will see.
			return true, true
		}
	}
	if invocation.CommandIndex < 0 || invocation.CommandIndex >= len(literals) {
		// env with no command to run: nothing launches, so nothing is redirected.
		return false, true
	}
	// env leaves the root alone, so the decision belongs to whatever it runs.
	if executableIsAgent(literals[invocation.CommandIndex], agent) {
		return false, true
	}
	return false, false
}

// executableIsAgent reports whether an already-tokenized executable operand IS
// the account's agent.
//
// Two things it deliberately does not do. It does not re-parse the operand as a
// command string: `env './codex wrapper'` yields ONE executable path whose name
// contains a space, and handing that to a command parser splits it and reports
// `./codex` — while the shell runs a repository-provided file called
// `codex wrapper`. And it does not accept just any known agent: a cross-agent
// override (`account.Agent` claude, command `codex`) would otherwise be called
// provable, after which the injected CLAUDE_CONFIG_DIR means nothing to codex and
// it authenticates from its own default home (#2983 review).
func executableIsAgent(executable, agent string) bool {
	if executable == "" || agent == "" {
		return false
	}
	// A BARE name only. `./codex` shares a basename with the real CLI and is an
	// arbitrary repository-provided file: the shell would hand it the selected
	// root, letting it read the account's credentials or unset the variable and
	// then exec the real agent. A basename is not provenance. Requiring no path
	// separator makes PATH resolution the thing that picks the binary, which is
	// the operator's configuration rather than the repository's (#2983 review).
	if strings.ContainsRune(executable, filepath.Separator) || strings.Contains(executable, "/") {
		return false
	}
	return strings.EqualFold(executable, agent)
}
