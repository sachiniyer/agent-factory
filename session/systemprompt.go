package session

import (
	"fmt"
	"strings"
)

// systemPromptTemplate is the template for the system prompt injected into AI sessions.
// It tells the AI about its Claude Squad context and available CLI commands.
const systemPromptTemplate = `You are running inside Claude Squad (cs), a terminal multiplexer for AI coding agents.

Your session name: %s

You can manage sessions and tasks using the "cs" CLI:

Session commands:
  cs api sessions list                          List all sessions
  cs api sessions kill <title>                  Delete/kill a session
  cs api sessions send-prompt <title> <prompt>  Send a prompt to another session
  cs api sessions preview <title>               View another session's terminal output
  cs api sessions diff <title>                  Get diff stats for a session

Task commands (kanban board):
  cs api tasks board                               Get kanban board (columns + tasks)
  cs api tasks list                                List all tasks (flat)
  cs api tasks add --title "description"           Add task to backlog
  cs api tasks add --title "desc" --status review  Add task to specific column
  cs api tasks move <id> --status in_progress      Move task between columns
  cs api tasks toggle <id>                         Mark a task as done/not done
  cs api tasks remove <id>                         Remove a task
Available columns: backlog, in_progress, review, done`

// buildSystemPrompt returns the system prompt text for a session.
func buildSystemPrompt(sessionTitle string) string {
	return fmt.Sprintf(systemPromptTemplate, sessionTitle)
}

// shellQuote wraps a string in single quotes, escaping any embedded single quotes
// using the standard shell idiom: replace ' with '\''
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// injectSystemPrompt injects Claude Squad instructions into the session.
//
// Strategy per tool:
//   - Claude Code: --append-system-prompt flag (appended to program command)
//   - Codex: -c developer_instructions="..." flag (appended to program command)
//
// TODO: Add support for Amp (reads AGENT.md), OpenCode (reads AGENTS.md), and other tools.
// These tools don't have CLI flags for system prompt injection — they only read project-level
// instruction files. We could write those files into the worktree, but that approach is deferred.
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
