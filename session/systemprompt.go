package session

import (
	"fmt"
	"strings"
)

// systemPromptTemplate is the template for the system prompt injected into AI sessions.
// It tells the AI about its Agent Factory context and available CLI commands.
const systemPromptTemplate = `You are running inside Agent Factory (af), a terminal multiplexer for AI coding agents.

Your session name: %s

You can manage sessions and tasks using the "af" CLI:

Session commands:
  af api sessions list                          List all sessions
  af api sessions kill <title>                  Delete/kill a session
  af api sessions send-prompt <title> <prompt>  Send a prompt to another session
  af api sessions preview <title>               View another session's terminal output
  af api sessions diff <title>                  Get diff stats for a session

Task commands (kanban board):
  af api tasks board                                    Get kanban board (columns + tasks)
  af api tasks list                                     List all tasks (flat)
  af api tasks add --title "description"                Add task to backlog
  af api tasks add --title "desc" --status in_progress  Add task to specific column
  af api tasks move <id> --status in_progress           Move task between columns
  af api tasks link <id> --instance "my-session"        Link yourself to a task
  af api tasks unlink <id>                              Remove linkage
  af api tasks toggle <id>                              Mark a task as done/not done
  af api tasks remove <id>                              Remove a task
Available columns: backlog, in_progress, review, done

Self-assignment workflow:
  1. af api tasks board                              # See available tasks
  2. af api tasks move <id> --status in_progress     # Claim a task
  3. af api tasks link <id> --instance "YOUR_SESSION" # Link yourself
  4. ... do the work ...
  5. af api tasks move <id> --status done             # Mark complete`

// buildSystemPrompt returns the system prompt text for a session.
func buildSystemPrompt(sessionTitle string) string {
	return fmt.Sprintf(systemPromptTemplate, sessionTitle)
}

// shellQuote wraps a string in single quotes, escaping any embedded single quotes
// using the standard shell idiom: replace ' with '\''
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// injectSystemPrompt injects Agent Factory instructions into the session.
//
// Strategy per tool:
//   - Claude Code: --append-system-prompt flag (appended to program command)
//   - Codex: -c developer_instructions="..." flag (appended to program command)
//
// Returns the (possibly modified) program string.
func injectSystemPrompt(program, sessionTitle, worktreePath string) string {
	prompt := buildSystemPrompt(sessionTitle)
	lower := strings.ToLower(program)

	// Claude Code: --append-system-prompt flag
	if strings.Contains(lower, "claude") {
		return program + " --append-system-prompt " + shellQuote(prompt)
	}

	// Codex: -c developer_instructions="..." config override
	if strings.Contains(lower, "codex") {
		return program + " -c " + shellQuote("developer_instructions="+prompt)
	}

	return program
}
