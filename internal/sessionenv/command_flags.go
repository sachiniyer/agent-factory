package sessionenv

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"mvdan.cc/sh/v3/syntax"
)

// AgentForCommand returns a supported agent only when the complete command is
// one literal invocation of that agent (optionally through env or exec). Nested
// agent-server commands are never classified from strings; Docker/SSH use the
// effect-bound exec protocol instead. This result grants credentials to a
// process, so an agent-looking argument to an arbitrary executable must never
// match.
func AgentForCommand(command string) string {
	invocation, ok := literalAgentCommand(command, isTrustedEnvExecutable)
	if !ok {
		return ""
	}
	return invocation.agent
}

// AgentNamespaceForCommand derives the agent NAMESPACE from a command for the
// non-credential caller that resolves an already-validated account's directory
// (vscodeAccountScopeForInstance). Unlike AgentForCommand it recognizes any env
// wrapper whose basename is env — including operator-supplied path-qualified
// spellings such as /usr/local/bin/env — using the same env-basename rule the
// create-time gate tmux.DetectAgentExecutable keeps, so a path-qualified env
// wrapper the gate accepted onto a pre-#4356 session stays classifiable on
// restore. This shares only the env-basename rule, not a parser:
// DetectAgentExecutable and AgentNamespaceForCommand are otherwise independent
// (the gate uses splitShellTokens + baseCommand, this surface uses mvdan.cc/sh +
// filepath.Base) and agree on the env-wrapper spellings the #4356 fix targets,
// not on every command.
//
// It grants no credentials. The agent namespace it returns names a directory
// under the af home the operator already selected an account in; nothing about
// this call hands a forked process the credentials the directory holds. A
// caller that filters or installs credentials for an exec must use
// AgentForCommand (or credentialAgentForCommand) instead, which keep the strict
// isTrustedEnvExecutable env set so a repository-controlled ./env cannot turn an
// agent-looking argument into a grant.
func AgentNamespaceForCommand(command string) string {
	invocation, ok := literalAgentCommand(command, isEnvExecutableByBase)
	if !ok {
		return ""
	}
	return invocation.agent
}

type agentCommand struct {
	agent                       string
	executable                  string
	executableResolutionChanged bool
}

// literalAgentCommand parses one literal agent invocation, optionally through an
// env wrapper. envMatch decides which spellings count as the env wrapper: the
// strict isTrustedEnvExecutable for credential-bearing callers, and the lenient
// isEnvExecutableByBase for namespace-only callers that share the env-basename
// rule tmux.DetectAgentExecutable keeps for env-wrapper spellings.
func literalAgentCommand(command string, envMatch func(string) bool) (agentCommand, bool) {
	if strings.TrimSpace(command) == "" {
		return agentCommand{}, false
	}
	call, ok := singleSimpleCall(command)
	if !ok || !callIsLiteral(call) {
		return agentCommand{}, false
	}
	// Detection, so the separator is ignored: `exec -- claude` is still a command
	// about claude, and the account boundary refuses it with its own message.
	words, _ := stripExecPrefix(call.Args)
	args, _ := literalCommandArgs(words)
	if len(args) == 0 {
		return agentCommand{}, false
	}
	executable := args[0]
	resolutionChanged := shellAssignmentsChangeExecutableResolution(call.Assigns)
	if envMatch(args[0]) {
		invocation, err := envcommand.Parse(args[1:], envcommand.Policy{AllowAssignments: true})
		if err != nil || invocation.CommandIndex < 0 {
			return agentCommand{}, false
		}
		executable = args[1+invocation.CommandIndex]
		resolutionChanged = resolutionChanged || envChangesExecutableResolution(invocation)
	}
	agent := supportedAgent(executable)
	return agentCommand{
		agent:                       agent,
		executable:                  executable,
		executableResolutionChanged: resolutionChanged,
	}, agent != ""
}

