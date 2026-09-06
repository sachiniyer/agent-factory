#!/usr/bin/env bash
# Shared policy for detached sandboxes. Durations are positive integer seconds.
# Keep the default/validation in sync with doctor/playtest_sandboxes.go.
playtest_lifetime() {
    local value="${1:-21600}"
    if [[ ! "$value" =~ ^[1-9][0-9]{0,9}$ ]] || ((value > 2147483647)); then
        echo "testbox: AF_PLAYTEST_MAX_LIFETIME must be 1..2147483647 seconds" >&2
        return 2
    fi
    printf '%s\n' "$value"
}

# Wrap setup as well as hold: a wedged build must not escape the deadline.
# timeout terminates the process group, then forces exit if TERM is ignored.
playtest_with_deadline() {
    local lifetime
    lifetime="$(playtest_lifetime "${AF_PLAYTEST_MAX_LIFETIME:-}")" || return
    exec timeout --signal=TERM --kill-after=5 "$lifetime" "$@"
}

playtest_started_epoch() {
    local started="$1" utc
    [[ "$started" != 0001-* ]] || return 1
    [[ "$started" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$ ]] || return 1
    if date -u -d "$started" +%s 2>/dev/null; then return; fi
    # BSD date: Docker emits UTC; strip fractional seconds when present.
    [[ "$started" == *Z ]] || return 1
    utc="${started%Z}"
    date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "${utc%%.*}Z" +%s 2>/dev/null
}

# Inspect is rechecked before removal, not just trusted from the ps filters.
# Unknown timestamps/policy fail closed. Use the container's lifetime, not a
# later caller's override, so a deliberately longer run survives later starts.
playtest_is_expired() {
    local inspected="$1" now="$2" name label started mode command lifetime epoch header
    header="${inspected%%$'\n'*}"
    IFS='|' read -r name label started mode command <<<"$header"
    [[ "$name" == /af-playtest-* && "$label" == testbox ]] || return 1
    # Interactive sandboxes have no deadline. Legacy launches lacked a mode
    # label, so identify only their exact detached entrypoint command.
    case "$mode" in
    detached) ;;
    '') [[ "$command" == '["bash","/src/scripts/container/playtest-entry.sh","hold"]' ]] || return 1 ;;
    *) return 1 ;;
    esac
    lifetime="$(printf '%s\n' "$inspected" | sed -n 's/^AF_PLAYTEST_MAX_LIFETIME=//p' | tail -n 1)"
    lifetime="$(playtest_lifetime "$lifetime")" || return 1
    epoch="$(playtest_started_epoch "$started")" || return 1
    ((epoch > 0 && now >= epoch && now - epoch >= lifetime))
}

reap_playtest_sandboxes() {
    local ids id inspected now
    ids="$("$ENGINE" ps -aq --filter label=af.harness=testbox --filter name=af-playtest- 2>/dev/null)" || return 0
    now="$(date -u +%s)"
    for id in $ids; do
        inspected="$("$ENGINE" inspect -f '{{.Name}}|{{index .Config.Labels "af.harness"}}|{{.State.StartedAt}}|{{index .Config.Labels "af.playtest.mode"}}|{{json .Config.Cmd}}{{println}}{{range .Config.Env}}{{println .}}{{end}}' "$id" 2>/dev/null)" || continue
        if playtest_is_expired "$inspected" "$now"; then
            echo "testbox: reaping expired sandbox ${inspected%%$'\n'*}" >&2
            "$ENGINE" rm -f "$id" >/dev/null 2>&1 || true
        fi
    done
}
