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
func commandOverridesName(command, agent, trustedWrapper string, names map[string]struct{}) (overrides, provable bool) {
	if command == "" {
		return false, true
	}
	call, ok := singleSimpleCall(command)
	if !ok {
		return false, false
	}
	return callOverridesName(call, agent, trustedWrapper, names, 0)
}

func callOverridesName(call *syntax.CallExpr, agent, trustedWrapper string, names map[string]struct{}, depth int) (overrides, provable bool) {
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
	// A bare `af`, or the EXACT path the launcher generated this handoff with.
	// docker emits /usr/local/bin/af and ssh a staged absolute path, so requiring
	// a bare name refuses af's own launch on those backends — the same
	// name-is-not-provenance problem in reverse: here the path IS trusted and the
	// spelling cannot say so, which is why the caller supplies it.
	if nested, ok := literalAgentServerProgram(call); ok && isTrustedAfBinary(words[0], trustedWrapper) {
		inner, ok := singleSimpleCall(nested)
		if !ok {
			return false, false
		}
		return callOverridesName(inner, agent, trustedWrapper, names, depth+1)
	}

	if isBareName(words[0], "env") {
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
	// Bare name, and NO ARGUMENTS. An agent's own flags can redirect its identity
	// as effectively as the environment can: codex documents
	// `-c cli_auth_credentials_store="keyring"`, which makes it ignore the account
	// directory's auth.json and use the machine-wide keyring instead, and claude's
	// help points at --settings for auth. Enumerating those per agent is the same
	// losing game as enumerating shell forms, one layer down, so a scoped
	// invocation carries no arguments at all (#2983 review).
	if len(words) == 1 {
		if executable, ok := literalShellWord(words[0]); ok && executableIsAgent(executable, agent) {
			return false, true
		}
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
	// ANY mutation, exactly as the shell-assignment branch does. Restricting this
	// to the identity names left the same hole that branch had: `env
	// LD_PRELOAD=./steal.so codex` loads repository code into the agent before it
	// reads the injected root, and `env PATH=. codex` redirects the bare
	// executable this guard requires — turning the provenance rule into a
	// redirection of the operator's PATH by the repository (#2983 review).
	//
	// The two branches now express one rule: a scoped program mutates nothing.
	if len(invocation.Mutations) > 0 {
		return true, true
	}
	// A chdir is a PATH mutation in disguise. `env -C attacker codex` changes the
	// working directory before command lookup, so a preserved relative PATH entry
	// resolves `attacker/bin/codex` — an arbitrary repository executable handed
	// the selected root. Same effect as rewriting PATH, which this already
	// refuses (#2983 review).
	if invocation.Chdir != "" {
		return true, true
	}
	_ = names
	if invocation.CommandIndex < 0 || invocation.CommandIndex >= len(literals) {
		// env with no command to run: nothing launches, so nothing is redirected.
		return false, true
	}
	// env leaves the root alone, so the decision belongs to whatever it runs.
	// The SAME no-argument rule as a direct invocation. Checking only the command
	// operand let `env codex -c 'cli_auth_credentials_store="keyring"'` through,
	// which makes Codex ignore the account's auth.json — the exact bypass the
	// direct branch already refuses, reached one wrapper over (#2983 review).
	if invocation.CommandIndex != len(literals)-1 {
		return false, false
	}
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
	// EXACT, not case-folded. On a case-sensitive filesystem `CODEX` is a
	// different executable from `codex`, and if PATH holds one, /bin/sh runs it
	// with the injected root — an unrelated repository-chosen binary handed the
	// account's credentials. Matching the shell's own name semantics is the only
	// comparison that means anything here.
	return executable == agent
}

// isBareName reports whether a word is exactly the given command name with no
// path component.
//
// A basename match is not provenance: `./env` and `./af` share a name with the
// tools this models and are arbitrary repository-provided executables that would
// receive the selected account root. Requiring a bare name puts PATH resolution —
// the operator's configuration — in charge of which binary runs.
func isBareName(word *syntax.Word, want string) bool {
	value, ok := literalShellWord(word)
	if !ok || strings.Contains(value, "/") {
		return false
	}
	return value == want
}

// isTrustedAfBinary reports whether a word is af's own binary: a bare `af`, or
// the exact path the launcher generated the handoff with.
//
// Exact, never a basename. `./af` and `/repo/af` are arbitrary repository files
// that would receive the selected account root; the trusted path is compared
// whole precisely so a shared name proves nothing.
func isTrustedAfBinary(word *syntax.Word, trustedWrapper string) bool {
	if isBareName(word, "af") {
		return true
	}
	if trustedWrapper == "" {
		return false
	}
	value, ok := literalShellWord(word)
	return ok && value == trustedWrapper
}