// credentialAgentForCommand is the fail-closed classifier for an untrusted
// resolved program. Unlike AgentForCommand it preserves the executable token as
// identity-bearing data and admits only a bare supported-agent name whose
// resolution still uses the inherited operator environment. A slash selects an
// untrusted file directly; PATH changes, env clearing or PATH removal, and env
// chdir can select one indirectly.
//
// The residual accepted set is a literal bare agent invocation, optionally
// preceded by exec or one of the modelled system env spellings, with literal
// non-resolution assignments such as TERM, LANG, and agent cloud selectors.
// Those remain accepted because POSIX executable lookup does not consult them.
// Every path-qualified or resolution-changing form is deliberately
// credential-free: this boundary receives no trusted provenance for the file it
// selects. The command still launches, and an operator can explicitly authorize
// required names through session_env_passthrough.
func credentialAgentForCommand(command string) string {
	invocation, ok := literalAgentCommand(command, isTrustedEnvExecutable)
	if !ok || strings.Contains(invocation.executable, "/") || invocation.executableResolutionChanged {
		return ""
	}
	return invocation.agent
}

// GrantAgentForCommand is the ONE predicate every credential-grant site
// shares: the tmux launcher (session_env.go), the pane exec shim
// (execInvocation), and the backend provisioners that install credential
// material (docker mounts, hook environments). It returns the supported
// agent command invokes ONLY when the file POSIX resolution selects is
// attributable to the operator — a bare name resolved through an
// all-absolute inherited PATH, or an absolute path outside workDir. Any
// spelling that lets the checked-out repository pick the file earns no
// grant, because program_overrides is an in-repo config key and a repo can
// therefore name an executable it ships (#5265):
//
//   - a RELATIVE executable (./codex, bin/codex, ../codex) resolves against
//     the pane's working directory;
//   - a command that respells resolution (a PATH=… assignment, env PATH=…,
//     env -u PATH, env -i, env -C) selects the directory itself;
//   - a PATH in environ containing an empty or relative entry resolves a
//     bare name into or relative to the pane's working directory;
//   - an ABSOLUTE path inside workDir is inside the checked-out tree.
//
// AgentForCommand remains the identity classifier ("what does this command
// run"); GrantAgentForCommand answers the different question "may this
// command carry the operator's credential grant". credentialAgentForCommand
// stays deliberately stricter — it serves the effect-bound agent-server
// handoff, where the program string crosses a wire and even an absolute path
// has no trusted provenance.
//
// Callers pass the environment the command's resolution actually reads (the
// launcher's own environment, which is what the filtered pane inherits) and
// the directory the pane launches in. A caller that cannot know the
// directory — a remote hook host — passes "" and still denies every
// non-absolute spelling.
func GrantAgentForCommand(command string, environ []string, workDir string) string {
	invocation, ok := literalAgentCommand(command, isTrustedEnvExecutable)
	if !ok {
		return ""
	}
	if !executableAttributableToOperator(invocation, environ, workDir) {
		return ""
	}
	return invocation.agent
}

// AgentCommandExecutable reports the executable token af's literal agent
// resolution selected in command — including through a trusted env wrapper —
// for a caller that must judge the FILE, not only the attributed agent. It
// answers "" when the command is not one literal supported-agent invocation.
// LoadInRepoConfig uses it to refuse a checked-in program_overrides value
// that points the credential grant at an absolute path inside the repository
// itself (#5265).
func AgentCommandExecutable(command string) string {
	invocation, ok := literalAgentCommand(command, isTrustedEnvExecutable)
	if !ok {
		return ""
	}
	return invocation.executable
}

// executableAttributableToOperator reports whether POSIX resolution of the
// parsed invocation's executable can only select a file the operator — not
// the checked-out repository — controls.
func executableAttributableToOperator(invocation agentCommand, environ []string, workDir string) bool {
	if strings.Contains(invocation.executable, "/") {
		// A path-qualified executable names its file directly: only an
		// absolute path is operator-attributable, and not one inside the
		// session's own working tree, which is repository content whatever
		// its spelling. ResolveForCompare canonicalizes both sides, so a
		// symlinked worktree root cannot evade containment.
		if !filepath.IsAbs(invocation.executable) {
			return false
		}
		if workDir == "" {
			return true
		}
		resolvedDir, err := filepath.Abs(workDir)
		if err != nil {
			// The working tree cannot be located, so the executable cannot be
			// proven outside it. Deny the grant rather than let an unknowable
			// containment answer default open.
			return false
		}
		return !pathutil.IsAtOrInside(
			pathutil.ResolveForCompare(invocation.executable),
			pathutil.ResolveForCompare(resolvedDir))
	}
	// A bare name resolves through PATH: the inherited environment is the
	// attribution channel. A command that already respelled resolution, or
	// an inherited PATH with an empty or relative entry, lets repository
	// content supply the file.
	if invocation.executableResolutionChanged {
		return false
	}
	return searchPathAbsolute(environ)
}

