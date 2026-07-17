package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// afUsageReference is the single source of truth for teaching an agent the af
// CLI. It is the body of the consolidated "af" skill delivered to every agent —
// the Claude Code plugin skill (plugin.go), the amp/codex/gemini SKILL.md
// (agentskill.go), and the aider --read context file — so no surface can drift
// (#1043). Keep it complete but terse: every user-facing command group (sessions,
// tabs, tasks, daemon, maintenance), no boilerplate.
const afUsageReference = `You are running inside Agent Factory (af), a terminal multiplexer that runs each AI coding agent in an isolated git worktree. Manage sessions, tasks, and the daemon with the "af" CLI. Commands print JSON on stdout; run "af <command> --help" for full flag lists. To target another repository, pass --repo <path>: honored by every title-taking command — sessions get/preview/create/list/send-prompt/kill/attach/tab-create/tab-delete/archive/restore/watch and tasks list/add. Session titles are unique WITHIN a project, not across projects, so the same name may exist in several repos: a <title> resolves inside --repo when given, else the current directory's repo. With no repo context a title held by just one session still resolves, while one held by several reports an error naming those projects — pass --repo to pick one. Remote hook sessions are the one exception: their names are shared across projects because the hook scripts receive them verbatim. tasks get/update/trigger/remove take a globally unique id (no --repo needed).

Sessions (one agent per isolated worktree):
  af sessions whoami                                   Identify your own session
  af sessions list                                     List sessions
  af sessions get <title>                              Fetch one session
  af sessions create --name <title> [--prompt <p>] [--program claude|codex|aider|gemini|amp|opencode]
  af sessions send-prompt <title> <prompt> [--create]  Send a prompt (--create makes the session first if missing)
  af sessions preview <title>                          Snapshot another session's terminal output
  af sessions watch <title>                            Block until the session goes idle (agent done, ready for review); exits 0 when ready, non-zero on lost/dead/archived or --timeout (default 30m)
  af sessions attach <title>                           Attach interactively (foreground)
  af sessions kill <title>                             Kill a session and clean up its worktree
  af sessions archive <title>                          Archive (tmux down, worktree moved out; restartable)
  af sessions archive --self                            Archive your OWN session (resolved via whoami); no title needed
  af sessions restore <title>                          Restore an archived, lost, or dead session

Tabs (extra processes in a session's worktree; max 9 per session; not available for remote sessions):
  af sessions tab-create <title> --command <cmd> [--name <tab>]   Prints the resolved tab name; tabs persist across restarts
  af sessions tab-create <title> --kind web --port <n>|--url <u>  A browser pane (no PTY): shows a dev server/site to the user in the web UI
  af sessions tab-create <title> --kind vscode                    A VS Code editor pane on this session's worktree (no --url/--port); needs code-server installed
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

Finishing up: when the user confirms your work is complete and asks you to wrap up, self-archive with "af sessions archive --self". It is non-destructive — the worktree is moved out, nothing is deleted, and the session is restorable later with "af sessions restore <title>". But it tears down THIS session's tmux, so it kills you the instant you run it: nothing you say or do after it is ever seen. Treat it as the ABSOLUTE LAST action — first finish every summary, result, and confirmation the user needs, and only once you have nothing left to report run "af sessions archive --self" as the very last step.

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
// Every agent gets the SAME afUsageReference; only the delivery seam differs.
// Prefer a file seam (a skill/context file the agent auto-discovers, launch
// command UNCHANGED) over a flag whenever the agent has a native skills folder,
// because an unknown flag kills the spawn as an opaque readiness timeout (the
// #1116/#1131 class) and the file seam keeps the launch byte-identical.
//
// Strategy per tool:
//   - Claude Code: --plugin-dir flag (a single "af" skill carrying afUsageReference).
//   - Codex: file seam — the af skill written to codex's skills folder
//     ($CODEX_HOME/skills, 0.144.1+). This RETIRES #1043 and the old
//     -c developer_instructions= blob: codex now auto-discovers user skills, so
//     the launch command is returned UNCHANGED and the big prompt is gone.
//   - Gemini: file seam — the af skill written to gemini's user skills folder
//     (~/.gemini/skills, 0.42.0+), auto-discovered and enabled at session start.
//   - Amp: file seam — the af skill written to amp's home skills dir (#1582).
//   - opencode: ENV seam — OPENCODE_CONFIG points at an af-OWNED config (under af's
//     own config dir) whose `instructions` key adds an af-owned markdown file.
//     opencode MERGES that config with the user's own rather than replacing it, so
//     their settings survive. af writes NOTHING into ~/.config/opencode: the
//     guidance exists only while af launches the process, and running `opencode` by
//     hand later sees no trace of af — see ensureOpencodeAfConfig.
//   - Aider: --read flag pointing at an af-owned context file. Aider has NO
//     auto-discovered global skills folder, so it takes a flag (like claude);
//     --read is a known, repeatable, additive aider flag.
//   - Commands running no known agent: no injection.
//
// Accepted tradeoff (#1585 review, finding 2): DetectAgentFromCommand is a shared
// basename heuristic, so a program_overrides entry pointing a NON-<agent> binary
// named "codex"/"gemini"/"amp"/"aider" reaches the matching branch. For the file
// seams this is benign (the launch command is unchanged; the worst case is a
// dormant af-owned skill dir). For the flag seams (claude, aider) it carries the
// same pre-existing #1116/#1131 exposure claude already has; we do NOT re-derive
// agent-ness with a second heuristic — the #1132 rule is one detection choke-point.
func injectSystemPrompt(resolved string) string {
	switch tmux.DetectAgentFromCommand(resolved) {
	case tmux.ProgramClaude:
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
	case tmux.ProgramCodex:
		if _, err := ensureCodexSkillDir(); err != nil {
			log.WarningLog.Printf("failed to set up codex af skill, af guidance unavailable to codex: %v", err)
		}
		return resolved
	case tmux.ProgramGemini:
		if _, err := ensureGeminiSkillDir(); err != nil {
			log.WarningLog.Printf("failed to set up gemini af skill, af guidance unavailable to gemini: %v", err)
		}
		return resolved
	case tmux.ProgramAmp:
		if _, err := ensureAmpSkillDir(); err != nil {
			log.WarningLog.Printf("failed to set up amp af skill, af guidance unavailable to amp: %v", err)
		}
		return resolved
	case tmux.ProgramOpencode:
		// opencode has no instructions FLAG, so this is an env seam: af points
		// OPENCODE_CONFIG at an af-OWNED config that adds af's instructions file.
		// opencode MERGES it with the user's own config (verified), so their
		// settings survive, and af writes nothing into ~/.config/opencode.
		if opencodeCarriesConfigEnv(resolved) {
			log.WarningLog.Printf("opencode command already sets OPENCODE_CONFIG; leaving it alone (af guidance not injected for opencode)")
			return resolved
		}
		path, err := ensureOpencodeAfConfig()
		if err != nil {
			log.WarningLog.Printf("failed to set up opencode af instructions, af guidance unavailable to opencode: %v", err)
			return resolved
		}
		if path == "" {
			return resolved
		}
		// Prefix, not append: an env assignment must precede the command. tmux
		// runs the program string through a shell, and both DetectAgentFromCommand
		// and preflight's firstExecutable already skip leading VAR=VALUE words
		// (the `env FOO=1 <agent>` wrapper case, #742), so the agent is still
		// detected at the right token.
		return "OPENCODE_CONFIG=" + shellQuote(path) + " " + resolved
	case tmux.ProgramAider:
		// Aider takes a flag, not a file drop: it has no auto-discovered global
		// skills folder. A write failure (or a non-af file at our path) yields an
		// empty path, in which case we must NOT append --read pointing at a file
		// we do not own — return the launch command unchanged.
		path, err := ensureAiderReadFile()
		if err != nil {
			log.WarningLog.Printf("failed to set up aider af skill, af guidance unavailable to aider: %v", err)
			return resolved
		}
		if path == "" {
			return resolved
		}
		return resolved + " --read " + shellQuote(path)
	}
	return resolved
}
