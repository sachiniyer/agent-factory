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
  af sessions preview <title>               View another session's terminal output`

// shellQuote wraps a string in single quotes, escaping any embedded single quotes
// using the standard shell idiom: replace ' with '\"
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// splitShell tokenizes a shell-style command string, respecting single
// quotes (no escapes), double quotes (with \" and \\ escapes), and
// backslash escapes outside quotes. Adjacent runs concatenate into a
// single token (e.g. 'foo'bar -> "foobar"), which makes the POSIX
// '\” idiom for embedded apostrophes work naturally. Unclosed quotes
// consume to end of input.
func splitShell(s string) []string {
	var tokens []string
	var cur strings.Builder
	inToken := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case ' ', '\t':
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
			i++
		case '\\':
			inToken = true
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i += 2
			} else {
				i++
			}
		case '\'':
			inToken = true
			i++
			for i < len(s) && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++
			}
		case '"':
			inToken = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					n := s[i+1]
					if n == '"' || n == '\\' {
						cur.WriteByte(n)
						i += 2
						continue
					}
				}
				cur.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				i++
			}
		default:
			inToken = true
			cur.WriteByte(c)
			i++
		}
	}
	if inToken {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// getBaseCommand extracts the lowercase basename of the executable from a
// program string that may include a full path and arguments.
// For example, "/home/user/bin/claude --model opus" returns "claude".
// It respects shell quoting (single/double quotes and backslash escapes)
// and additionally tolerates unquoted absolute or relative paths whose
// directory components contain spaces — common when users pass --program
// with a path like "/home/my user/claude" via the CLI (issue #463).
//
// Authoritative principle: the basename of the RECONSTRUCTED command path
// is the source of truth. The left-to-right token scan over
// tmux.SupportedPrograms is a fallback ONLY for the case where
// reconstruction lands on a non-agent basename — specifically, paths whose
// directory components contain a literal " - " (issue #513) that breaks
// the naive "join non-flag tokens" reconstruction by splitting at the
// "-" token.
//
// Reorder rationale (issue #639): scanning all tokens left-to-right
// BEFORE reconstruction false-matches on intermediate directories named
// like a supported agent. For example "/home/user/claude backups/aider"
// splits into ["/home/user/claude", "backups/aider"]; a leading scan
// would match "claude" on the intermediate dir and return the wrong
// agent for the actual "aider" executable.
func getBaseCommand(program string) string {
	program = strings.TrimSpace(program)
	if len(program) == 0 {
		return ""
	}
	parts := splitShell(program)
	if len(parts) == 0 {
		return ""
	}
	cmd := parts[0]
	// If the first token looks like an unquoted path (absolute or relative),
	// greedily join the following non-flag tokens to recover the executable
	// basename for inputs like `/home/my user/claude --foo` where the path
	// contains spaces and was not quoted on the command line (#463).
	if strings.HasPrefix(cmd, "/") || strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") {
		end := 1
		for end < len(parts) && !strings.HasPrefix(parts[end], "-") {
			end++
		}
		cmd = strings.Join(parts[:end], " ")
	}
	reconstructed := strings.ToLower(filepath.Base(cmd))
	for _, supported := range tmux.SupportedPrograms {
		if reconstructed == supported {
			return supported
		}
	}
	// Fallback (#513/#537): reconstruction landed on a non-agent basename,
	// which happens when an unquoted path contains a literal " - " segment
	// — splitShell emits the "-" as its own token and the reconstruction
	// loop stops there. Scan all tokens left-to-right for a supported
	// program basename; left-to-right wins so the leading executable
	// dominates if a flag value happens to point at another known agent.
	for _, p := range parts {
		base := strings.ToLower(filepath.Base(p))
		for _, supported := range tmux.SupportedPrograms {
			if base == supported {
				return supported
			}
		}
	}
	return reconstructed
}

// BaseCommand extracts the lowercase executable basename from a program string.
func BaseCommand(program string) string {
	return getBaseCommand(program)
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
