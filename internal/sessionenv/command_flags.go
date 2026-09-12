package sessionenv

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
	"mvdan.cc/sh/v3/syntax"
)

const (
	maxNestedProgramDepth     = 1
	defaultAgentServerProgram = "claude"
)

// AgentForCommand returns a supported agent only when the complete command is
// one literal invocation of that agent (optionally through env or exec). Nested
// agent-server commands require launcher provenance that this generic string API
// deliberately does not have. This result grants credentials to a process, so
// an agent-looking argument to an arbitrary executable must never match.
func AgentForCommand(command string) string {
	return agentForCommandAtDepth(command, "", 0)
}

// agentForTrustedAgentServerCommand is AgentForCommand with the one piece of
// identity a command string cannot carry: the absolute af path the launcher
// installed and used for its agent-server handoff. It is deliberately
// package-private so generic callers cannot turn an argv spelling back into
// provenance. The Docker/SSH exec shim supplies its own executable path.
func agentForTrustedAgentServerCommand(command, trustedWrapper string) string {
	return agentForCommandAtDepth(command, trustedWrapper, 0)
}

func agentForCommandAtDepth(command, trustedWrapper string, depth int) string {
	if depth > maxNestedProgramDepth || strings.TrimSpace(command) == "" {
		return ""
	}
	call, ok := singleSimpleCall(command)
	if !ok || !callIsLiteral(call) {
		return ""
	}
	// Detection, so the separator is ignored: `exec -- claude` is still a command
	// about claude, and the account boundary refuses it with its own message.
	words, _ := stripExecPrefix(call.Args)
	if nested, ok := trustedAgentServerProgram(call, trustedWrapper); ok {
		return agentForCommandAtDepth(nested, trustedWrapper, depth+1)
	}
	args, _ := literalCommandArgs(words)
	if len(args) == 0 {
		return ""
	}
	if isTrustedEnvExecutable(args[0]) {
		invocation, err := envcommand.Parse(args[1:], envcommand.Policy{AllowAssignments: true})
		if err != nil || invocation.CommandIndex < 0 {
			return ""
		}
		return supportedAgent(args[1+invocation.CommandIndex])
	}
	return supportedAgent(args[0])
}

func supportedAgent(command string) string {
	agent := strings.ToLower(filepath.Base(command))
	if _, ok := agentNames[agent]; ok {
		return agent
	}
	return ""
}

// commandEnvironmentFlagState recognizes a selector override only when the
// command is one simple invocation of the selected agent. Nested agent-server
// commands require the separate launcher-provenance entry point below. In
// particular, an agent-looking word used as data, a compound command, a
// redirect, or a second possible execution path can never widen the credential
// allowlist. Nothing is expanded or executed.
func commandEnvironmentFlagState(command, agent, name string) (found, enabled bool) {
	return commandEnvironmentFlagStateAtDepth(command, agent, name, "", 0)
}

func commandEnvironmentFlagStateForTrustedAgentServer(command, agent, name, trustedWrapper string) (found, enabled bool) {
	return commandEnvironmentFlagStateAtDepth(command, agent, name, trustedWrapper, 0)
}

func commandEnvironmentFlagStateAtDepth(command, agent, name, trustedWrapper string, depth int) (found, enabled bool) {
	if depth > maxNestedProgramDepth || strings.TrimSpace(command) == "" || agent == "" {
		return false, false
	}
	call, ok := singleSimpleCall(command)
	if !ok {
		return false, false
	}

	if nested, ok := trustedAgentServerProgram(call, trustedWrapper); ok {
		return commandEnvironmentFlagStateAtDepth(nested, agent, name, trustedWrapper, depth+1)
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

// trustedAgentServerProgram is the single authorization point for nested
// agent-server argv. Syntax and identity are inseparable here so a new caller
// cannot parse the nested program and forget to prove who will execute it.
func trustedAgentServerProgram(call *syntax.CallExpr, trustedWrapper string) (string, bool) {
	program, ok := literalAgentServerProgram(call)
	if !ok {
		return "", false
	}
	words, _ := stripExecPrefix(call.Args)
	if len(words) == 0 || !isTrustedAfBinary(words[0], trustedWrapper) {
		return "", false
	}
	return program, true
}

// literalAgentServerProgram parses only the argument order af itself emits. It
// deliberately establishes no executable identity: an arbitrary repository
// binary can reproduce every word here. Only trustedAgentServerProgram may
// expose its result to a credential decision.
func literalAgentServerProgram(call *syntax.CallExpr) (string, bool) {
	if !callIsLiteral(call) || len(call.Assigns) != 0 {
		return "", false
	}
	words, _ := stripExecPrefix(call.Args)
	args, _ := literalCommandArgs(words)
	if len(args) < 8 || args[1] != "agent-server" {
		return "", false
	}
	if args[2] != "--listen" || args[3] == "" || args[4] != "--repo" || args[5] == "" ||
		args[6] != "--title" || args[7] == "" {
		return "", false
	}
	program := defaultAgentServerProgram
	idx := 8
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

func literalShellWord(word *syntax.Word) (string, bool) {
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
