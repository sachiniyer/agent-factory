# Usage limits

Subscription-plan agents (`claude`, `codex`) can hit a **plan usage-limit
wall**: the CLI stops working and prints a banner like *"Claude usage limit
reached. Your limit will reset at 2pm (America/New_York)"* or *"You've hit your
usage limit … try again at Jul 25th, 2026 5:55 PM"*. The agent is not dead — it
is parked until its limit window resets.

Agent Factory detects that state, surfaces it, and can bring the session back on
its own once the window elapses. This page covers the whole flow end to end.

## What is detected

- **`claude` and `codex`** — their usage-limit banners are recognized, and when
  the banner states a reset time it is parsed into an absolute instant. These
  are the plan-metered agents that stall at a dead prompt with a reset window,
  so they are the only ones that get auto-resume.
- **`gemini` and `aider`** — **not** detected. They are API-key-metered: a
  "limit" there is a transient HTTP 429 the CLI already retries, with no plan
  reset time to schedule against. They are surface-only in the sense that
  nothing special happens — no badge, no auto-resume.
- **`opencode`** — **not** detected, deliberately. It is API-key-metered against
  your own Anthropic/OpenAI credentials, and its TUI reports spend (`$0.10
  spent`) rather than a plan wall — there is no plan-reset banner to match and no
  reset window to schedule against, so af ships no usage-limit matcher for it.

