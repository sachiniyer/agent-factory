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
  af sessions preview <title>               View another session's terminal output
  af sessions tab-create <title> --command <cmd>  Spawn a process tab in the worktree
  af sessions tab-delete <title> --name <tab>     Delete a single tab`

// shellQuote wraps a string in single quotes, escaping any embedded single quotes
// using the standard shell idiom: replace ' with '\"
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// injectSystemPrompt injects Agent Factory instructions into the session.
//
// resolved is the actual command string to be passed to tmux — the agent
// name, the configured program_overrides entry, or a legacy free-form
// persisted Program value ("/home/foo/bin/claude --plugin-dir x", #677).
// Which flags to inject, if any, is decided by the agent detected IN that
// resolved command (tmux.DetectAgentFromCommand), never by the config-name
// enum the instance was created with: an override may point "claude" at a
// different agent or at a non-agent binary, and injecting claude flags there
// makes the program exit on the unknown option, so the spawn dies as an
// opaque timeout (#1116, #1131).
//
// Strategy per tool:
//   - Claude Code: --plugin-dir flag only (slash commands + /af-whoami for self-identification)
//   - Codex: -c developer_instructions="..." flag (text-based, no plugin support)
//   - aider, gemini, and commands running no known agent: no injection.
func injectSystemPrompt(resolved string) string {
	agent := tmux.DetectAgentFromCommand(resolved)
	if agent == tmux.ProgramClaude {
		pluginDir, err := ensurePluginDir()
		if err != nil {
			log.WarningLog.Printf("failed to set up plugin directory, slash commands unavailable: %v", err)
			return resolved
		}
		// Unconditional append is safe here, unlike the AutoYes (#818) and
		// codex (#820) flags: no binary has ever persisted --plugin-dir into
		// Instance.Program (the injected form only reaches tmux SetProgram),
		// and claude's --plugin-dir is repeatable, so one in a user's
		// program_overrides is additive rather than a conflict.
		return resolved + " --plugin-dir " + shellQuote(pluginDir)
	}
	if agent == tmux.ProgramCodex {
		// Skip when the resolved command already carries a
		// developer_instructions override — e.g. a deliberate
		// program_overrides entry (#820). codex's -c is last-wins for the
		// same key, so appending ours would silently clobber the user's.
		if strings.Contains(resolved, "developer_instructions=") {
			return resolved
		}
		return resolved + " -c " + shellQuote("developer_instructions="+codexSystemPrompt)
	}
	return resolved
}
