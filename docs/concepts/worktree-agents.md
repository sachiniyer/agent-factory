# Worktree-isolated agents

The core idea in Agent Factory is simple: **one agent, one git worktree, one
branch.** Every session you create is a real, separate working tree of your
repository, checked out on its own branch, with the agent running inside it.

## Why worktrees

Running several AI agents against a single checkout is a recipe for chaos: they
overwrite each other's edits, trip over half-finished changes, and leave you
unable to tell whose work is whose. A [git
worktree](https://git-scm.com/docs/git-worktree) solves this at the level git
already understands — it's a second (third, fourth…) checkout of the *same*
repository, sharing history but with its own working directory and branch.

Because each session gets its own worktree:

- **Agents never collide.** Five sessions can edit overlapping files at the same
  time; each sees only its own working tree. There is no shared mutable state to
  race on.
- **Every session is reviewable.** The work lands on a branch. You review and
  merge it with your normal git and pull-request flow — nothing about Agent
  Factory is in the way of `git diff`, `gh pr create`, or your CI.
- **Finishing work is restorable by default.** Archive a session and `af` moves
  the worktree aside with the branch and changes intact, ready for restore. If
  you really want to discard it, kill refuses recoverable branch/worktree work
  unless you force the permanent cleanup.

## The lifecycle of a session

1. **Create.** You give a session a name (and optionally a starting prompt).
   `af` creates a branch (prefixed per your config, e.g. `af/`), adds a worktree
   for it (next to your repo by default; see `worktree_root` in
   [configuration](../configuration.md) for the subdirectory option), and starts
   your chosen agent in that directory. Any configured post-worktree setup commands
   run first, so the agent starts in a ready environment.
2. **Work.** The agent runs in its worktree. You watch it in the Agent tab,
   interact in-pane, or attach full-screen. Extra [tabs](tui.md#tabs) can run a
   shell or a long-lived process (a dev server, a test watcher) in the *same*
   worktree, sharing the agent's files.
3. **Archive.** Archiving is the default "done" action: it tears down the
   session's tmux and moves its worktree out of the way, but keeps the record
   and branch. Restore it later and the worktree comes back and the agent
   re-spawns.
4. **Kill.** Killing a session permanently ends the agent, removes the worktree,
   and deletes the branch when Agent Factory owns it. It refuses by default
   unless it can confirm the worktree is clean and HEAD has no commits
   unreachable from the session's base/default branch; use `--force` only when
   you mean to discard that work.

## In-place sessions

Sometimes you don't want a new worktree — you want an agent in the repo you're
already sitting in, on the branch you already have checked out. `af sessions
create --here` (alias `--in-place`) does exactly that: the agent runs in the
repo root at its current branch, **no worktree or branch is created**, and
killing the session never removes your working tree or branch. This is also how
the always-on [root agent](../configuration.md#root-agents-always-ensured)
attaches to a repo.

## Local and remote

Everything above describes **local** sessions — worktrees on the machine running
`af`. Sessions can also run on a **remote** backend you define, through
[remote hooks](../remote-hooks.md): your own scripts launch, list, attach to,
and delete sessions elsewhere, and they show up in the same sidebar with the
same Agent tab, attach, and kill experience. The worktree-isolation model is the same;
only the machine changes.

## Who owns the state

You never manage worktrees by hand, and the TUI isn't the thing that creates
them either. A background **daemon** is the single writer that owns every
session record and performs every worktree operation — see
[The daemon](daemon.md). That's what keeps the sidebar, the CLI, and the HTTP
API showing the same truth, and what lets a dead agent process be re-spawned
without losing its worktree.