// searchPathAbsolute reports whether every entry in environ's PATH is
// absolute — the condition under which execvp's resolution of a bare name
// cannot land in or relative to the working directory. An absent PATH
// resolves through execvp's compiled-in default, which is absolute; an empty
// PATH or any empty entry means the working directory.
func searchPathAbsolute(environ []string) bool {
	// Every PATH= entry is checked rather than only the last, which is the one
	// exec semantics would honor: a duplicate-PATH environment is pathological
	// enough that failing closed costs nothing, while trusting an early safe
	// value and missing a later relative one would open the resolution the
	// check exists to close.
	for _, entry := range environ {
		value, found := strings.CutPrefix(entry, "PATH=")
		if !found {
			continue
		}
		for _, dir := range strings.Split(value, ":") {
			if !filepath.IsAbs(dir) {
				return false
			}
		}
	}
	return true
}

func shellAssignmentsChangeExecutableResolution(assignments []*syntax.Assign) bool {
	for _, assignment := range assignments {
		if assignment != nil && assignment.Name != nil && assignment.Name.Value == "PATH" {
			return true
		}
	}
	return false
}

func envChangesExecutableResolution(invocation envcommand.Invocation) bool {
	// Clearing the environment or changing cwd changes lookup whenever PATH is
	// absent or contains a relative entry. Neither can be proven harmless here.
	if invocation.ClearEnvironment || invocation.Chdir != "" {
		return true
	}
	for _, mutation := range invocation.Mutations {
		if mutation.Name == "PATH" {
			return true
		}
	}
	return false
}

func supportedAgent(command string) string {
	agent := strings.ToLower(filepath.Base(command))
	if _, ok := agentNames[agent]; ok {
		return agent
	}
	return ""
}

// commandEnvironmentFlagState recognizes a selector override only when the
// command is one simple invocation of the selected agent. In particular, an
// agent-looking word used as data, a compound command, a redirect, or a second
// possible execution path can never widen the credential allowlist. Nothing is
// expanded or executed.
func commandEnvironmentFlagState(command, agent, name string) (found, enabled bool) {
	if strings.TrimSpace(command) == "" || agent == "" {
		return false, false
	}
	call, ok := singleSimpleCall(command)
	if !ok {
		return false, false
	}

	return directAgentFlagState(call, agent, name)
}

func singleSimpleCall(command string) (*syntax.CallExpr, bool) {
	return singleCall(command, false)
}

// singleCallIgnoringRedirections is singleSimpleCall for callers that ask about
// the command's WORDS rather than about what af can prove it will do.
//
// A redirection makes a command unprovable — it is why singleSimpleCall refuses
// one — but it says nothing about the exec prefix: `exec -- claude >agent.log`
// still hands dash `--` as the command name, and dash still exits 127. A caller
// that rejected the whole string because it ends in `>agent.log` would answer
// "no separator here" about a command that has one (#3566).
func singleCallIgnoringRedirections(command string) (*syntax.CallExpr, bool) {
	return singleCall(command, true)
}

// singleCall is the one parse behind both. allowRedirections relaxes exactly one
// rule and nothing else, so the two questions cannot drift apart on any of the
// others.
func singleCall(command string, allowRedirections bool) (*syntax.CallExpr, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return nil, false
	}
	stmt := file.Stmts[0]
	if stmt == nil || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown {
		return nil, false
	}
	if !allowRedirections && len(stmt.Redirs) != 0 {
		return nil, false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return nil, false
	}
	return call, true
}

