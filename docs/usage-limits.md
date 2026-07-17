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

## Manual retry (the `c` key)

Select a limit-blocked session and press **`c`** to resume it immediately. This:

1. Re-spawns the agent if its tmux session exited while blocked (the rare case);
   a live stall just gets nudged.
2. Re-delivers the pending prompt — a task-driven session re-sends its **stored
   task prompt** so it resumes its actual work; an interactive session with no
   stored prompt is sent a bare `continue` (which loses the agent's prior
   in-context state, an unavoidable limitation — see
   [anthropics/claude-code#5977](https://github.com/anthropics/claude-code/issues/5977)).
3. Clears the `[limit]` badge so the poll re-resolves the real state.

The `c` binding is rebindable like any other via the `[keys]` table (action
`limit_retry`); see [configuration.md](configuration.md#key-bindings-keys).

## Opt-in auto-resume

By default a limit is **surface-only**: the badge plus the manual `c` retry. Set
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

| Agent | Detected | `[limit]` badge | Manual `c` retry | Auto-resume | Task park |
|-------|:--------:|:---------------:|:----------------:|:-----------:|:---------:|
| `claude` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `codex` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `gemini` | — | — | — | — | — |
| `aider` | — | — | — | — | — |
| `amp` | — | — | — | — | — |
| `opencode` | — | — | — | — | — |

Auto-resume covers `claude`/`codex` because their banners carry a parseable
reset window; other supported agents either do not expose a known plan-reset
banner or are API-key-metered (transient 429s the CLI retries) with no plan
window to schedule against.

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
