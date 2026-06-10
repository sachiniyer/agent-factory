#!/usr/bin/env bash
# Watch-task skeleton: poll a GitHub repo and emit one event per newly opened
# issue. Demonstrates the polling-loop pattern with a since-cursor: the script
# owns its own resume point, so daemon restarts never replay or drop issues.
#
#   af tasks add --name "gh-issues" \
#     --watch-cmd "REPO=owner/name $PWD/gh-issue-poll.sh" \
#     --prompt "Triage this new issue: {{line}}" \
#     --target-session captain
#
# Contract reminders (see docs/tasks.md):
#   - stdout = events, one per newline-terminated line. Log to stderr only.
#   - stderr lands in ~/.agent-factory/logs/task-$AF_TASK_ID.log.
#   - Events are rate-limited to 10/min per task; this poller stays far below.
#   - The daemon does not replay events across restarts — the cursor file is
#     what makes this script resumable.
#
# Requires: gh (authenticated), jq.
#
# Env:
#   REPO            owner/name of the repo to watch (required)
#   POLL_INTERVAL   seconds between polls (default: 60)
#   STATE_FILE      cursor file (default: ~/.agent-factory/gh-issue-poll-<task id>.cursor)

set -euo pipefail

: "${REPO:?REPO is required (e.g. owner/name)}"
POLL_INTERVAL="${POLL_INTERVAL:-60}"
STATE_FILE="${STATE_FILE:-$HOME/.agent-factory/gh-issue-poll-${AF_TASK_ID:-default}.cursor}"

log() { printf '[gh-issue-poll] %s\n' "$*" >&2; }

now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

mkdir -p "$(dirname "$STATE_FILE")"
if [[ ! -f "$STATE_FILE" ]]; then
  now_utc > "$STATE_FILE"
  log "initialized cursor at $(cat "$STATE_FILE"); only issues opened after that fire events"
fi

if ! gh api "repos/$REPO" --silent >/dev/null 2>&1; then
  # Exit non-zero: misconfiguration is a failure, so the daemon retries with
  # backoff and the crash-loop breaker surfaces a persistent problem as
  # status "errored" instead of looping forever.
  log "cannot read repos/$REPO via gh — check the owner/name spelling and gh auth"
  exit 1
fi

log "task=${AF_TASK_NAME:-?} watching $REPO every ${POLL_INTERVAL}s"

while true; do
  since="$(cat "$STATE_FILE")"
  next="$(now_utc)"

  # One line per new issue: "#<number> <title> <url>". The whole line becomes
  # {{line}} in the task prompt.
  if issues="$(gh api "repos/$REPO/issues?since=$since&state=open&per_page=100" --paginate 2>/dev/null \
      | jq -r --arg since "$since" \
          '.[] | select(.pull_request | not) | select(.created_at >= $since)
               | "#\(.number) \(.title) \(.html_url)"')"; then
    if [[ -n "$issues" ]]; then
      printf '%s\n' "$issues"
    fi
    # Advance the cursor only after a successful poll so a failed call
    # never skips the window it would have covered.
    printf '%s\n' "$next" > "$STATE_FILE"
  else
    log "gh api call failed; keeping cursor at $since and retrying next tick"
  fi

  sleep "$POLL_INTERVAL"
done
