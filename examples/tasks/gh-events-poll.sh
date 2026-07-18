#!/usr/bin/env bash
# Watch-task skeleton: poll SEVERAL GitHub event sources at once and emit one
# event per new issue, pull request, or issue comment.
#
# gh-issue-poll.sh is the single-source version of this. Watching more than one
# source is where the interesting failure lives, so this script is built around
# one rule: EVERY SOURCE IS INDEPENDENT. Each keeps its own cursor and its own
# seen-set, so a source whose API call is failing cannot stop the others from
# making progress — and cannot make them repeat themselves either.
#
#   af tasks add --name "gh-events" \
#     --watch-cmd "REPO=owner/name $PWD/gh-events-poll.sh" \
#     --prompt "Handle this GitHub event: {{line}}" \
#     --target-session triage
#
# Contract reminders (see docs/tasks.md):
#   - stdout = events, one per newline-terminated line. Log to stderr only.
#   - stderr lands in ~/.agent-factory/logs/task-$AF_TASK_ID.log.
#   - Events are rate-limited to 10/min per task; this poller stays far below.
#   - The daemon does not replay events across restarts — the cursor files are
#     what make this script resumable.
#
# Requires: gh (authenticated), jq.
#
# Env:
#   REPO            owner/name of the repo to watch (required)
#   POLL_INTERVAL   seconds between polls (default: 60)
#   STATE_DIR       cursor + seen-set directory
#                   (default: ~/.agent-factory/gh-events-poll-<task id>)
#   SOURCES         space-separated subset of "issues prs comments"
#                   (default: all three)
#   POLL_CYCLES     stop after N cycles instead of running forever (default: 0
#                   = forever). Set it to 1 to run this from cron instead of as
#                   a watch task; the tests use it to run a bounded number of
#                   polls.
#   FAIL_REPORT_AFTER  consecutive failures of ONE source before saying so on
#                   stderr (default: 3). Reported once per outage, not per
#                   cycle.

set -uo pipefail

: "${REPO:?REPO is required (e.g. owner/name)}"
POLL_INTERVAL="${POLL_INTERVAL:-60}"
STATE_DIR="${STATE_DIR:-$HOME/.agent-factory/gh-events-poll-${AF_TASK_ID:-default}}"
SOURCES="${SOURCES:-issues prs comments}"
POLL_CYCLES="${POLL_CYCLES:-0}"
FAIL_REPORT_AFTER="${FAIL_REPORT_AFTER:-3}"

export GH_PAGER=cat PAGER=cat NO_COLOR=1

log() { printf '[gh-events-poll] %s\n' "$*" >&2; }

now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

mkdir -p "$STATE_DIR"

# --- per-source state -------------------------------------------------------
#
# One cursor file and one seen-set file per source. Nothing is shared: that is
# the entire point. A single global "did every call succeed?" flag is what turns
# one broken endpoint into a repo-wide stall.

cursor_file() { printf '%s/%s.cursor' "$STATE_DIR" "$1"; }
seen_file() { printf '%s/%s.seen' "$STATE_DIR" "$1"; }
fails_file() { printf '%s/%s.fails' "$STATE_DIR" "$1"; }

# read_cursor re-reads the cursor FROM DISK every cycle rather than caching it
# at startup. The file is the source of truth, not a seed: an operator who
# repairs a stuck cursor by hand must see it take effect without restarting the
# watcher, and a long-lived process must not drift from its own state.
read_cursor() {
    local f
    f=$(cursor_file "$1")
    if [ -s "$f" ]; then
        cat "$f"
    else
        now_utc | tee "$f"
    fi
}

write_cursor() { printf '%s\n' "$2" >"$(cursor_file "$1")"; }

seen() { grep -qxF "$2" "$(seen_file "$1")" 2>/dev/null; }
mark() { printf '%s\n' "$2" >>"$(seen_file "$1")"; }

# note_failure counts CONSECUTIVE failures for one source and says so once, when
# the count crosses the threshold. A failure that never gets reported is how a
# broken token turns into a monitor that looks alive and sees nothing.
note_failure() {
    local source=$1 f n
    f=$(fails_file "$source")
    n=$(cat "$f" 2>/dev/null || echo 0)
    n=$((n + 1))
    printf '%s\n' "$n" >"$f"
    if [ "$n" -eq "$FAIL_REPORT_AFTER" ]; then
        log "source '$source' has failed $n polls in a row — its cursor is held at $(read_cursor "$source") and it is emitting nothing. Check 'gh auth status'; other sources are unaffected."
    fi
}

note_success() {
    local f n
    f=$(fails_file "$1")
    n=$(cat "$f" 2>/dev/null || echo 0)
    if [ "$n" -ge "$FAIL_REPORT_AFTER" ]; then
        log "source '$1' is answering again after $n failed polls"
    fi
    rm -f "$f"
}

# --- sources ----------------------------------------------------------------
#
# Each fetch_* prints "<id>\t<event text>" lines for everything created since
# the cursor, and returns non-zero if its API call failed. It must NOT print a
# partial result on failure: the caller distinguishes "nothing new" from "could
# not look", and those must never be confused.

fetch_issues() {
    gh issue list --repo "$REPO" --state open --search "created:>=$1" \
        --json number,title --jq '.[] | "i\(.number)\t[ISSUE #\(.number)] \(.title)"' 2>/dev/null
}

fetch_prs() {
    gh pr list --repo "$REPO" --state open --search "created:>=$1" \
        --json number,title --jq '.[] | "p\(.number)\t[PR #\(.number)] \(.title)"' 2>/dev/null
}

fetch_comments() {
    gh api "repos/$REPO/issues/comments?sort=created&direction=desc&since=$1&per_page=20" \
        --jq '.[] | "c\(.id)\t[COMMENT #\(.issue_url|split("/")|last)] \(.user.login): \(.body|gsub("[\n\r]+";" ")|.[0:140])"' 2>/dev/null
}

# poll_source runs one source through one cycle. The cursor advances ONLY on a
# successful call, and only for THIS source.
poll_source() {
    local source=$1 since now out
    since=$(read_cursor "$source")
    now=$(now_utc)

    if ! out=$("fetch_$source" "$since"); then
        # Conservative and CONTAINED: hold this source's cursor so nothing is
        # skipped, and leave every other source alone.
        note_failure "$source"
        return
    fi
    note_success "$source"

    while IFS=$'\t' read -r id text; do
        [ -z "$id" ] && continue
        # The seen-set is the second line of defence, independent of the cursor.
        # A cursor that cannot advance (or one an operator winds backwards) must
        # not turn into the same event over and over.
        seen "$source" "$id" && continue
        mark "$source" "$id"
        printf '%s\n' "$text"
    done <<<"$out"

    write_cursor "$source" "$now"
}

log "task=${AF_TASK_NAME:-?} watching $REPO [$SOURCES] every ${POLL_INTERVAL}s"

cycle=0
while true; do
    for source in $SOURCES; do
        poll_source "$source"
    done

    cycle=$((cycle + 1))
    if [ "$POLL_CYCLES" -gt 0 ] && [ "$cycle" -ge "$POLL_CYCLES" ]; then
        break
    fi
    sleep "$POLL_INTERVAL"
done
