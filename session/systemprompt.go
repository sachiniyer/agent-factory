package session

import (
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
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

// getBaseCommand extracts the lowercase basename of the executable from a
// program string that may include a full path and arguments.
// For example, "/home/user/bin/claude --model opus" returns "claude".
// It handles single-quoted and double-quoted paths for binaries whose
// paths contain spaces (e.g. on WSL).
func getBaseCommand(program string) string {
	program = strings.TrimSpace(program)
	if len(program) == 0 {
		return ""
	}

	var cmd string
	// Handle single-quoted paths. Embedded single quotes are escaped using the
	// POSIX idiom '\'' (close quote, escaped literal quote, reopen quote), so
	// scan for a closing quote that is NOT part of a '\'' escape sequence.
	if program[0] == '\'' {
		end := -1
		for i := 1; i < len(program); i++ {
			if program[i] != '\'' {
				continue
			}
			// Check for the '\'' escape idiom: current ' followed by \'' —
			// i.e. program[i..i+3] == "'\\''". Advance past the whole 4-byte
			// sequence so the reopening quote is not mistaken for the closer.
			if i+3 < len(program) && program[i+1] == '\\' && program[i+2] == '\'' && program[i+3] == '\'' {
				i += 3
				continue
			}
			end = i
			break
		}
		if end >= 0 {
			cmd = program[1:end]
		} else {
			cmd = program[1:]
		}
		// Unescape '\'' sequences back to a literal ' so filepath.Base works.
		cmd = strings.ReplaceAll(cmd, "'\\''", "'")
	} else if program[0] == '"' {
		end := strings.Index(program[1:], "\"")
		if end >= 0 {
			cmd = program[1 : end+1]
		} else {
			cmd = program[1:]
		}
	} else {
		parts := strings.Fields(program)
		if len(parts) == 0 {
			return ""
		}
		cmd = parts[0]
	}
	return strings.ToLower(filepath.Base(cmd))
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
			log.WarningLog.Printf("failed to set up plugin directory, slash commands unavailable: %v", err)
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
