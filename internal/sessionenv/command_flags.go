package sessionenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"mvdan.cc/sh/v3/syntax"
)

// AgentForCommand returns a supported agent only when the complete command is
// one literal invocation of that agent (optionally through env or exec). Nested
// agent-server commands are never classified from strings; Docker/SSH use the
// effect-bound exec protocol instead. This is the IDENTITY classifier ("what
// does this command run"); the credential grant a process receives is gated by
// GrantAgentForCommand, which layers operator attribution on top of it. The
// identity answer still feeds account boundaries, so an agent-looking argument
// to an arbitrary executable must never match.
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
// (the gate uses SplitShellTokens + baseCommand, this surface uses mvdan.cc/sh +
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
	agent      string
	executable string
	// wrapper is the env executable spelling the invocation ran through (""
	// for a direct invocation). Its resolution is part of the grant's
	// attribution surface: `env /opt/codex` names an operator file, but a bare
	// `env` resolving into the repository runs repository code with the grant.
	wrapper string
	// executableResolutionChanged marks the environment changes that redirect
	// executable RESOLUTION — PATH assignment/removal, environment clearing,
	// env chdir. The grant predicate refuses them because POSIX lookup then
	// selects a file the command string cannot attribute. Non-resolution
	// assignments (TERM, ANTHROPIC_BASE_URL, agent cloud selectors) stay
	// tolerated: a bare string cannot tell an operator's documented override
	// apart from a repo-authored one, and checked-in program_overrides are
	// refused assignment-carrying values at load instead (#5265 review).
	executableResolutionChanged bool
	// environmentMutated records ANY command-local environment change: a
	// shell assignment prefix or an env-wrapper mutation, unset, clear, or
	// chdir. The launch predicate does not consult it — config load does, to
	// refuse checked-in program_overrides values that set environment (#5265
	// review).
	environmentMutated bool
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
	wrapper := ""
	resolutionChanged := shellAssignmentsChangeExecutableResolution(call.Assigns)
	var envMutations []envcommand.Mutation
	envClearedOrMoved := false
	if envMatch(args[0]) {
		invocation, err := envcommand.Parse(args[1:], envcommand.Policy{AllowAssignments: true})
		if err != nil || invocation.CommandIndex < 0 {
			return agentCommand{}, false
		}
		wrapper = args[0]
		executable = args[1+invocation.CommandIndex]
		envMutations = invocation.Mutations
		envClearedOrMoved = invocation.ClearEnvironment || invocation.Chdir != ""
		resolutionChanged = resolutionChanged || envChangesExecutableResolution(invocation)
	}
	agent := supportedAgent(executable)
	return agentCommand{
		agent:                       agent,
		executable:                  executable,
		wrapper:                     wrapper,
		executableResolutionChanged: resolutionChanged,
		environmentMutated:          envClearedOrMoved || len(call.Assigns) > 0 || len(envMutations) > 0,
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
// material (docker mounts, hook environments — via the remote variant
// below). It returns the supported agent command invokes ONLY when the
// file POSIX resolution selects is attributable to the operator — a bare
// name whose first PATH hit is an absolute directory outside workDir or
// whose all-absolute PATH walk never lands a hit (exec can then only fail,
// never run repository content), or an absolute path outside workDir. Any
// spelling that lets the checked-out repository pick the file earns no
// grant, because program_overrides is an in-repo config key and a repo can
// therefore name an executable it ships (#5265):
//
//   - a RELATIVE executable (./codex, bin/codex, ../codex) resolves against
//     the pane's working directory;
//   - a command that respells resolution (a PATH=… assignment, env PATH=…,
//     env -u PATH, env -i, env -C) selects the directory itself. Other
//     command-local assignments cannot be told apart from an operator's own
//     override by the string alone, so checked-in config is refused them at
//     load (config.LoadInRepoConfig) instead;
//   - an env WRAPPER that itself fails attribution: a bare env resolving
//     through a repository-controlled PATH entry runs repository code no
//     matter what its operand names;
//   - an inherited PATH whose walk reaches an empty or relative entry
//     before the executable resolves a bare name into or relative to the
//     pane's working directory;
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
	return grantAgentForCommand(command, environ, workDir)
}

// GrantAgentForRemoteCommand is GrantAgentForCommand for a provisioner whose
// resolved executable lives on a filesystem the host cannot enumerate — a
// docker image's rootfs or an ssh target. The remote PATH walk cannot be
// stat'ed from here, so a bare name is attributed the way it was before the
// grant predicate existed: the remote's PATH is image or sshd state, which the
// repository does not control through program_overrides. Every spelling the
// repository CAN control — a relative or in-workspace executable, a
// resolution-mutating command — is denied exactly as it is locally.
func GrantAgentForRemoteCommand(command, workDir string) string {
	return grantAgentForCommand(command, nil, workDir)
}

