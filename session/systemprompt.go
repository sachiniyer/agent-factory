package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// afUsageReference is the single source of truth for teaching an agent the af
// CLI. It is the body of the consolidated "af" plugin skill for Claude Code
// (see plugin.go) and the developer_instructions text for Codex, so the two
// surfaces cannot drift (#1043). Keep it complete but terse: every user-facing
// command group (sessions, tabs, tasks, daemon, maintenance), no boilerplate.
const afUsageReference = `You are running inside Agent Factory (af), a terminal multiplexer that runs each AI coding agent in an isolated git worktree. Manage sessions, tasks, and the daemon with the "af" CLI. Commands print JSON on stdout; run "af <command> --help" for full flag lists. To target another repository, pass --repo <path>: honored by sessions create/list/send-prompt/kill/attach/tab-create/tab-delete and tasks list/add. Two commands accept --repo but SILENTLY IGNORE it — "sessions get" and "sessions preview" always resolve the title across ALL repos, so with the same title in two repos you may get the wrong one regardless of --repo; disambiguate by using unique titles. tasks get/update/trigger/remove take a globally unique id (no --repo needed).

Sessions (one agent per isolated worktree):
  af sessions whoami                                   Identify your own session
  af sessions list                                     List sessions
  af sessions get <title>                              Fetch one session
  af sessions create --name <title> [--prompt <p>] [--program claude|codex|aider|gemini]
  af sessions send-prompt <title> <prompt> [--create]  Send a prompt (--create makes the session first if missing)
  af sessions preview <title>                          Snapshot another session's terminal output
  af sessions attach <title>                           Attach interactively (foreground)
  af sessions kill <title>                             Kill a session and clean up its worktree

Tabs (extra processes in a session's worktree; max 9 per session; not available for remote sessions):
  af sessions tab-create <title> --command <cmd> [--name <tab>]   Prints the resolved tab name; tabs persist across restarts
  af sessions tab-delete <title> --name <tab>                     The agent tab itself can't be deleted; kill the session instead

Tasks (deliver a prompt on a cron schedule, or whenever a long-running watch script prints a stdout line; exactly one of --cron/--watch-cmd per task):
  af tasks list                                        List tasks
  af tasks get <id>                                    Fetch one task
  af tasks add --name <n> --prompt <p> --cron "0 9 * * *" [--target-session <title>] [--program <agent>]
  af tasks add --name <n> --watch-cmd <cmd> [--prompt "... {{line}} ..."] [--target-session <title>] [--program <agent>]
  af tasks update <id> [--cron ...|--watch-cmd ...] [--prompt ...] [--target-session ...] [--program ...] [--enabled true|false]
  af tasks trigger <id>                                Run a cron task immediately
  af tasks remove <id>
Without --target-session each run creates a fresh session; {{line}} in a watch prompt is replaced by the emitted stdout line. On update, setting one trigger clears the other, and --target-session "" reverts to session-per-run. The background daemon runs all schedules; "af daemon install" / "af daemon uninstall" manage its login autostart.

Creating or prompting a session: the prompt is the entire contract, because the receiving agent inherits no context from your conversation. State everything it needs, including the expected output shape, e.g. "Open a PR titled X, link it back, do not merge" or "Write a report to <file> and stop; no code changes".

Maintenance: af version, af debug (print resolved config), af upgrade (self-upgrade). Never run "af reset": it kills every session and deletes ALL linked worktrees and their branches across repos.`

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
//   - Claude Code: --plugin-dir flag only (a single "af" skill carrying afUsageReference)
//   - Codex: -c developer_instructions="..." flag carrying the same afUsageReference
//     (no custom-skills-folder mechanism exists in the Codex CLI; see #1043)
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
		return resolved + " -c " + shellQuote("developer_instructions="+afUsageReference)
	}
	return resolved
}
