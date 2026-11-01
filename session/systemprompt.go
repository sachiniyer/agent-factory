package session

import (
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// afUsageReference is the single source of truth for teaching an agent the af
// CLI. It is the body of the consolidated "af" skill delivered to every agent —
// the Claude Code plugin skill (plugin.go), the amp/codex/gemini/devin SKILL.md
// (agentskill.go), and the aider --read context file — so no surface can drift
// (#1043). Keep it complete but terse: every user-facing command group (sessions,
// tabs, tasks, daemon, maintenance), no boilerplate.
//
// It is assembled from segments rather than written as one literal because the
// text is delivered to TWO audiences whose framing differs (#2172):
//
//   - an agent af itself launched, which IS a session (afUsageIntroInside,
//     afUsageDispatched, afUsageOutroInside), and
//   - an agent that installed the af plugin/skill on its own and is NOT running
//     inside af (afUsageIntroPlugin, afUsageOutroPlugin) — see
//     AfPluginUsageReference.
//
// Everything that is true for both — every command group, every scoping rule —
// lives in the shared afUsageBody, so editing af's usage guidance changes both
// audiences at once and neither can drift. The dispatched-parent escalation
// guidance (afUsageDispatched) is inside-only: an agent that installed the plugin
// is a CALLER of af, not a sub-instance a parent dispatched, so it appears in
// afUsageReference but never in AfPluginUsageReference. Only the framing is
// swapped otherwise; the
// generated plugin artifacts must never carry a verbatim copy of the "you are
// running inside af" opening, which is false for the audience that installed
// them.
const afUsageReference = afUsageIntroInside + "\n\n" + afUsageDispatched + "\n\n" + afUsageBody + "\n\n" + afUsageProposalsInside + "\n\n" + afUsageBacklogInside + "\n\n" + afUsageAttentionInside + "\n\n" + afUsageOutroInside + "\n\n" + afUsageOutro

// afUsageReferenceCallbackless replaces afUsageReference for an instance that
// has NO af callback — a sandbox session whose credential could not be minted
// or delivered (#5571 W3.1). Teaching af verbs there is worse than omitting
// them: every command fails on its own and the agent spends turns discovering
// it. The variant keeps the inside framing (the agent IS an af instance — that
// fact stays true) and says plainly that af is unreachable from here, so it
// does its work and reports instead of trying the CLI. The shared file seams
// (codex/gemini/amp/devin SKILL.md) keep the full doc until W3.3's
// capability-generated guidance; only the per-instance seams (claude
// --plugin-dir, aider --read) carry this text.
const afUsageReferenceCallbackless = `You are an AI coding agent running inside Agent Factory (af): a tool that runs many AI coding agents at once, each isolated in its own git worktree — you are one of them.

The af CLI is NOT reachable from this session: af could not deliver a callback credential to this sandbox, so af commands run here cannot reach the daemon and will fail. Do not run af commands — do not try to open tabs, dispatch work, or query the daemon. Do your work in this worktree and report plainly what you did and anything you could not do; the user reads your output directly.`

// AfPluginUsageReference is afUsageReference reframed for an agent that is NOT
// running inside af: the same body and the same command reference, but an
// opening that introduces af as a separate CLI the agent drives (and that this
// plugin does not install), and a closing that tells it how to leave the
// sessions it created behind. It is what the generated per-agent plugin
// artifacts carry (see `af gen-docs --plugin-root`).
const AfPluginUsageReference = afUsageIntroPlugin + " " + afUsageBody + "\n\n" + afUsageOutroPlugin + "\n\n" + afUsageOutro

// afUsageIntroInside frames the body for an agent af launched: it IS an instance.
// It leads with the model and the terms (instance/session/tab/pane) because an
// agent that does not know it is an af instance reaches for unrelated tools — the
// #2473 case, where an agent asked to show a URL used a separate global browser
// skill instead of an af web tab. "instance" leads; "session" is noted as the
// CLI's word for the same thing.
const afUsageIntroInside = `You are an AI coding agent running inside Agent Factory (af): a tool that runs many AI coding agents at once, each isolated in its own git worktree — you are one of them.

- instance (the CLI calls it a session — same thing): one isolated git worktree and the agent in it. This worktree is yours, that agent is you; af runs many others alongside it.
- tab: a process or view inside an instance. You run in its agent tab, and you can add terminal, web, and editor tabs next to it.
- pane: what the user is looking at — an instance's currently-shown tab, in the terminal UI or the web UI.

Drive af with the "af" CLI, and prefer its native mechanisms over unrelated or global tools. In particular, to put a web page, running dev server, or editor in front of the user, add a tab to your own session (see Tabs) — not a separate browser or editor tool, which opens somewhere the user cannot see.`

// afUsageDispatched is the inside-only guidance for an agent another instance
// dispatched: the parent watches the GitHub issue/PR, not this terminal, so a
// blocker must be surfaced there — never in a local interactive prompt/picker the
// parent cannot see, and never by guessing silently or idling. Several lanes got
// stuck invisibly for exactly this reason (#2473). Not in the shared body: the
// plugin audience installed af itself and is not a dispatched sub-instance.
const afUsageDispatched = `When another instance dispatched you (your task came from a parent agent or lane, not a person at this terminal): the parent watches the relevant GitHub issue or PR, not your screen. If you hit a question or decision you cannot resolve, raise it there — post it plainly on that issue or PR. Never fall back on a local interactive prompt, picker, or menu: it renders only in your own terminal, so the parent never sees it and you block invisibly. Never guess silently, and never sit idle waiting — an unraised blocker is a stuck lane no one can help.`

// afUsageIntroPlugin frames the body for an agent that is not an af session —
// the plugin/skill audience. It states the two facts that framing changes:
// af is a separate program this plugin does not install, and the agent is a
// caller of af rather than a thing af is running. The "check, then tell the
// user" instruction is deliberate: an agent must never fetch and execute a
// release binary on the user's behalf (#2172, #2174).
const afUsageIntroPlugin = `Agent Factory (af) is a terminal multiplexer that runs each AI coding agent in its own isolated git worktree and tmux session. Use the "af" CLI to hand a task to a background agent instead of doing it in this conversation, and to schedule recurring agent runs. You are NOT running inside af: af is a separate program that this plugin does not install, so run "af version" first, and if that fails tell the user to install it with "` + AfInstallCommand + `" and stop — never download or run an installer yourself. Session and task commands need af's background daemon, which starts on demand; "af daemon status" reports it.`

// AfInstallCommand is the one command that installs af, as published in the
// README and the getting-started guide. It is a constant because two generated
// surfaces quote it — the plugin framing above and the plugin's detect-only
// preflight hook (commands/plugins_gen.go) — and an install command that has
// drifted is worse than none.
const AfInstallCommand = `curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | sh`

// The tab section of the reference, in parts so guidance can be generated from
// the capability projection (#5571 W3.3). afUsageBody concatenates them back to
// the full text, so the plugin audience — a CALLER of af, not a session — and a
// session whose projection admits everything read byte-for-byte what they always
// did.
const (
	afTabsLead         = `Tabs (extra processes and views in your instance's worktree; no limit on how many). `
	afTabsRestrictions = `Shell and process tabs require a local session; web and VS Code tabs work on remote sessions too — an external HTTPS URL is framed directly, a loopback target (--port is loopback) shows whatever listens on that port inside the session's own workspace, and a VS Code tab opens the editor running on the session's host (code-server or openvscode-server must be installed there). `
	afTabsTail         = `The <title> is the target session — for your own, get it from "af sessions whoami". Tabs are how you put something in front of the user; use them instead of an external browser, editor, or global tool.`
	afTabLineShell     = `  af sessions tab-create <title> --kind shell                    Terminal tab running $SHELL in the worktree (the same Terminal the TUI/web opens)`
	afTabLineProcess   = `  af sessions tab-create <title> --command <cmd> [--name <tab>]   Process tab running <cmd> in the worktree; prints the tab name; persists across restarts`
	afTabLineWeb       = `  af sessions tab-create <title> --kind web --url <u>|--port <n>  Web tab — a live browser view (no process) shown to the user in the web UI; this is how you show a URL, site, or dev server (--port n = http://localhost:n)`
	afTabLineVSCode    = `  af sessions tab-create <title> --kind vscode                    VS Code editor tab on the worktree (no --url/--port); needs code-server installed`
	afTabLineDelete    = `  af sessions tab-delete <title> --name <tab>                     Delete a tab; the agent tab can't be deleted (delete the session instead)`
)

// afUsageBody is every part of the reference that is true for BOTH audiences.
const afUsageBody = `Commands print JSON on stdout; run "af <command> --help" for full flag lists. Every session and task command is PROJECT-SCOPED: it acts on the current directory's project unless --repo <path> names another one, so "af tasks list" shows this project's tasks and "af tasks add" binds the new task here. Acting on another project always takes an explicit --repo; "af tasks list --all" and "af sessions prune --all" are the opt-ins that span every project. Session titles are unique WITHIN a project, not across projects, so the same name may exist in several repos: a <title> resolves inside --repo when given, else the current directory's repo. Task ids are globally unique but still project-scoped — "af tasks get/show/update/trigger/remove/restart <id-or-name>" accepts a task's id OR its exact name: an exact id match always wins (a name that collides with another task's id still resolves to the id), and otherwise the name must belong to exactly one task in scope — a name several in-scope tasks share is refused with their ids, and a task owned by another project is refused naming the --repo that would reach it. A task whose project changed underneath a command is refused rather than acted on; nothing is changed, so re-run it. With no repo context (outside a git repository) a title or task id-or-name held by just one project still resolves, while one held by several reports an error naming the matches — sessions list the projects to pick with --repo, tasks the ids to pass instead. Remote hook sessions are the one exception: their names are shared across projects because the hook scripts receive them verbatim.

Sessions (one agent per isolated worktree):
  af sessions whoami                                   Identify your own session
  af sessions list [--live] [--status <state>] [--max-age <duration>] [--limit N]
  af sessions get <title>                              Fetch one session
  af sessions create --name <title> [--prompt <p>] [--program claude|codex|aider|gemini|amp|opencode|devin]
  af sessions send-prompt <title> <prompt> [--create]  Send a prompt (--create makes the session first if missing)
  af sessions preview <title>                          Snapshot another session's terminal output
  af sessions watch <title>                            Wait for agent idle (awaiting input, not work completion); exits 0 on idle, non-zero on lost/dead/archived or --timeout (default 30m)
  af sessions attach <title>                           Attach interactively (foreground)
  af sessions kill <title>                             Delete a session; work in af-owned workspaces can be lost
  af sessions archive <title>                          Archive (tmux down, worktree moved out; restartable)
  af sessions archive --self                            Archive your OWN session (resolved via whoami); no title needed
  af sessions attention --self [--reason <text>]        Tell the user you need them (a blinking dot on your row in the web UI); clears when they reply
  af sessions handoff <title> [--to <agent>] [--account <name>]  Continue under another agent, account, or both
  af sessions retry-limit <title>                      Retry a usage-limit resume or an inspected, unconfirmed handoff
  af sessions retry-limit <title> --delivered           Mark an inspected handoff mission delivered (no resend); only after confirming it landed
  af sessions restore <title>                          Restore an archived, lost, or dead session
  af sessions prune --older-than <dur> [--all]            Dry-run: list reclaimable archived-session worktrees (af writes nothing)

Never rebind an af worktree to another lane's branch with "git checkout -B", "git switch -C", or a direct ref/HEAD write. Those commands bypass Git's ordinary worktree collision refusal and can turn the other lane's unchanged index into a staged revert when the branch moves. Use "af sessions handoff --to <agent> -- <title>" to continue in the existing worktree, or create a session on a different branch.

` + afTabsLead + afTabsRestrictions + afTabsTail + "\n" + afTabLineShell + "\n" + afTabLineProcess + "\n" + afTabLineWeb + "\n" + afTabLineVSCode + "\n" + afTabLineDelete + `

Tasks (deliver a prompt on a cron schedule, or whenever a long-running watch script prints a stdout line; exactly one of --cron/--watch-cmd per task):
  af tasks list [--all]                                List this project's tasks (--all spans every project)
  af tasks get <id-or-name>                            Fetch one task
  af tasks add --name <n> --prompt <p> --cron "0 9 * * *" [--target-session <title>] [--program <agent>]
  af tasks add --name <n> --watch-cmd <cmd> [--prompt "... {{line}} ..."] [--target-session <title>] [--program <agent>]
  af tasks update <id-or-name> [--cron ...|--watch-cmd ...] [--prompt ...] [--target-session ...] [--project-path <repo>] [--program ...] [--enabled true|false]
  af tasks trigger <id-or-name>                        Run a cron task immediately
  af tasks remove <id-or-name>
Without --target-session each run creates a fresh session; {{line}} in a watch prompt is replaced by the emitted stdout line. On update, setting one trigger clears the other, --target-session "" reverts to session-per-run, and --project-path moves the task to another project (--repo still scopes its current project). A task is bound to one project at creation and "tasks add" echoes the binding as project_path — check it matches the project you meant, and never create a task from a scratch clone of a repo, which binds the automation to the clone instead of the real project. The background daemon runs all schedules; "af daemon install" / "af daemon uninstall" manage its login autostart.

` + afBacklogSection + `

Root-agent configuration (global unless --project <id-or-path> selects a personal project override):
  af config set root_agent <compact-json>             Merge the enabled/program profile as one table
  af config set root_agent.enabled <bool>             Enable or disable the singleton profile
  af config set root_agent.program <command>          Set the command while preserving enabled
Root-agent configuration is frozen at daemon start. An already-running root session is adopted as-is; after changing its program, disabling it, or removing its enabling entry, restart the daemon first and then kill that session.

Creating or prompting a session: the prompt is the entire contract, because the receiving agent inherits no context from your conversation. State everything it needs, including the expected output shape, e.g. "Open a PR titled X, link it back, do not merge" or "Write a report to <file> and stop; no code changes".`

// afBacklogSection is the backlog part of afUsageBody, a constant of its own so
// guidance can drop it where the backlog is unreachable (guidanceCaps.HostStores).
// It teaches reading and adding drafts only: queueing and starting items are the
// user's, so an agent outside an item session does them only when asked.
const afBacklogSection = `Backlog (one project's work items in a strict order; ids are B-12 or 12; items you add are always drafts; do not queue or start items unless the user asks):
  af backlog list [--all] [--state <s>]                Items in order, as a tree, with each one's state (--all includes done and dropped)
  af backlog show <id>                                 One item: body, state and reason, waits-on, children, runs, and thread
  af backlog add <title> [--body <text or ->] [--parent <id>] [--waits-on <id>,…]  Add one draft item
  af backlog add --file <path or ->                    Add or update many items at once from JSON: {"proposal": "<slug>", "items": [{"key", "title", "body", "parent", "waits_on", "program", "sequential"}]}; every item needs a title; parent and waits_on take a key (a string) or an item id (a number, such as 12), and "12" or "B-12" as a string is read as a key; with "proposal" set, an item with the same proposal and key is updated, not added again`

// afUsageBacklogInside is inside-only: --self resolves the calling session, so
// it works only in a session af started for a backlog item. The refused list is
// the one api/backlog_self.go enforces; TestItemSessionGuidanceNamesEveryRefusal
// (api) derives that list from the commands and pins this sentence to it.
const afUsageBacklogInside = `Backlog items: when af started your session for a backlog item (your first prompt says "You are working on backlog item B-12"), report through that item, not only in your reply:
  af backlog done --self [--pr <url>]                  The work is finished; the item goes to the user for review. Pass --pr when you opened a pull request
  af backlog ask --self <text>                         Ask the user a question on the item, then stop; when af can resume this conversation it archives this session, and the answer restores it and arrives here as a prompt; otherwise the session stays live
  af backlog ask --self <text> --then wait             The same, but this session stays live until the answer arrives
  af backlog ask --self <text> --handoff-file <path or ->  The same, with notes (at most 8 KB) for whoever continues: a new session if this one is gone by the answer, or this one if it comes back without its conversation; the user sees them in af backlog inbox
  af backlog note --self <text>                        Add a note to the item's thread; nobody is notified
  af backlog add <title> [--parent <id>]               Add more work you found as a draft (under the item yours is part of: the "It is part of B-1" line of your first prompt); the user decides whether to queue it
` + AfBacklogItemSessionAddForm + ` ` + AfBacklogItemSessionRefusals + ` If you need one of them, ask with "af backlog ask --self". Do not create other af sessions from an item session.`

// AfBacklogItemSessionAddForm is the sentence of afUsageBacklogInside that names
// the one form of add an item session may run (decision (3) on #6284). Exported
// so the api package, which enforces the form, can pin it to the flags it
// allows.
const AfBacklogItemSessionAddForm = `From an item session, add takes only a title and --parent; the item is always a draft, and af notes on its thread which item's agent added it.`

// AfBacklogItemSessionRefusals is the sentence of afUsageBacklogInside that names
// what an item session cannot run. It is exported only so the api package, which
// owns those refusals, can pin it to them.
const AfBacklogItemSessionRefusals = `From an item session af refuses these, which are the user's: edit, move, queue, draft, review, reopen, start, done <id>, drop, delete, note <id>, answer, run, pause, unpause, archive, unarchive, and changing the runner or budget.`

// afUsageProposalsInside names only the entry points; the working rules are
// printed by af proposal start and attach, so they can change without a
// skill update. Proposal commands need a session, so plugins do not get it.
const afUsageProposalsInside = `Proposals: when the user asks to make a proposal (a plan or design written as docs you edit together), run "af proposal start <slug>" with a short lowercase-dashed slug. When the user asks you to work on an existing proposal, run "af proposal list" to find it and "af proposal attach <slug>". Both print the proposal's path and a "rules" field: follow those rules for the rest of the work. Do not write proposal docs into the repository yourself; af keeps them outside the worktree.`

// afSkillDescriptionInside is AfSkillDescription for an agent inside an af
// session, where proposal commands work.
const afSkillDescriptionInside = "Manage Agent Factory (af) sessions, tabs, scheduled tasks, proposals, the backlog, and the daemon via the af CLI"

// afProposalTrigger is the activation text for proposal requests. Agents load a
// skill body only when the request matches its description, and "make a
// proposal" does not match a description about sessions and tasks.
const afProposalTrigger = "Use when the user asks to make, write, draft, or start a proposal, plan, spec, or design doc, or to work on, continue, review, or update an existing proposal"

// afUsageAttentionInside is inside-only: the user scans many sessions at once,
// and an agent that stops to wait gives no other sign that it needs them.
const afUsageAttentionInside = `Asking for the user: the user watches many sessions at once and cannot tell that you are blocked on them. Run "af sessions attention --self --reason '<what you need, in one line>'" only when you are blocked on a decision or question that only the user can answer, as the last step before you stop, after you write the question in your reply. Do not run it when a task has simply finished or at the end of an ordinary turn. It clears on its own when the user replies; "af sessions attention --self --clear" removes it if you no longer need them.`

// afUsageOutroInside is the wrap-up for an agent that IS an af session: it can
// self-archive, and doing so kills it.
const afUsageOutroInside = `Finishing up: when the user confirms your work is complete and asks you to wrap up, self-archive with "af sessions archive --self". It is non-destructive — the worktree is moved out, nothing is deleted, and the session is restorable later with "af sessions restore <title>". But it tears down THIS session's tmux, so it kills you the instant you run it: nothing you say or do after it is ever seen. Treat it as the ABSOLUTE LAST action — first finish every summary, result, and confirmation the user needs, and only once you have nothing left to report run "af sessions archive --self" as the very last step.`

// afUsageOutroPlugin is the wrap-up for an agent that is NOT an af session.
// The inside version would be actively wrong here in both directions: there is
// no own session to archive, and the sessions this agent created keep running
// after the conversation ends unless it says so.
const afUsageOutroPlugin = `Finishing up: the sessions you create keep running after this conversation ends — they are separate agents in their own worktrees, not part of this one. When work in a session is done and reviewed, archive it with "af sessions archive <title>": non-destructive, the worktree is moved out, nothing is deleted, and it comes back with "af sessions restore <title>". Prefer archiving over "af sessions kill <title>", which permanently removes af-owned workspaces; uncommitted changes and unpushed commits there can be lost. "af sessions whoami", "af sessions archive --self", and "af sessions attention --self" resolve the CALLING session, so they only work from inside a session af launched — from here, always name the session.`

// afUsageOutro closes the reference for both audiences.
const afUsageOutro = `Maintenance: af version, af debug (print resolved config), af upgrade (self-upgrade). Never run "af reset": it kills every session and deletes ALL linked worktrees and their branches across repos.`

// shellQuote wraps a string in single quotes, escaping any embedded single
// quote with the standard POSIX idiom:
//
//	'   ->   '\''
//
// The idiom is written above as an indented code block on purpose: gofmt
// rewrites a doubled quote in doc PROSE into a typographic ” character, which
// is how this very comment previously came to claim the escape was '\" — an
// incorrect description of a security-relevant primitive, produced by the
// formatter rather than by a typo.
//
// It quotes UNCONDITIONALLY, so the empty string renders as a quoted empty word
// rather than vanishing — unlike config.ShellQuotePath, which returns ""
// unchanged. See internal/shellquote for why the tree's quoters are not
// interchangeable.
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
//   - Devin: BOTH seams. File seam — the af skill written to devin's home skills
//     dir ($HOME/.config/devin/skills, verified via `devin skills paths`), behind
//     the same global_agent_skills consent gate as codex/gemini/amp. And
//     --respect-workspace-trust false, appended CONDITIONALLY (devin rejects the
//     flag twice) to suppress its interactive workspace-trust modal so an af
//     worktree reaches ready without a human answering it.
//   - Commands running no known agent: no injection.
//
// Accepted tradeoff (#1585 review, finding 2): DetectAgentFromCommand is a shared
// basename heuristic, so a program_overrides entry pointing a NON-<agent> binary
// named "codex"/"gemini"/"amp"/"aider" reaches the matching branch. For the file
// seams this is benign (the launch command is unchanged; the worst case is a
// dormant af-owned skill dir). For the flag seams (claude, aider) it carries the
// same pre-existing #1116/#1131 exposure claude already has; we do NOT re-derive
// agent-ness with a second heuristic — the #1132 rule is one detection choke-point.
// injectSystemPrompt adds af's guidance to the resolved command, and for the two
// agents that DISCOVER it from a config root, places it where the launch will
// actually look: target carries the account boundary's config root for a scoped
// session (#3645) and the root the command itself resolves to for an unscoped one
// (#4501). The zero value places nothing; see skillTarget.
//
// caps renders the guidance on the PER-INSTANCE seams (claude's --plugin-dir,
// aider's --read) from the session's capability projection — no af verbs without
// a callback (#5571 W3.1), only the tab kinds the projection admits, and
// self-archive only where it works (W3.3). The shared config-root seams
// (codex/gemini/amp/devin/opencode) keep the full doc: they are opted into
// globally, so one file cannot vary per session.
func injectSystemPrompt(resolved string, target skillTarget, caps guidanceCaps) string {
	return injectSystemPromptWithEnv(resolved, target, caps, launchEnv{})
}

// launchEnv is the launch's own environment inputs: the session's merged
// exact-name pass-through list and the directory the pane starts in. The
// opencode seam asks whether a name the user supplied will survive the pane's
// filter, and that answer depends on both (#5981). The zero value gives the
// conservative "no extras, remote workDir" answer. It is deliberately not part
// of skillTarget, which identifies WHERE the skill lands and is compared by
// value.
type launchEnv struct {
	passthrough []string
	workDir     string
}

// launchEnvFor is the launch environment of session i starting in workDir.
func launchEnvFor(i *Instance, workDir string) launchEnv {
	return launchEnv{passthrough: SessionEnvPassthroughForInstance(i), workDir: workDir}
}

// injectSystemPromptWithEnv is injectSystemPrompt for a launch whose
// environment inputs are known.
func injectSystemPromptWithEnv(resolved string, target skillTarget, caps guidanceCaps, env launchEnv) string {
	switch tmux.DetectAgentFromCommand(resolved) {
	case tmux.ProgramClaude:
		pluginDir, err := ensurePluginDir(caps)
		if err != nil {
			log.WarningLog.Printf("failed to set up plugin directory, slash commands unavailable: %v", err)
			return resolved
		}
		// Unconditional append is safe here, unlike provider approval and
		// sandbox flags: no binary has ever persisted --plugin-dir into
		// Instance.Program (the injected form only reaches tmux SetProgram),
		// and claude's --plugin-dir is repeatable, so one in a user's
		// program_overrides is additive rather than a conflict.
		out := resolved + " --plugin-dir " + shellQuote(pluginDir)
		// Opt-in plan-usage tap ([backlog] plan_usage): nothing is added, and
		// nothing logged, unless the user turned it on.
		if planUsageEnabled() {
			if hasSettingsFlag(resolved) {
				log.WarningLog.Printf("plan usage: the claude command already passes --settings, so af does not add its status line tap")
			} else if settings := planTapSettings(target.root); settings != "" {
				out += " --settings " + shellQuote(settings)
			}
		}
		return out
	case tmux.ProgramCodex:
		if _, err := ensureCodexSkillDir(target); err != nil {
			log.WarningLog.Printf("failed to set up codex af skill, af guidance unavailable to codex: %v", err)
		}
		// Keep codex's startup update picker from ever rendering for a launch
		// af owns: af answering it is exactly the interaction that let a
		// respawn die seconds after restore (#5238). Shared with the
		// config-agent spawn through tmux.EnsureCodexUpdateCheckDisabled, one
		// place so the two launch paths cannot drift — the same contract
		// devin's trust flag holds on the line below.
		return tmux.EnsureCodexUpdateCheckDisabled(resolved)
	case tmux.ProgramGemini:
		if _, err := ensureGeminiSkillDir(target); err != nil {
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
		//
		// A user-supplied OPENCODE_CONFIG reaches this branch through TWO
		// channels, and af must skip injection for EITHER so it does not
		// silently shadow the user's file:
		//
		//   1. An inline assignment in the resolved command (a program_overrides
		//      entry like `OPENCODE_CONFIG=~/mine.jsonc opencode`), detected by
		//      opencodeCarriesConfigEnv. Shell semantics give the LAST
		//      assignment precedence, so af's injected prefix would lose to it
		//      anyway — but detecting it lets af SAY the guidance did not land.
		//   2. An exported OPENCODE_CONFIG in the launching process
		//      environment, which the opencode allowlist
		//      (internal/sessionenv) deliberately delivers to the pane.
		//      Unlike channel 1, a command-prefix assignment SHADOWS an
		//      already-exported variable of the same name in POSIX shells, so
		//      injecting af's own OPENCODE_CONFIG would silently override the
		//      user's exported value and their config file would never be read
		//      — the exact silent outcome this guard exists to prevent.
		//
		// Only OPENCODE_CONFIG needs this treatment: af injects solely that
		// name; OPENCODE_CONFIG_DIR / OPENCODE_CONFIG_CONTENT pass through the
		// allowlist unshadowed. Checking os.Getenv here mirrors the same
		// os.Environ source that prepareLaunchEnvironment (session/tmux)
		// filters into the pane, so the env-channel decision sees exactly the
		// value the pane would have inherited.
		//
		// The env channel only reaches the pane when the opencode allowlist
		// actually applies to the launch — i.e. when the resolved command is
		// operator-attributable, so prepareLaunchEnvironment's
		// GrantAgentForCommand admits the opencode name set and
		// FilterForCommand keeps OPENCODE_CONFIG. A non-attributable spelling
		// (a relative `./opencode`, or an `ionice … opencode` wrapper
		// GrantAgentForCommand does not model) reaches this branch through
		// DetectAgentFromCommand but is granted "" and filtered to the common
		// set, so the user's exported OPENCODE_CONFIG never reaches the pane.
		// Honoring it there would skip injection for a value the pane will not
		// see, leaving the pane with neither the user's config nor af's — af
		// would silently drop its own guidance while logging that it honored
		// the user's. opencodeExportedConfigReachesPane gates the env channel
		// on the same grant predicate the pane uses, so a non-attributable
		// command falls through to af's injection (the pre-env-channel
		// behavior) and the pane still gets af's config. The inline-assignment
		// channel (opencodeCarriesConfigEnv) is in the command string, which
		// FilterForCommand never touches, so it is not subject to this gate.
		if opencodeCarriesConfigEnv(resolved) || opencodeExportedConfigReachesPane(resolved, env.passthrough, env.workDir) {
			// INFO, not WARNING (#2166): the user pointed OPENCODE_CONFIG at
			// their own file (program_overrides assignment OR exported env var
			// that the pane's allowlist will actually deliver) and af is
			// honoring it.
			log.InfoLog.Printf("opencode already has OPENCODE_CONFIG (command or environment); leaving it alone (af guidance not injected for opencode)")
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
		// and preflight's shared launch resolver skip leading VAR=VALUE words
		// (the `env FOO=1 <agent>` wrapper case, #742), so the agent is still
		// detected at the right token.
		return "OPENCODE_CONFIG=" + shellQuote(path) + " " + resolved
	case tmux.ProgramAider:
		// Aider takes a flag, not a file drop: it has no auto-discovered global
		// skills folder. A write failure (or a non-af file at our path) yields an
		// empty path, in which case we must NOT append --read pointing at a file
		// we do not own — return the launch command unchanged.
		path, err := ensureAiderReadFile(caps)
		if err != nil {
			log.WarningLog.Printf("failed to set up aider af skill, af guidance unavailable to aider: %v", err)
			return resolved
		}
		if path == "" {
			return resolved
		}
		return resolved + " --read " + shellQuote(path)
	case tmux.ProgramDevin:
		if _, err := ensureDevinSkillDir(); err != nil {
			log.WarningLog.Printf("failed to set up devin af skill, af guidance unavailable to devin: %v", err)
		}
		// af created and owns this worktree, so it is trusted: suppress devin's
		// interactive workspace-trust modal so the session reaches ready without a
		// human answering it. The suppression — and its guard against devin's
		// rejection of a repeated flag — is shared with the config-agent launch path
		// through tmux.EnsureDevinWorkspaceTrustSuppressed, one place so the two
		// launch paths cannot drift (#2435; the #729/#2097/#2416 drift class).
		return tmux.EnsureDevinWorkspaceTrustSuppressed(resolved)
	}
	return resolved
}

// opencodeExportedConfigReachesPane reports whether a user's EXPORTED
// OPENCODE_CONFIG would actually reach the opencode pane's environment, so the
// guard in injectSystemPrompt can decide whether honoring it (and skipping af's
// own injection) leaves the pane with the user's config rather than nothing.
//
// The opencode allowlist in internal/sessionenv admits OPENCODE_CONFIG only when
// prepareLaunchEnvironment attributes the command to opencode through
// GrantAgentForCommand — a relative `./opencode` or an `ionice … opencode`
// wrapper the grant predicate does not model reaches the opencode branch through
// DetectAgentFromCommand but is granted "" and filtered to the common set, so
// the exported value never reaches the pane. DetectAgentFromCommand and
// GrantAgentForCommand diverge by design: the former classifies what the command
// runs for guidance injection, the latter what the operator may carry credentials
// for, and a non-attributable spelling is exactly the case the credential grant
// refuses.
//
// This helper asks the pane's own filter (FilterForCommand over the grant the
// launch uses) so the env-channel guard does not skip af's injection for a value
// the pane will never see — which would leave the pane with neither the user's
// config nor af's, while the INFO log claims the user's value was honored. The
// converse matters as much: a value the user explicitly named in the session's
// pass-through list reaches the pane even for a spelling the grant refuses, and
// af must not shadow it with a command-prefix assignment (#5981). passthrough
// and workDir are the launch's own (launchEnv carries them); zero values give
// the conservative "no extras, remote workDir" answer.
func opencodeExportedConfigReachesPane(resolved string, passthrough []string, workDir string) bool {
	if os.Getenv("OPENCODE_CONFIG") == "" {
		return false
	}
	// Ask the pane's own filter rather than re-deriving its rules: the value
	// reaches the pane when FilterForCommand keeps it, whether the opencode
	// allowlist admits it (an attributable command) or the operator named it in
	// the pass-through list (which survives for an unattributable spelling too).
	source := os.Environ()
	agent := sessionenv.GrantAgentForCommand(resolved, source, workDir)
	for _, entry := range sessionenv.FilterForCommand(source, agent, resolved, passthrough) {
		if name, value, ok := strings.Cut(entry, "="); ok && name == "OPENCODE_CONFIG" && value != "" {
			return true
		}
	}
	return false
}