func directAgentFlagState(call *syntax.CallExpr, agent, name string) (found, enabled bool) {
	words, _ := stripExecPrefix(call.Args)
	if len(words) == 0 {
		return false, false
	}

	allLiteral := callIsLiteral(call)
	if isTrustedEnvWord(words[0]) {
		found, enabled, ok := envAgentFlagState(call.Assigns, words[1:], agent, name)
		if !ok || !found {
			return false, false
		}
		if !allLiteral {
			return true, false
		}
		return true, enabled
	}
	if !wordBaseEquals(words[0], agent) {
		return false, false
	}
	found, enabled = callAssignmentFlagState(call.Assigns, name)
	if !found {
		return false, false
	}
	if !allLiteral {
		return true, false
	}
	return true, enabled
}

func envAgentFlagState(assignments []*syntax.Assign, words []*syntax.Word, agent, name string) (found, enabled, ok bool) {
	args := make([]string, 0, len(words))
	dynamicValues := make(map[string]struct{})
	for idx, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			assignmentName, assignment := shellWordAssignmentName(word)
			if assignment {
				value = fmt.Sprintf("%s=AF_DYNAMIC_VALUE_%d", assignmentName, idx)
				dynamicValues[value] = struct{}{}
			} else {
				// Parse still needs a placeholder to locate env's command. If this
				// dynamic word is the command, the placeholder cannot match agent;
				// if it follows the literal command, Parse has already stopped.
				value = fmt.Sprintf("AF_DYNAMIC_WORD_%d", idx)
			}
		}
		args = append(args, value)
	}
	invocation, err := envcommand.Parse(args, envcommand.Policy{AllowAssignments: true})
	if err != nil || invocation.CommandIndex < 0 || invocation.CommandIndex >= len(args) ||
		!strings.EqualFold(filepath.Base(args[invocation.CommandIndex]), agent) {
		return false, false, false
	}

	found, enabled = callAssignmentFlagState(assignments, name)
	if invocation.ClearEnvironment {
		found, enabled = true, false
	}
	for _, mutation := range invocation.Mutations {
		if mutation.Name != name {
			continue
		}
		found = true
		if mutation.Unset {
			enabled = false
			continue
		}
		_, dynamic := dynamicValues[mutation.Name+"="+mutation.Value]
		enabled = !dynamic && flagValueEnabled(mutation.Value)
	}
	return found, enabled, true
}

// agentServerProgram parses the exact argument order generated by the Docker and
// SSH launchers. It deliberately accepts no executable word: the effect-bound
// exec mode always re-execs the CURRENT af binary, so there is no name or path
// here that repository content can substitute.
func agentServerProgram(args []string) (string, bool) {
	if len(args) < 7 || args[0] != "agent-server" {
		return "", false
	}
	if args[1] != "--listen" || args[2] == "" || args[3] != "--repo" || args[4] == "" ||
		args[5] != "--title" || args[6] == "" {
		return "", false
	}
	program := "claude"
	idx := 7
	if idx < len(args) && args[idx] == "--program" {
		if idx+2 >= len(args) || args[idx+1] == "" || args[idx+2] != "--program-resolved" {
			return "", false
		}
		program = args[idx+1]
		idx += 3
	}
	for idx < len(args) {
		if args[idx] != "--session-env" || idx+1 >= len(args) || !validName(args[idx+1]) {
			return "", false
		}
		idx += 2
	}
	return program, true
}

func callIsLiteral(call *syntax.CallExpr) bool {
	for _, assignment := range call.Assigns {
		if assignment == nil || assignment.Name == nil || assignment.Append || assignment.Naked ||
			assignment.Index != nil || assignment.Array != nil || assignment.Value == nil {
			return false
		}
		if _, ok := literalShellWord(assignment.Value); !ok {
			return false
		}
	}
	_, literal := literalCommandArgs(call.Args)
	return literal
}

