package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// codexSystemPrompt is the system prompt for Codex sessions, which don't support plugins.
const codexSystemPrompt = `You are running inside Agent Factory (af), a terminal multiplexer for AI coding agents.

You can manage sessions using the "af" CLI:

Session commands:
  af sessions create --name <title> [--prompt <prompt>]  Create a new session
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

// injectSystemPrompt injects Agent Factory instructions into the session.
//
// agent is the canonical enum name (e.g. tmux.ProgramClaude) and resolved is
// the actual command string to be passed to tmux (the agent name or the
// configured program_overrides entry). System-prompt flags are appended to
// resolved.
//
// Strategy per tool:
//   - Claude Code: --plugin-dir flag only (slash commands + /af-whoami for self-identification)
//   - Codex: -c developer_instructions="..." flag (text-based, no plugin support)
func injectSystemPrompt(agent, resolved, sessionTitle, worktreePath string) string {
	if agent == tmux.ProgramClaude {
		pluginDir, err := ensurePluginDir()
		if err != nil {
			log.WarningLog.Printf("failed to set up plugin directory, slash commands unavailable: %v", err)
			return resolved
		}
		return resolved + " --plugin-dir " + shellQuote(pluginDir)
	}
	if agent == tmux.ProgramCodex {
		return resolved + " -c " + shellQuote("developer_instructions="+codexSystemPrompt)
	}
	return resolved
}