Detection runs on captured pane content, so it needs no agent cooperation. You
can tune the detection regex per agent with
[`limit_patterns`](#custom-detection-patterns).

## The `[limit]` badge

When the daemon's status poll sees a usage-limit banner for a `claude`/`codex`
session, it marks the session **LimitReached**. In the sidebar the row shows a
`[limit]` badge, and — when the banner carried a parseable reset time — when the
limit resets:

```
▸ fix-auth-bug   [limit] resets 2:00 PM
```

`[limit]` means "parked at a usage-limit wall", distinct from working, idle
(ready), or dead. The badge clears automatically once the session resumes work
(whether you resume it, the daemon auto-resumes it, or the banner scrolls away
on its own).

## Manual retry

Resume a limit-blocked session immediately from any surface:

- TUI: select it and press **`c`**.
- Web: select it and click **Retry**.
- CLI: run `af sessions retry-limit <title>` (with `--repo` when needed).

Every surface calls the same daemon recovery action. It:

1. Re-spawns the agent if its tmux session exited while blocked (the rare case);
   a live stall just gets nudged.
2. Re-delivers the pending prompt — a task-driven session re-sends its **stored
   task prompt** so it resumes its actual work; an interactive session with no
   stored prompt is sent a bare `continue` (which loses the agent's prior
   in-context state, an unavoidable limitation — see
   [anthropics/claude-code#5977](https://github.com/anthropics/claude-code/issues/5977)).
3. Clears the `[limit]` badge so the poll re-resolves the real state.

The TUI's `c` binding is rebindable like any other via the `[keys]` table
(action `limit_retry`); see
[configuration.md](configuration.md#key-bindings-keys).

## Opt-in auto-resume

By default a limit is **surface-only**: the badge plus the manual retry. Set
`limit_auto_resume = true` in your global config to let the **daemon** resume a
parked `claude`/`codex` session on its own once its limit window elapses:

```toml
limit_auto_resume = true
limit_retry_interval = "30m"   # fallback cadence when a banner states no reset time
```

- **Parsed reset time** → the daemon resumes shortly after that time (a small
  grace buffer is added because limit windows are rolling and approximate). A
  reset time already in the past resumes promptly.
- **No parseable reset time** → the daemon retries on the fixed
  `limit_retry_interval` cadence. Set it to empty or `0` to leave such a session
  surface-only.
- **Re-limit backoff** → if a resumed session immediately hits the wall again,
  the daemon backs off exponentially (settling at one attempt every 5 minutes)
  rather than hammering an exhausted plan. Killing the session is always the
  off-ramp.
- **Global-only.** `limit_auto_resume` and `limit_retry_interval` are rejected
  in in-repo configs and take effect on the next daemon restart.

Full config reference: [configuration.md](configuration.md#usage-limit-auto-resume).

## Hand off to another agent

Waiting is not the only option. If the work should not sit until the window
resets — a day, sometimes several — hand the session to a **different agent**:

```
af sessions handoff fix-auth --to claude
```

In the TUI, press **`F`** on the selected session, pick the agent, and confirm.
The key is advertised on the status bar for a limit-blocked session, next to the
`c` retry, because the two are the two answers to the same wall: `c` waits for
*this* agent's window, `F` continues under another one.

What a handoff does, and does not do:

- **The session is the same session.** Same worktree, same branch, same tabs,
  same task binding, same name. Only the agent process is replaced. Nothing is
  archived, nothing is re-cloned, and uncommitted work is untouched — it is
  simply still there, because the worktree never moved.
- **The new agent starts fresh, with a brief.** Agent conversations are not
  portable between providers: claude cannot read codex's transcript and vice
  versa. So instead of a transcript, the incoming agent is told the session's
  goal, that it is continuing someone else's work, and where to look
  (`git log`/`git diff` on the branch). It is explicitly told not to start over.
- **The swap is recorded.** af notes which agent handed off to which, and the
  branch tip at that moment. That tip is the attribution boundary: everything up
  to it is the outgoing agent's work, everything after is the incoming agent's.
  af cannot label the commits themselves — your agent writes them — so the
  recorded tip is what lets a reviewer check the split rather than take it on
  trust.

Use `--brief` when the stored goal is stale or too broad, which is common on a
long-running session:

```
af sessions handoff fix-auth --to gemini --brief "just finish the retry test; leave the docs alone"
```

Any supported agent can be a target. Two things worth knowing rather than being
blocked on:

- **Approval policy belongs to the target agent.** A handoff starts the target's
  resolved command and configuration; it does not carry approval settings from
  the outgoing agent. See [Agent approval
  behavior](configuration.md#agent-approval-behavior).
- **Local-worktree sessions only.** A docker/ssh/hook session runs its agent
  inside a provisioned sandbox, where swapping the agent is a different
  lifecycle; those sessions refuse the handoff rather than half-perform it.

Handing off is **reversible**. Each agent's conversation history is stored per
directory, so the outgoing agent's thread is still in the worktree — hand back
to it once its limit resets and it picks up its own conversation.

There is no automatic handoff. A swap changes which agent is editing your
branch, so it is always something you ask for.

## Task runs: park, don't fail

A **task** (cron or watch) can fire while your plan is already exhausted. When a
task-driven session hits a usage-limit wall as it starts up — before its prompt
is even delivered — Agent Factory **parks** the run instead of failing it:

- The session is **kept**, not torn down, and marked `[limit]` (with its reset
  time) so the badge, the manual `c` retry, and auto-resume all apply to it.
- The task's run status is recorded as **`parked: usage limit`** — *not* an
  errored/failed run. It shows in the task manager as waiting for the limit
  window, and no failure side-effects fire.
- Once the window resets, the **same resume machinery** takes over: auto-resume
  (if `limit_auto_resume` is on) or your manual `c` retry re-delivers the
  session's stored task prompt, and the run proceeds to completion. A parked run
  becomes a completed one — never a failed one.

Before this behavior, such a run spun a readiness timeout and was recorded as a
failure even though nothing was actually wrong — you'd just hit your plan limit.

## Scope summary

| Agent | Detected | `[limit]` badge | Manual `c` retry | Auto-resume | Task park | Handoff target |
|-------|:--------:|:---------------:|:----------------:|:-----------:|:---------:|:--------------:|
| `claude` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `codex` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `gemini` | — | — | — | — | — | ✅ |
| `aider` | — | — | — | — | — | ✅ |
| `amp` | — | — | — | — | — | ✅ |
| `opencode` | — | — | — | — | — | ✅ |

Auto-resume covers `claude`/`codex` because their banners carry a parseable
reset window; other supported agents either do not expose a known plan-reset
banner or are API-key-metered (transient 429s the CLI retries) with no plan
window to schedule against.

The last column runs the other way, and deliberately so. Detection answers "can
af tell this agent hit a wall", which only `claude`/`codex` support — so only
they can be handed *from* on a limit. Being handed *to* needs nothing from the
agent at all: af stops one process and starts another in the same worktree, so
every supported agent is a valid destination.

## Custom detection patterns

If an agent reworded its banner, override the detection regex per agent with
`limit_patterns` (the built-in reset-time parser is kept):

```toml
[limit_patterns]
claude = "Claude usage limit reached\\."
codex  = "You've hit your usage limit"
```

Keys must be a supported agent (`claude`, `codex`, `aider`, `gemini`, `amp`,
`opencode`); an override for an agent with no built-in matcher
(`aider`/`gemini`/`amp`/`opencode` today) is ignored, and an uncompilable regex
warns and falls back to the built-in default.
See [configuration.md](configuration.md#custom-usage-limit-detection-limit_patterns).
