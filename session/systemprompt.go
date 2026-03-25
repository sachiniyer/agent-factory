package session

import (
	"fmt"
	"strings"
)

// codexSystemPromptTemplate is the system prompt for Codex sessions, which don't support plugins.
const codexSystemPromptTemplate = `You are running inside Agent Factory (af), a terminal multiplexer for AI coding agents.

Your session name: %s

You can manage sessions and tasks using the "af" CLI:

Session commands:
  af api sessions list                          List all sessions
  af api sessions kill <title>                  Delete/kill a session
  af api sessions send-prompt <title> <prompt>  Send a prompt to another session
  af api sessions preview <title>               View another session's terminal output`

// buildCodexSystemPrompt returns the full system prompt text for a Codex session.
func buildCodexSystemPrompt(sessionTitle string) string {
	return fmt.Sprintf(codexSystemPromptTemplate, sessionTitle)
}

// shellQuote wraps a string in single quotes, escaping any embedded single quotes
// using the standard shell idiom: replace ' with '\"
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// injectSystemPrompt injects Agent Factory instructions into the session.
//
// Strategy per tool:
//   - Claude Code: --plugin-dir flag for slash commands + minimal --append-system-prompt
//   - Codex: -c developer_instructions="..." flag (text-based, no plugin support)
//
// Returns the (possibly modified) program string.
func injectSystemPrompt(program, sessionTitle, worktreePath string) string {
	lower := strings.ToLower(program)

	// Claude Code: --plugin-dir for commands + --append-system-prompt for session context
	if strings.Contains(lower, "claude") {
		pluginDir, err := ensurePluginDir()
		if err != nil {
			// Fall back to append-system-prompt only if plugin dir fails
			prompt := fmt.Sprintf("You are running inside Agent Factory (af). Your session name: %s", sessionTitle)
			return program + " --append-system-prompt " + shellQuote(prompt)
		}
		prompt := fmt.Sprintf("You are running inside Agent Factory (af), a terminal multiplexer for AI coding agents.\n\nYour session name: %s", sessionTitle)
		return program + " --plugin-dir " + shellQuote(pluginDir) + " --append-system-prompt " + shellQuote(prompt)
	}

	// Codex: -c developer_instructions="..." config override
	if strings.Contains(lower, "codex") {
		prompt := buildCodexSystemPrompt(sessionTitle)
		return program + " -c " + shellQuote("developer_instructions="+prompt)
	}

	return program
}
