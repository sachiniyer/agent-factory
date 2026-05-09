#!/usr/bin/env bash
# Poll a GitHub repo for newly opened issues and spawn an Agent Factory
# session for each one.
#
# Env:
#   REPO            owner/name of the repo to watch (required)
#   POLL_INTERVAL   seconds between polls (default: 60)
#   STATE_FILE      path to the cursor file (default: $HOME/.agent-factory/issue-listener-state)
#   LABEL           if set, only spawn for issues carrying this label
#
# Logs go to stderr. Stdout is reserved for future structured output.

set -euo pipefail

: "${REPO:?REPO is required (e.g. owner/name)}"
POLL_INTERVAL="${POLL_INTERVAL:-60}"
STATE_FILE="${STATE_FILE:-$HOME/.agent-factory/issue-listener-state}"
LABEL="${LABEL:-}"

log() { printf '[issue-listener] %s\n' "$*" >&2; }

shutdown() {
  log "shutting down"
  exit 0
}
trap shutdown SIGTERM SIGINT

now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

mkdir -p "$(dirname "$STATE_FILE")"
if [[ ! -f "$STATE_FILE" ]]; then
  now_utc > "$STATE_FILE"
  log "initialized state file at $STATE_FILE; only issues opened after $(cat "$STATE_FILE") will be picked up"
fi

log "watching $REPO every ${POLL_INTERVAL}s${LABEL:+ (label: $LABEL)}"

while true; do
  since="$(cat "$STATE_FILE")"
  next="$(now_utc)"

  # $since is a jq variable; LABEL is read from the env. Quoting is correct.
  # shellcheck disable=SC2016
  jq_filter='.[] | select(.pull_request | not) | select(.created_at >= $since)'
  if [[ -n "$LABEL" ]]; then
    # shellcheck disable=SC2016
    jq_filter="$jq_filter"' | select(.labels | map(.name) | index(env.LABEL))'
  fi
  # shellcheck disable=SC2016
  jq_filter="$jq_filter"' | {number, title, body}'

  if ! issues_json="$(gh api \
      "repos/$REPO/issues?since=$since&state=open&per_page=100" \
      --paginate 2>/dev/null \
    | jq -c --arg since "$since" "$jq_filter")"; then
    log "gh api call failed; will retry next tick"
    sleep "$POLL_INTERVAL"
    continue
  fi

  if [[ -n "$issues_json" ]]; then
    while IFS= read -r issue; do
      [[ -z "$issue" ]] && continue
      number="$(printf '%s' "$issue" | jq -r '.number')"
      title="$(printf '%s' "$issue" | jq -r '.title')"
      body="$(printf '%s' "$issue" | jq -r '.body // ""')"
      session_name="issue-$number"

      if [[ -z "$body" ]]; then
        body='(no issue body — use the title as the spec)'
      fi
      prompt="$(printf '%s\n\n%s' "$title" "$body")"

      log "spawning session $session_name for issue #$number: $title"
      if ! af sessions create --name "$session_name" --prompt "$prompt" >/dev/null 2>&1; then
        log "af sessions create failed for $session_name (already exists?); skipping"
      fi
    done < <(printf '%s\n' "$issues_json")
  fi

  printf '%s\n' "$next" > "$STATE_FILE"
  sleep "$POLL_INTERVAL"
done
