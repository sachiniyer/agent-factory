package config

import (
	"fmt"
	"sort"
	"strings"
)

// sandbox_agent_config (#5571 W7.7): the operator's own choice of agent
// personalization to place in a sandbox session's account scope.
//
// An entry is "<agent>:<file>". The set of files is CLOSED — it is the
// personalization surface, never a credential — so a value cannot name an
// arbitrary path off the daemon box, and "what leaves the machine" is decided
// by this list and by the operator's own config alone. It is global-only for the
// reason sandbox.ssh is: a repository must never choose what af copies, or to
// where. Placement additionally requires a LISTED host and a session account
// scope (session/sandbox_agent_config.go); this file only validates the key.
var sandboxAgentConfigFiles = map[string][]string{
	// settings.json, the user CLAUDE.md, and .claude.json — the MCP server config.
	"claude": {"settings.json", "CLAUDE.md", ".claude.json"},
	// config.toml and the user AGENTS.md.
	"codex": {"config.toml", "AGENTS.md"},
}

// SandboxAgentConfigAllowed lists the permitted entries, for error text and docs.
func SandboxAgentConfigAllowed() []string {
	var out []string
	for agent, files := range sandboxAgentConfigFiles {
		for _, f := range files {
			out = append(out, agent+":"+f)
		}
	}
	sort.Strings(out)
	return out
}

// NormalizeSandboxAgentConfig validates and de-duplicates the entries, keeping
// the operator's order. An entry outside the closed set is an error naming the
// permitted ones.
func NormalizeSandboxAgentConfig(entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		agent, file, ok := strings.Cut(entry, ":")
		if !ok || !allowedSandboxAgentConfig(agent, file) {
			return nil, fmt.Errorf("%q is not a permitted sandbox agent config entry; use one of %s",
				raw, strings.Join(SandboxAgentConfigAllowed(), ", "))
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	return out, nil
}

func allowedSandboxAgentConfig(agent, file string) bool {
	for _, f := range sandboxAgentConfigFiles[agent] {
		if f == file {
			return true
		}
	}
	return false
}
