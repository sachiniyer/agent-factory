#!/usr/bin/env bash
# Lifetime contract for throwaway sandboxes (#6844). Source this file for its
# functions, or run it directly:
#
#   bash scripts/container/sandbox-lifetime.sh mark <af-home> <sandbox-root> <kind>
#
# The marker file — throwaway-sandbox.json at the top of the AF home — is the
# ONLY identity a throwaway sandbox has: a home without a valid marker is
# never touched. A daemon serving a marked home enforces the recorded
# deadline (and the declared owner's liveness) itself, and registers the home
# so the next marked daemon start reaps sandboxes that are expired or whose
# creator is gone. Keep this schema in sync with internal/throwawaysandbox.
#
# The sandbox root's creation is proven by a sentinel the SCAFFOLD plants
# when it creates the dedicated root — .af-throwaway-sandbox, holding the
# creation stamp. mark never writes it: a root the scaffold merely names
# could be a shared directory, and manufacturing the sentinel there would
# forge the very proof the reaper trusts. mark instead ADOPTS a planted
# sentinel's stamp as the marker's created_unix, so the two always agree.
# The create-root subcommand below is how a scaffold plants correctly.
#
# Environment:
#   AF_SANDBOX_MAX_LIFETIME   seconds; preferred. Falls back to
#                             AF_PLAYTEST_MAX_LIFETIME, then the shared
#                             six-hour default.
#   AF_SANDBOX_OWNER_PID      optional: the creating process's pid. When set,
#                             the marker records as much of its process
#                             identity as the platform offers (start stamp,
#                             boot id, pid namespace) so a later start can
#                             prove the owner is gone. An unreadable identity
#                             degrades to a pid-only record — never a
#                             fabricated one. The owner must be a DURABLE
#                             process — one that lives as long as the sandbox
#                             should, such as the driver pane or a dedicated
#                             keeper pid. "$$" inside a one-shot shell dies
#                             with the call and trips the owner-death
#                             watchdog immediately.

throwaway_sandbox_lifetime() {
    local value="${1:-21600}"
    if [[ ! "$value" =~ ^[1-9][0-9]{0,9}$ ]] || ((value > 2147483647)); then
        echo "sandbox-lifetime: sandbox lifetime must be 1..2147483647 seconds" >&2
        return 2
    fi
    printf '%s\n' "$value"
}

throwaway_sandbox_json_quote() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    value="${value//$'\b'/\\b}"
    value="${value//$'\f'/\\f}"
    value="${value//$'\n'/\\n}"
    value="${value//$'\r'/\\r}"
    value="${value//$'\t'/\\t}"
    printf '"%s"' "$value"
}

# Emit the marker's owner_* JSON fragments for $1 (a pid), best-effort. Each
# fragment ends with a comma because it is only emitted between fields.
throwaway_sandbox_owner_identity() {
    local pid="$1"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 0
    printf '  "owner_pid": %s,\n' "$pid"
    if [[ -r "/proc/$pid/stat" ]]; then
        # comm sits in parens and may itself contain ')'; everything after
        # the LAST ') ' is numeric, and starttime is field 20 of that tail.
        local stat_tail start_id
        stat_tail="$(sed 's/.*) //' "/proc/$pid/stat" 2>/dev/null)"
        start_id="$(awk '{print $20}' <<<"$stat_tail" 2>/dev/null)"
        [[ "$start_id" =~ ^[0-9]+$ ]] && printf '  "owner_start_id": %s,\n' "$start_id"
    fi
    if [[ -r /proc/sys/kernel/random/boot_id ]]; then
        local boot_id
        boot_id="$(tr -d '[:space:]' </proc/sys/kernel/random/boot_id)"
        [[ -n "$boot_id" ]] &&
            printf '  "owner_boot_id": %s,\n' "$(throwaway_sandbox_json_quote "$boot_id")"
    fi
    if [[ -e "/proc/$pid/ns/pid" ]]; then
        local dev ino
        read -r dev ino < <(stat -Lc '%D %i' "/proc/$pid/ns/pid" 2>/dev/null)
        [[ "$dev" =~ ^[0-9a-fA-F]+$ && "$ino" =~ ^[0-9]+$ ]] &&
            printf '  "owner_ns_id": "pidns:%s:%x",\n' "$dev" "$ino"
    fi
}