func callAssignmentFlagState(assignments []*syntax.Assign, name string) (found, enabled bool) {
	// Shell assignment prefixes are applied left-to-right, so the last
	// assignment to the selector decides its value. A dynamic value is an
	// explicit but unprovable override and therefore fails closed as disabled.
	for idx := len(assignments) - 1; idx >= 0; idx-- {
		assignment := assignments[idx]
		if assignment == nil || assignment.Name == nil || assignment.Name.Value != name {
			continue
		}
		if assignment.Append || assignment.Naked || assignment.Index != nil || assignment.Array != nil || assignment.Value == nil {
			return true, false
		}
		value, literal := literalShellWord(assignment.Value)
		return true, literal && flagValueEnabled(value)
	}
	return false, false
}

func shellWordAssignmentName(word *syntax.Word) (string, bool) {
	if word == nil || len(word.Parts) == 0 {
		return "", false
	}
	literal, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return "", false
	}
	name, _, assignment := strings.Cut(literal.Value, "=")
	return name, assignment && validName(name)
}

func wordEquals(word *syntax.Word, want string) bool {
	value, literal := literalShellWord(word)
	return literal && value == want
}

func wordBaseEquals(word *syntax.Word, want string) bool {
	value, literal := literalShellWord(word)
	return literal && strings.EqualFold(filepath.Base(value), want)
}

func isTrustedEnvWord(word *syntax.Word) bool {
	value, literal := literalShellWord(word)
	return literal && isTrustedEnvExecutable(value)
}

// isTrustedEnvExecutable keeps the existing bare form for compatibility: it is
// resolved through the operator's inherited PATH, not a path selected from the
// repository command. The two absolute forms are the conventional root-owned
// system binaries on supported Unix hosts and are strictly less redirectable.
// Every other path stays untrusted; in particular, a repository's ./env and a
// user-writable /tmp/env cannot turn an agent-looking argument into a grant.
func isTrustedEnvExecutable(executable string) bool {
	switch executable {
	case "env", "/bin/env", "/usr/bin/env":
		return true
	default:
		return false
	}
}

// isEnvExecutableByBase is the lenient counterpart that matches any spelling
// whose basename is env, mirroring tmux.DetectAgentExecutable's create-time
// detection. It is the env rule for AgentNamespaceForCommand's namespace-only
// callers: those classify operator-controlled input (instance.AgentProgram is
// stored verbatim from opts.Program) purely to look up an account directory,
// never to grant a credential, so the overwrite-by-./env threat the strict set
// closes does not apply.
func isEnvExecutableByBase(executable string) bool {
	return strings.EqualFold(filepath.Base(executable), "env")
}

func literalCommandArgs(words []*syntax.Word) ([]string, bool) {
	args := make([]string, len(words))
	for idx, word := range words {
		value, ok := literalShellWord(word)
		if !ok {
			return nil, false
		}
		args[idx] = value
	}
	return args, true
}

// literalShellWordWork counts literalShellWord calls when a test sets it. Every
// step of the account-command walk literalizes the word it stands on, so the
// count is the walk's work in word visits, independent of wall-clock time
// (#4966). It panics once calls passes a positive limit, so a walk that turns
// superlinear again fails its test at the budget instead of running for
// minutes. It is nil outside tests.
var literalShellWordWork *workCounter

type workCounter struct {
	calls int
	limit int
}

// errWorkLimitExceeded is literalShellWordWork's panic value.
var errWorkLimitExceeded = errors.New("literalShellWord work limit exceeded")

func literalShellWord(word *syntax.Word) (string, bool) {
	if counter := literalShellWordWork; counter != nil {
		counter.calls++
		if counter.limit > 0 && counter.calls > counter.limit {
			panic(errWorkLimitExceeded)
		}
	}
	if word == nil {
		return "", false
	}
	var value strings.Builder
	for _, part := range word.Parts {
		if !appendLiteralShellPart(&value, part) {
			return "", false
		}
	}
	return value.String(), true
}

func appendLiteralShellPart(value *strings.Builder, part syntax.WordPart) bool {
	switch part := part.(type) {
	case *syntax.Lit:
		value.WriteString(part.Value)
		return true
	case *syntax.SglQuoted:
		if part.Dollar {
			return false
		}
		value.WriteString(part.Value)
		return true
	case *syntax.DblQuoted:
		if part.Dollar {
			return false
		}
		for _, nested := range part.Parts {
			if !appendLiteralShellPart(value, nested) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
