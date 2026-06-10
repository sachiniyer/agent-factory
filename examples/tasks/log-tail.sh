#!/usr/bin/env bash
# Watch-task skeleton: emit one event per matching line appended to a log file.
#
# Register it as a watch task (the daemon keeps it running and turns each
# stdout line into one prompt delivery):
#
#   af tasks add --name "log-errors" \
#     --watch-cmd "LOG_FILE=/var/log/app.log $PWD/log-tail.sh" \
#     --prompt "Investigate this error: {{line}}" \
#     --target-session debugger
#
# Contract reminders (see docs/tasks.md):
#   - stdout = events, one per newline-terminated line. Log to stderr only.
#   - stderr lands in ~/.agent-factory/logs/task-$AF_TASK_ID.log.
#   - Exit 0 means "stop me until re-enabled"; non-zero gets a backoff restart.
#   - SIGTERM is the daemon asking you to shut down; the default handler is fine.
#
# Env:
#   LOG_FILE   file to tail (required)
#   PATTERN    extended regex to match (default: ERROR)

set -euo pipefail

: "${LOG_FILE:?LOG_FILE is required}"
PATTERN="${PATTERN:-ERROR}"

echo "[log-tail] task=${AF_TASK_NAME:-?} watching $LOG_FILE for /$PATTERN/" >&2

# -F follows across rotation/truncation; -n 0 skips history so only lines
# written after the watcher starts become events. --line-buffered keeps grep
# from batching matches — without it, events can sit in grep's stdout buffer
# for a long time on a quiet log. grep exiting (e.g. the pipe breaking) ends
# the pipeline non-zero, so the daemon restarts the watcher with backoff.
tail -F -n 0 -- "$LOG_FILE" | grep --line-buffered -E -- "$PATTERN"
