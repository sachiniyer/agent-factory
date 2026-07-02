package session

import (
	"path/filepath"
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

// DetectAgentFromProgram maps a possibly-legacy Program value to its canonical
// agent enum name (one of tmux.SupportedPrograms). Sessions persisted by
// pre-#659 binaries may hold free-form Program strings — an absolute path
// and/or trailing flags, e.g. "/home/foo/bin/claude --plugin-dir x" — rather
// than the bare enum that current binaries write. On restore those legacy
// values flow into the flag-injection layer, where strict enum equality
// (added in #659) no longer recognizes them, silently dropping --plugin-dir,
// bypassPermissions, and Codex developer_instructions (#677).
//
// The match is deliberately DEFENSIVE, not permissive: it returns a canonical
// name only when filepath.Base of the first whitespace-split token equals a
// tmux.SupportedPrograms entry verbatim. For anything else — unknown tools,
// empty input, the bare enum (which maps to itself) — it returns the input
// unchanged, so we never inject Claude flags into a non-Claude session. This
// is scoped purely to flag injection on restore; it is NOT a general-purpose
// command parser and must not be reused as one.
func DetectAgentFromProgram(program string) string {
	fields := strings.Fields(program)
	if len(fields) == 0 {
		return program
	}
	base := filepath.Base(fields[0])
	for _, supported := range tmux.SupportedPrograms {
		if base == supported {
			return supported
		}
	}
	return program
}

// injectSystemPrompt injects Agent Factory instructions into the session.
//
// agent is the Instance.Program value (normally the canonical enum, but
// possibly a legacy free-form string on restored sessions) and resolved is
// the actual command string to be passed to tmux (the agent name or the
// configured program_overrides entry). agent is normalized via
// DetectAgentFromProgram so legacy paths still match; flags are appended to
// resolved.
//
// Strategy per tool:
//   - Claude Code: --plugin-dir flag only (slash commands + /af-whoami for self-identification)
//   - Codex: -c developer_instructions="..." flag (text-based, no plugin support)
func injectSystemPrompt(agent, resolved, sessionTitle, worktreePath string) string {
	agent = DetectAgentFromProgram(agent)
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
