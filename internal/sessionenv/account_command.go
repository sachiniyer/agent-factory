package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// commandOverridesName reports whether a resolved program could set, unset, or
// otherwise redirect a variable, and whether that answer is PROVABLE.
//
// It exists because the launch runs the program through `/bin/sh -c`, which
// applies command-local assignments AFTER the session environment is installed.
// So `CODEX_HOME=$HOME/.codex codex` silently wins over an injected account
// root — and program_overrides is reachable from a repository's checked-in
// config, which makes it a repo-controlled way to redirect whose quota a session
// spends (#2983).
//
// The second return is the honest-unknown channel. A command this cannot parse
// into a single simple call — a pipeline, a subshell, dynamic syntax — is not
// evidence of safety, and the caller fails closed on it. That is the same
// posture the surrounding package already takes for cloud-mode assignments.
func commandOverridesName(command, name string) (overrides, provable bool) {
	if command == "" {
		return false, true
	}
	call, ok := singleSimpleCall(command)
	if !ok {
		return false, false
	}
	return callOverridesName(call, name, 0)
}

func callOverridesName(call *syntax.CallExpr, name string, depth int) (overrides, provable bool) {
	if depth > maxNestedProgramDepth {
		return false, false
	}
	for _, assign := range call.Assigns {
		if assign != nil && assign.Name != nil && assign.Name.Value == name {
			return true, true
		}
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
	// assignment — `exec CODEX_HOME=/other codex` puts it in Args, not Assigns.
	for _, word := range words {
		assigned, ok := shellWordAssignmentName(word)
		if !ok {
			break
		}
		if assigned == name {
			return true, true
		}
	}
	// `env NAME=value agent`, and its unset/clear forms. env's own arguments are
	// assignments the shell does not expose through call.Assigns.
	if wordBaseEquals(words[0], "env") {
		for _, word := range words[1:] {
			lit, ok := literalShellWord(word)
			if !ok {
				// A non-literal argument to env could expand to anything.
				return false, false
			}
			switch {
			case lit == "-i" || lit == "--ignore-environment":
				// Clears the environment wholesale, which removes the injected root.
				return true, true
			case lit == "-u" || lit == "--unset":
				return true, true
			case strings.HasPrefix(lit, name+"="):
				return true, true
			}
		}
	}
	// A nested agent-server program carries its own command.
	if nested, ok := literalAgentServerProgram(call); ok {
		inner, ok := singleSimpleCall(nested)
		if !ok {
			return false, false
		}
		return callOverridesName(inner, name, depth+1)
	}
	return false, true
}