func grantAgentForCommand(command string, environ []string, workDir string) string {
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
// answers "" only when the command is not one literal simple call at all —
// the executable is reported whether or not it classifies as a supported
// agent, because the in-repo refusal applies to absolute spellings wholesale:
// LoadInRepoConfig uses it to refuse any checked-in program_overrides value
// that selects an absolute executable: a repository spelling is not trusted
// provenance for a file outside it (#5265).
func AgentCommandExecutable(command string) string {
	invocation, _ := literalAgentCommand(command, isTrustedEnvExecutable)
	return invocation.executable
}

// AgentCommandSetsEnvironment reports whether command sets command-local
// environment — a shell assignment prefix (LD_PRELOAD=./x.so codex) or an
// env-wrapper mutation (env NAME=…, env -u, env -i, env -C). LoadInRepoConfig
// uses it to refuse checked-in program_overrides values that set environment:
// the launch grant cannot tell an operator's documented override
// (ANTHROPIC_BASE_URL=… claude in a global config) apart from a
// repository-authored one, so the untrusted layer is refused at load while
// operator layers keep the power (#5265 review).
func AgentCommandSetsEnvironment(command string) bool {
	invocation, _ := literalAgentCommand(command, isTrustedEnvExecutable)
	return invocation.environmentMutated
}

// executableAttributableToOperator reports whether POSIX resolution of the
// parsed invocation's executables can only select files the operator — not
// the checked-out repository — controls. Both the agent executable and, when
// the invocation runs through env, the wrapper itself must attribute: `env
// /opt/codex` names an operator file, but a bare `env` resolving into the
// repository runs repository code with the grant (#5265 review).
func executableAttributableToOperator(invocation agentCommand, environ []string, workDir string) bool {
	if invocation.executableResolutionChanged {
		return false
	}
	if invocation.wrapper != "" && !spellingAttributableToOperator(invocation.wrapper, environ, workDir) {
		return false
	}
	return spellingAttributableToOperator(invocation.executable, environ, workDir)
}

// spellingAttributableToOperator is the attribution rule for one executable
// spelling. A path-qualified executable names its file directly: only an
// absolute path is operator-attributable, and not one inside the session's
// working tree, which is repository content whatever its spelling. A bare
// name resolves through PATH: the inherited environment is the attribution
// channel, so the first entry holding the executable must be absolute and
// outside workDir — and an all-absolute walk that finds no hit attributes
// too, since exec can then only fail rather than land in the repository.
func spellingAttributableToOperator(executable string, environ []string, workDir string) bool {
	if strings.Contains(executable, "/") {
		return filepath.IsAbs(executable) && pathOutsideWorkDir(executable, workDir)
	}
	return searchPathAttributesExecutable(environ, executable, workDir)
}

// pathOutsideWorkDir reports whether path resolves outside workDir — outside
// the checked-out tree, where repository content cannot supply the file.
// ResolveForCompare canonicalizes both sides, so a symlinked worktree root or
// a symlinked target cannot evade containment. An unresolvable workDir denies
// rather than letting an unknowable containment answer default open; an empty
// workDir leaves the check open for a caller that cannot know the directory.
func pathOutsideWorkDir(path, workDir string) bool {
	if workDir == "" {
		return true
	}
	absDir, err := filepath.Abs(workDir)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	resolvedDir := pathutil.ResolveForCompare(absDir)
	// LEXICAL containment is checked alongside RESOLVED containment: a
	// spelling inside the tree names repository content even when the file it
	// selects resolves out — an in-tree symlink to /bin/sh still lets a later
	// commit repoint it before exec. Containment is tested against workDir as
	// spelled and as resolved, so a workDir itself spelled through a link
	// cannot slide a spelling past the lexical half either.
	if pathutil.IsAtOrInside(absPath, absDir) ||
		pathutil.IsAtOrInside(absPath, resolvedDir) ||
		pathutil.IsAtOrInside(pathutil.ResolveForCompare(absPath), resolvedDir) {
		return false
	}
	return true
}

// searchPathAttributesExecutable models execvp's real walk of environ's PATH:
// directories are tried in order and the first one holding the executable
// wins, so the grant follows when that first hit is absolute AND outside
// workDir. An empty or relative entry resolves in or relative to the working
// directory — repository content — and denies before any absolute hit, while
// an absolute entry that does not hold the file is skipped exactly as execvp
// skips it (ENOENT and EACCES alike continue the walk). An ALL-ABSOLUTE walk
// that never holds the executable still grants: resolution can then only
// fail, never land in the repository — nothing repo-controlled runs, which is
// what the grant exists to prevent. An absent PATH resolves through execvp's
// compiled-in default, which is absolute and operator-controlled.
//
// The stat is advisory only — the filesystem can change between this check
// and the shell's own resolution at exec (TOCTOU). Exploiting the gap needs
// the file to vanish from the winning absolute directory and reappear under a
// later empty/relative entry inside the race window, which requires write
// access to an operator-controlled PATH directory — outside the repository's
// reach by definition.
func searchPathAttributesExecutable(environ []string, name, workDir string) bool {
	searchPath, found := firstEnvValue(environ, "PATH")
	if !found {
		return true
	}
	for _, dir := range strings.Split(searchPath, ":") {
		if !filepath.IsAbs(dir) {
			return false
		}
		if !pathEntryHoldsExecutable(dir, name) {
			continue
		}
		return pathOutsideWorkDir(filepath.Join(dir, name), workDir)
	}
	return true
}

// pathEntryHoldsExecutable reports whether dir contains an executable file
// named name — the local-filesystem stat the search-path walk uses for
// execvp's "first entry that holds it" question. Mode bits alone are not the
// answer: execvp's walk is EACCES-aware and skips a file it cannot actually
// execute, so the probe is the daemon's own effective access — a bit-check
// that ignores it would attribute a hit the real exec never makes and let
// resolution fall through to a later relative entry (#5265 review).
func pathEntryHoldsExecutable(dir, name string) bool {
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() &&
		info.Mode().Perm()&0o111 != 0 && executableAccess(path)
}

// firstEnvValue returns the value of the FIRST name= entry in environ — the
// one getenv, and therefore execvp's PATH walk, honors when an environment
// carries a name twice.
func firstEnvValue(environ []string, name string) (string, bool) {
	for _, entry := range environ {
		if value, found := strings.CutPrefix(entry, name+"="); found {
			return value, true
		}
	}
	return "", false
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
//
// The selector's value is parsed under BOTH POSIX and Bash and the verdict is
// only returned when the two variants agree. A command-local assignment whose
// value is dialect-dependent — the bug report's $'0' / $"0" family — tokenizes
// under POSIX as Lit{"$"}+SglQuoted{Dollar:false} (literalizing to "$0", which
// flagValueEnabled calls truthy) while under Bash it tokenizes as a single
// SglQuoted{Dollar:true} (rejected by the Dollar guard in
// appendLiteralShellPart). The /bin/sh -c that finally runs the command expands
// such a word differently per host: bash-as-sh turns $'0' into 0 (falsy — mode
// OFF), while dash leaves it as the literal "$0" (truthy).
//
// The operator DID write an explicit assignment to the selector, so the
// command-local value must override whatever the ambient environment carries —
// otherwise `ResolveAuthSelectors` falls back to the ambient value and a
// pre-existing `CLAUDE_CODE_USE_BEDROCK=1` in the environment would re-admit
// the AWS credential group to a session the running shell started with the mode
// off (bash-as-sh expands $'0' to 0). A dialect-dependent word is therefore
// unprovable and fails closed as found-but-disabled (found=true, enabled=false):
// the selector was set on the command, so the ambient no longer speaks for it,
// but the value cannot be proven enabling, so it reads as disabled. This is the
// documented invariant ("a dynamic value is an explicit but unprovable override
// and therefore fails closed as disabled"), applied with the same dual-variant
// treatment commandMutatesAccountEnvironment already uses for the package's
// other dialect-dependent validators.
func commandEnvironmentFlagState(command, agent, name string) (found, enabled bool) {
	if strings.TrimSpace(command) == "" || agent == "" {
		return false, false
	}
	posix, ok := singleCallVariant(command, false, syntax.LangPOSIX)
	if !ok {
		return false, false
	}
	bash, ok := singleCallVariant(command, false, syntax.LangBash)
	if !ok {
		return false, false
	}
	posixFound, posixEnabled := directAgentFlagState(posix, agent, name)
	bashFound, bashEnabled := directAgentFlagState(bash, agent, name)
	if posixFound != bashFound || posixEnabled != bashEnabled {
		// The selector IS assigned on the command, but its value is dialect-
		// dependent and cannot be proven enabling. Report it as found-but-disabled
		// so the ambient does not leak back in: ResolveAuthSelectors overrides the
		// ambient with disabled, and CommandEnablesCloudCredentials (found &&
		// enabled) still reports disabled.
		return true, false
	}
	return posixFound, posixEnabled
}

func singleSimpleCall(command string) (*syntax.CallExpr, bool) {
	return singleCallVariant(command, false, syntax.LangPOSIX)
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
	return singleCallVariant(command, allowRedirections, syntax.LangPOSIX)
}

// singleCallVariant is singleCall parameterized by a parser dialect. The
// selector path parses under BOTH LangPOSIX and LangBash and fails closed on
// disagreement (see commandEnvironmentFlagState); the other callers stay on the
// POSIX variant they always used by going through singleCall.
func singleCallVariant(command string, allowRedirections bool, variant syntax.LangVariant) (*syntax.CallExpr, bool) {
	file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
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

	// An argument after the agent cannot change the agent's environment, so a
	// dollar-quoted ($'…') argument — static text whose only dialect dependence
	// is how it expands — must not poison a selector that is itself
	// unambiguous (#5652). Dynamic arguments still fail closed. Everything
	// that can change the environment (assignment prefixes, env's own words up
	// to the agent) is held to the strict literal rule.
	assignsLiteral := assignmentsAreLiteral(call.Assigns)
	if isTrustedEnvWord(words[0]) {
		found, enabled, commandIndex, ok := envAgentFlagState(call.Assigns, words[1:], agent, name)
		if !ok || !found {
			return false, false
		}
		if !assignsLiteral {
			return true, false
		}
		if _, literal := literalCommandArgs(words[:commandIndex+2]); !literal ||
			!argsAreStatic(words[commandIndex+2:]) {
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
	if !assignsLiteral || !argsAreStatic(words[1:]) {
		return true, false
	}
	return true, enabled
}

// argsAreStatic reports whether every word is fixed text: literal, or an ANSI-C
// $'…' quote (whose content is never expanded). Locale $"…" quotes and any
// expansion are not static.
func argsAreStatic(words []*syntax.Word) bool {
	for _, word := range words {
		if word == nil {
			return false
		}
		for _, part := range word.Parts {
			if sq, ok := part.(*syntax.SglQuoted); ok && sq.Dollar {
				continue
			}
			var sb strings.Builder
			if !appendLiteralShellPart(&sb, part) {
				return false
			}
		}
	}
	return true
}

func envAgentFlagState(assignments []*syntax.Assign, words []*syntax.Word, agent, name string) (found, enabled bool, commandIndex int, ok bool) {
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
		return false, false, 0, false
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
	return found, enabled, invocation.CommandIndex, true
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
		// --branch is the daemon-named branch (#5571 W2.4): any non-empty value,
		// since it only names a git ref and is never executed.
		if args[idx] == "--branch" && idx+1 < len(args) && args[idx+1] != "" {
			idx += 2
			continue
		}
		// --adopt-worktree names the preserved worktree an archived session is
		// reattached to (#5571 W8.6): an absolute path the daemon proved present,
		// bound as the workspace and never executed.
		if args[idx] == "--adopt-worktree" && idx+1 < len(args) && strings.HasPrefix(args[idx+1], "/") {
			idx += 2
			continue
		}
		// --policy-file names the daemon-resolved operator policy to READ
		// (#5571 W2.5): an absolute path, never executed.
		if args[idx] == "--policy-file" && idx+1 < len(args) && strings.HasPrefix(args[idx+1], "/") {
			idx += 2
			continue
		}
		if args[idx] != "--session-env" || idx+1 >= len(args) || !validName(args[idx+1]) {
			return "", false
		}
		idx += 2
	}
	return program, true
}

func callIsLiteral(call *syntax.CallExpr) bool {
	if !assignmentsAreLiteral(call.Assigns) {
		return false
	}
	_, literal := literalCommandArgs(call.Args)
	return literal
}

func assignmentsAreLiteral(assigns []*syntax.Assign) bool {
	for _, assignment := range assigns {
		if assignment == nil || assignment.Name == nil || assignment.Append || assignment.Naked ||
			assignment.Index != nil || assignment.Array != nil || assignment.Value == nil {
			return false
		}
		if _, ok := literalShellWord(assignment.Value); !ok {
			return false
		}
	}
	return true
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
	// Read the word the way the shell will pass it, not as its written text
	// (#4523): literalArgvWord removes unquoted backslashes (`\env` is env,
	// `CODEX\_HOME` is CODEX_HOME) and fails closed on what the shell expands
	// at run time — glob characters, a leading tilde, brace expansion and
	// $'…'/$"…" — so a word that decides a command name, an option or an
	// assignment is never judged by raw source bytes.
	text, _, ok := literalArgvWord(word)
	return text, ok
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
