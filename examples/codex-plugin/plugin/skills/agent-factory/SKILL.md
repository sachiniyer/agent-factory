---
name: agent-factory
description: Run and manage Agent Factory (af) coding-agent sessions, tabs, and scheduled tasks from the shell
---

Agent Factory (`af`) is a terminal multiplexer that runs each AI coding agent in
an isolated git worktree. Use it to hand work to a background agent instead of
doing it in this conversation, and to schedule recurring agent runs.

## Before you use it

`af` is a separate CLI and is not bundled with this plugin. Check it is
installed:

```
af version
```

If that fails, tell the user to install it and stop — do not attempt to download
or install a binary yourself:

```
curl -fsSL https://raw.githubusercontent.com/sachiniyer/agent-factory/master/install.sh | bash
```

Session and task commands need the af daemon, which starts on demand; `af daemon
status` reports it.

## Commands

Commands print JSON on stdout. Run `af <command> --help` for full flag lists.

Every session and task command is PROJECT-SCOPED: it acts on the current
directory's project unless `--repo <path>` names another one. Session titles are
unique within a project, not across projects. Task ids are globally unique but
still project-scoped.

Sessions (one agent per isolated worktree):

```
af sessions list                                     List sessions
af sessions get <title>                              Fetch one session
af sessions create --name <title> [--prompt <p>] [--program claude|codex|aider|gemini|amp|opencode]
af sessions send-prompt <title> <prompt> [--create]  Send a prompt
af sessions preview <title>                          Snapshot a session's terminal output
af sessions watch <title>                            Block until the session goes idle
af sessions archive <title>                          Archive it (restartable; nothing is deleted)
af sessions restore <title>                          Restore an archived session
af sessions kill <title>                             Kill it and clean up its worktree
```

Tabs (extra processes in a session's worktree; max 9 per session):

```
af sessions tabs <title>
af sessions tab-create <title> --command <cmd> [--name <tab>]
af sessions tab-delete <title> --name <tab>
```

Tasks (deliver a prompt on a cron schedule, or whenever a watch script prints a
stdout line):

```
af tasks list [--all]
af tasks add --name <n> --prompt <p> --cron "0 9 * * *" [--target-session <title>]
af tasks add --name <n> --watch-cmd <cmd> [--prompt "... {{line}} ..."]
af tasks trigger <id>
af tasks remove <id>
```

## Writing a prompt for a session

The prompt is the entire contract: the receiving agent inherits no context from
this conversation. State everything it needs, including the expected output
shape — e.g. "Open a PR titled X, link it back, do not merge" or "Write a report
to <file> and stop; no code changes".

## Destructive commands

`af sessions kill` deletes the session's worktree and branch — prefer `af
sessions archive`, which is restorable. Never run `af reset`: it kills every
session and deletes all linked worktrees and their branches across every repo.
Confirm with the user before either.