# throwaway_sandbox_create_root ROOT — creates a DEDICATED sandbox root and
# plants the creation sentinel the reaper trusts. Refuses an existing path:
# stamping a pre-existing directory would forge "the sandbox made this" on
# whatever happens to live there — shared dirs included. Prints the stamp
# on success; callers need not record it — mark reads the sentinel itself.
throwaway_sandbox_create_root() {
    local root="$1" stamp
    if [[ -e "$root" ]]; then
        echo "sandbox-lifetime: refusing to plant a creation sentinel in existing path $root" >&2
        return 1
    fi
    mkdir -p "$root" || return
    stamp="$(date +%s)"
    printf '%s\n' "$stamp" >"$root/.af-throwaway-sandbox" || return
    printf '%s\n' "$stamp"
}

# throwaway_sandbox_abspath PATH — renders PATH absolute. A recorded path
# is resolved later in the DAEMON's frame, not the caller's, so a relative
# spelling here would point somewhere else entirely (codex review #6846).
throwaway_sandbox_abspath() {
    local p="$1"
    if [[ "$p" != /* ]]; then
        if [[ -d "$p" ]]; then
            (cd "$p" && pwd -P)
            return
        fi
        p="$(pwd -P)/${p#./}"
    fi
    # Clean any dot/.. remnants without requiring the path to exist.
    realpath -m -- "$p" 2>/dev/null || printf '%s\n' "$p"
}

# throwaway_sandbox_mark HOME ROOT KIND — writes HOME/throwaway-sandbox.json
# atomically. Returns non-zero without leaving a partial marker.
throwaway_sandbox_mark() {
    local home="$1" root="$2" kind="$3"
    local lifetime created
    lifetime="$(throwaway_sandbox_lifetime \
        "${AF_SANDBOX_MAX_LIFETIME:-${AF_PLAYTEST_MAX_LIFETIME:-}}")" || return
    [[ -n "$root" ]] && root="$(throwaway_sandbox_abspath "$root")"
    created="$(date +%s)"
    # A scaffold-planted sentinel means this root was created for THIS
    # sandbox: adopt its stamp as created_unix so the marker and the
    # sentinel provably name the same instance. A root without one stays
    # recorded-but-unproven — the reaper refuses its tmux metadata.
    if [[ -n "$root" && -f "$root/.af-throwaway-sandbox" ]]; then
        local stamp
        stamp="$(tr -d '[:space:]' <"$root/.af-throwaway-sandbox" 2>/dev/null)"
        [[ "$stamp" =~ ^[0-9]+$ ]] && created="$stamp"
    fi
    mkdir -p "$home" || return
    local tmp="$home/.throwaway-sandbox.json.tmp"
    if ! {
        printf '{\n'
        printf '  "version": 1,\n'
        printf '  "kind": %s,\n' "$(throwaway_sandbox_json_quote "$kind")"
        printf '  "created_unix": %s,\n' "$created"
        printf '  "max_lifetime_seconds": %s,\n' "$lifetime"
        if [[ -n "${AF_SANDBOX_OWNER_PID:-}" ]]; then
            throwaway_sandbox_owner_identity "$AF_SANDBOX_OWNER_PID"
        fi
        if [[ -n "${TMUX_TMPDIR:-}" ]]; then
            printf '  "tmux_tmpdir": %s,\n' \
                "$(throwaway_sandbox_json_quote "$(throwaway_sandbox_abspath "$TMUX_TMPDIR")")"
        fi
        printf '  "sandbox_root": %s\n' "$(throwaway_sandbox_json_quote "$root")"
        printf '}\n'
    } >"$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    mv -f "$tmp" "$home/throwaway-sandbox.json"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    set -euo pipefail
    case "${1:-}" in
    mark)
        shift
        if [[ $# -ne 3 ]]; then
            echo "usage: $0 mark <af-home> <sandbox-root> <kind>" >&2
            exit 2
        fi
        throwaway_sandbox_mark "$@"
        ;;
    create-root)
        shift
        if [[ $# -ne 1 ]]; then
            echo "usage: $0 create-root <sandbox-root>" >&2
            exit 2
        fi
        throwaway_sandbox_create_root "$@"
        ;;
    *)
        echo "usage: $0 mark <af-home> <sandbox-root> <kind> | create-root <sandbox-root>" >&2
        exit 2
        ;;
    esac
fi
