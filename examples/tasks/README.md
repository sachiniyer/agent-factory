# Watch-task examples

Runnable skeletons for the `watch_cmd` trigger ([docs/tasks.md](../../docs/tasks.md)): the daemon keeps the script running, and every newline-terminated stdout line becomes one event — a prompt delivered to an agent session, with `{{line}}` substituted in the task's prompt.

| Script | Pattern |
|---|---|
| [`log-tail.sh`](log-tail.sh) | Stream a file: `tail -F` piped through a line-buffered `grep`. One event per matching log line. |
| [`gh-issue-poll.sh`](gh-issue-poll.sh) | Poll an API with a since-cursor the script persists itself. One event per newly opened GitHub issue; survives daemon restarts without replaying or dropping. |

## Quick start

```bash
chmod +x log-tail.sh gh-issue-poll.sh

af tasks add --name "log-errors" \
  --watch-cmd "LOG_FILE=/var/log/app.log $PWD/log-tail.sh" \
  --prompt "Investigate this error: {{line}}" \
  --target-session debugger

af tasks add --name "gh-issues" \
  --watch-cmd "REPO=owner/name $PWD/gh-issue-poll.sh" \
  --prompt "Triage this new issue: {{line}}"
```

With `--target-session` the prompt is sent into that session (auto-created if missing); without it, every event creates a fresh session.

## Rules of thumb

- **stdout is the event stream** — one event per line. Send all logging to stderr; it lands in `~/.agent-factory/logs/task-<id>.log`.
- **Exit 0 means "stop me"** (status `stopped`, no restart until re-enable). Exit non-zero on failure so the daemon restarts you with backoff — and so a persistent misconfiguration trips the crash-loop breaker (`errored`) instead of looping silently.
- **Stay under 10 events/min** per task; excess events are dropped (with a logged warning).
- **Own your cursor.** Events are not replayed across daemon restarts, so pollers should persist their resume point, like `gh-issue-poll.sh` does.
