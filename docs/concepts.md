# Concepts

For anyone who has installed `af` and wants the ideas behind it. After reading
this you will know the five terms every other page assumes — session, worktree,
daemon, task, account — and how a prompt becomes a branch you can merge.

Five terms carry the whole tool.

## Session

**A session is one agent working on one task.** You give it a name and usually a
starting prompt; `af` picks the agent (your default, or one you choose on the
form), prepares a workspace, and starts the agent inside it.

A session is the unit everything else is scoped to. It has a status you can scan
without attaching, a **tab strip** — its agent tab plus any shell, process, web,
or VS Code tabs you add in the same workspace — and an explicit lifecycle:

- **Archive** is the default way to finish. The processes stop, the workspace
  moves aside, and the record and branch stay restorable.
- **Restore** brings an archived, lost, or dead session back, resuming its
  recorded conversation where the agent supports it.
- **Kill** is permanent. It destroys the session and prunes the workspace and
  branch `af` owns, including uncommitted or unmerged work.

Full page: [Sessions and worktrees](sessions.md).

## Worktree

**A worktree is the isolated checkout each session gets.** `af` cuts a branch
(prefixed per your config) and adds a [git
worktree](https://git-scm.com/docs/git-worktree) for it next to your repository,
runs any configured setup commands, and launches the agent in that directory.

This is the isolation, and it is deliberately unremarkable: five agents can edit
overlapping files at once because each one sees only its own working tree, and
what comes back is an ordinary branch. There is no proprietary workspace to
export from — review it with `git diff`, push it, open a pull request, run CI:

```bash
git -C ../your-project-fix-auth diff
```

Pass `af sessions create --here` when you deliberately want an agent in the
checkout you already have, on the branch you already have; then no branch or
worktree is created, and killing the session never touches your tree.

Full page: [Sessions and worktrees](sessions.md).

## Daemon

**The daemon is the background process that owns all state.** Session records,
task definitions, the worktrees on disk — every mutation goes through it. The
TUI, the web client, and the CLI are thin clients: they render its projection
and ask it to change things, so they cannot disagree about what exists, and
closing any of them does not stop the work.

It also keeps sessions alive across process death and reboots, runs the
scheduler, handles usage-limit parking and resume, and serves the web client. It
starts on demand when there is work to host; install its autostart unit once to
keep tasks firing across logouts:

```bash
af daemon install   # systemd user service on Linux, launchd agent on macOS
af daemon status    # liveness, supervision ownership, config freshness
```

Full page: [The daemon](daemon.md).

## Task

**A task is a prompt the daemon delivers on its own.** Every task has exactly
one trigger and one delivery mode. The trigger is either a **cron** schedule or
a **watch** command whose every stdout line fires the task; delivery either
creates a fresh session per fire or sends the prompt into an existing one.

```bash
# every weekday at 09:00, in a fresh session
af tasks add --name "Daily triage" --cron "0 9 * * 1-5" \
  --prompt "Triage new issues and propose next actions"

# one prompt per line the watcher prints, into a session that persists
af tasks add --name "CI failures" --watch-cmd "./watch-ci.sh" \
  --prompt "Investigate this CI failure: {{line}}" --target-session triage
```

Because the daemon hosts them, there are no per-task cron entries or OS units to
manage, and a run that hits a usage limit parks instead of failing.

Full page: [Tasks](tasks.md).

## Account

**An account is a named credential home for one agent.** Agent CLIs read their
login from a directory; an account is such a directory that `af` creates and
names, so a session can be pinned to a specific Claude, Codex, or Gemini login
rather than whichever one your shell happens to have.

```bash
af accounts login codex work   # register it, then run codex's own login in it
af sessions create --name spike --account work
```

`af` never reads, stores, or forwards the credential itself — it decides which
directory the agent sees, and the agent's own login flow puts the material
there. It also refuses an unproved fallback and never rotates accounts on its
own. The TUI's naming form (`ctrl+o`) and the web client's new-session modal
offer the same field.

Full page: [Accounts and usage limits](usage-limits.md).

## The short version

```text
prompt
  → session
  → branch + worktree
  → agent + helper tabs
  → web / TUI / CLI supervision
  → git review
  → archive, restore, or kill
```

Next: [Getting started](getting-started.md) for the first-run walkthrough, or
pick the surface you want to drive it from — the [web client](web.md), the
[TUI](tui.md), or the [CLI](cli.md).
