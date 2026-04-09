package session

import (
	"path/filepath"
	"strings"
)

// codexSystemPrompt is the system prompt for Codex sessions, which don't support plugins.
const codexSystemPrompt = `You are running inside Agent Factory (af), a terminal multiplexer for AI coding agents.

You can manage sessions using the "af" CLI:

Session commands:
  af sessions whoami                        Identify your current session
  af sessions list                          List all sessions
  af sessions kill <title>                  Delete/kill a session
  af sessions send-prompt <title> <prompt>  Send a prompt to another session
  af sessions preview <title>               View another session's terminal output`

// shellQuote wraps a string in single quotes, escaping any embedded single quotes
// using the standard shell idiom: replace ' with '\"
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// getBaseCommand extracts the lowercase basename of the executable from a
// program string that may include a full path and arguments.
// For example, "/home/user/bin/claude --model opus" returns "claude".
func getBaseCommand(program string) string {
	parts := strings.Fields(program)
	if len(parts) == 0 {
		return ""
	}
	return strings.ToLower(filepath.Base(parts[0]))
}

// injectSystemPrompt injects Agent Factory instructions into the session.
//
// Strategy per tool:
//   - Claude Code: --plugin-dir flag only (slash commands + /af-whoami for self-identification)
//   - Codex: -c developer_instructions="..." flag (text-based, no plugin support)
//
// Returns the (possibly modified) program string.
func injectSystemPrompt(program, sessionTitle, worktreePath string) string {
	base := getBaseCommand(program)

	// Claude Code: --plugin-dir provides slash commands including /af-whoami
	if base == "claude" || base == "claude-code" {
		pluginDir, err := ensurePluginDir()
		if err != nil {
			return program
		}
		return program + " --plugin-dir " + shellQuote(pluginDir)
	}

	// Codex: -c developer_instructions="..." config override
	if base == "codex" {
		return program + " -c " + shellQuote("developer_instructions="+codexSystemPrompt)
	}

	return program
}
