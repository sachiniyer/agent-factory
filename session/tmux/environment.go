package tmux

import "strings"

// CommandEnvOverride describes how a shell command changes one environment
// variable before the detected agent executable. Present=false means the child
// inherits the daemon value. Present=true with Set=false means wrappers such as
// `env -u NAME` or `env -i` remove it. Literal=false means shell expansion makes
// the effective value unknowable without executing the command.
type CommandEnvOverride struct {
	Value   string
	Present bool
	Set     bool
	Literal bool
}

// EnvironmentOverrideFromCommand finds the last command-local change to name
// before the agent token. It covers leading shell assignment words and the env
// forms that change inheritance. This deliberately shares splitShellTokens and
// findAgentToken with agent detection: receipt routing must interpret the exact
// command through the same lexical model as launch routing (#2228 review).
func EnvironmentOverrideFromCommand(command, name string) CommandEnvOverride {
	tokens, _ := splitShellTokens(command)
	agentIdx, agent := findAgentToken(tokens)
	if agent == "" {
		return CommandEnvOverride{}
	}
	var result CommandEnvOverride
	inEnv := false
	for idx := 0; idx < agentIdx; idx++ {
		tok := tokens[idx]
		if strings.EqualFold(baseCommand(tok), "env") {
			inEnv = true
			continue
		}
		if key, value, ok := strings.Cut(tok, "="); ok && key == name {
			result = CommandEnvOverride{
				Value: value, Present: true, Set: true, Literal: literalEnvironmentValue(value),
			}
			continue
		}
		if !inEnv {
			continue
		}
		switch {
		case tok == "-i" || tok == "--ignore-environment" || tok == "-":
			result = CommandEnvOverride{Present: true, Literal: true}
		case tok == "-u" || tok == "--unset":
			if idx+1 < agentIdx {
				idx++
				if tokens[idx] == name {
					result = CommandEnvOverride{Present: true, Literal: true}
				}
			}
		case strings.HasPrefix(tok, "--unset="):
			if strings.TrimPrefix(tok, "--unset=") == name {
				result = CommandEnvOverride{Present: true, Literal: true}
			}
		}
	}
	return result
}

func baseCommand(token string) string {
	if slash := strings.LastIndexAny(token, `/\\`); slash >= 0 {
		return token[slash+1:]
	}
	return token
}

// literalEnvironmentValue is conservative because splitShellTokens removes
// quote provenance. Values requiring expansion cannot be reproduced safely in
// the daemon process; callers must fail loudly instead of inspecting the wrong
// receiver store. Spaces are fine (they necessarily came from quoting), while
// expansion/control metacharacters are not.
func literalEnvironmentValue(value string) bool {
	return !strings.ContainsAny(value, "$`;&|<>\n\r") && !strings.HasPrefix(value, "~")
}
