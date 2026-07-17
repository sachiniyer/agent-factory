#!/usr/bin/env bash
# lifecycle-lib.sh — guards, assertions, and daemon introspection for the
# clean-environment install/upgrade gate (scripts/lifecycle.sh).
#
# Split from lifecycle.sh so the scenarios read as scenarios: everything here
# is plumbing that answers ONE question — "what is actually true of this
# machine right now" — without any opinion about what should be true.
#
# Sourced, never executed.

# ----------------------------------------------------------------------------
# Result accounting. Assertions never abort the run: a lifecycle scenario that
# stops at the first failure hides the other five, and the whole point of this
# gate is to report the full state of an upgraded machine in one pass.
# ----------------------------------------------------------------------------
LC_PASS=0
LC_FAIL=0
LC_SKIP=0
LC_FAILED_NAMES=()

lc_say() { printf '[lifecycle] %s\n' "$*" >&2; }
lc_section() { printf '\n[lifecycle] ===== %s =====\n' "$*" >&2; }

lc_pass() {
    LC_PASS=$((LC_PASS + 1))
    printf '[lifecycle]   PASS  %s\n' "$*" >&2
}

lc_fail() {
    LC_FAIL=$((LC_FAIL + 1))
    LC_FAILED_NAMES+=("$1")
    printf '[lifecycle]   FAIL  %s\n' "$*" >&2
}

# lc_skip — a check that could not run here. Loud on purpose: a silently
# skipped assertion reads exactly like a passing one in a green log, and the
# supervision check (#4) is skipped on every container run. Anything that
# skips must say so in the summary too.
lc_skip() {
    LC_SKIP=$((LC_SKIP + 1))
    printf '[lifecycle]   SKIP  %s\n' "$*" >&2
}

lc_assert_eq() {
    local want="$1" got="$2" what="$3"
    if [ "$want" = "$got" ]; then
        lc_pass "$what (= $got)"
    else
        lc_fail "$what: want '$want', got '$got'"
    fi
}

lc_assert_ne() {
    local unwant="$1" got="$2" what="$3"
    if [ "$unwant" != "$got" ]; then
        lc_pass "$what (changed to $got)"
    else
        lc_fail "$what: expected a change, but it is still '$got'"
    fi
}

# lc_summary — the run's verdict.
#
# A SKIP is NOT a pass. A run that skipped the supervision assertion and then
# reported green would manufacture exactly the confidence this gate exists to
# destroy: the one regression we know shipped (an upgrade demoting the daemon
# off its unit, #796) is invisible to every OTHER assertion, because the
# demoted daemon keeps running and keeps answering. So an incomplete run exits
# NON-ZERO unless the operator explicitly acknowledges partial coverage with
# AF_LIFECYCLE_ALLOW_PARTIAL=1 (which `make lifecycle-container` sets, because a
# dev box's container genuinely cannot host a service manager — the CI leg is
# the authority for those checks and fails if they ever stop running).
lc_summary() {
    printf '\n[lifecycle] ===== summary =====\n' >&2
    printf '[lifecycle] %d PASS, %d FAIL, %d SKIP\n' "$LC_PASS" "$LC_FAIL" "$LC_SKIP" >&2

    local rc=0
    if [ "$LC_FAIL" -gt 0 ]; then
        printf '[lifecycle] failed checks:\n' >&2
        local n
        for n in "${LC_FAILED_NAMES[@]}"; do printf '[lifecycle]   - %s\n' "$n" >&2; done
        rc=1
    fi

    if [ "$LC_SKIP" -gt 0 ]; then
        printf '[lifecycle]\n' >&2
        printf '[lifecycle] *** PARTIAL COVERAGE: %d check(s) did NOT run. ***\n' "$LC_SKIP" >&2
        printf '[lifecycle] This run proves nothing about them. Supervision-across-upgrade\n' >&2
        printf '[lifecycle] (#796) is invisible to every other assertion here: a demoted\n' >&2
        printf '[lifecycle] daemon still runs and still answers.\n' >&2
        if [ "${AF_LIFECYCLE_ALLOW_PARTIAL:-}" = "1" ]; then
            printf '[lifecycle] AF_LIFECYCLE_ALLOW_PARTIAL=1 — not failing the run, but this is\n' >&2
            printf '[lifecycle] NOT full coverage. The CI leg (real systemd) is the authority.\n' >&2
        else
            printf '[lifecycle] Failing the run: a skipped check must never read as a pass.\n' >&2
            rc=1
        fi
    fi
    return "$rc"
}

# ----------------------------------------------------------------------------
# The isolation guard.
#
# This harness is destructive BY DESIGN: it installs binaries, registers an
# autostart unit with the user's service manager, upgrades af over itself, and
# stops daemons. Every one of those is a catastrophe against a real machine —
# so the guard's default answer is NO, and it takes both an explicit opt-in and
# a disposable-looking environment to get a yes.
#
# Detection is positive, never "not obviously a dev box": a container has
# /.dockerenv, a CI runner sets CI=true. A shared dev box has neither, so it
# refuses even if someone exports the opt-in by accident.
# ----------------------------------------------------------------------------
lc_guard_disposable() {
    if [ "${AF_LIFECYCLE_DISPOSABLE:-}" != "1" ]; then
        lc_say "REFUSING: this harness installs binaries, registers autostart units,"
        lc_say "upgrades af over itself and stops daemons. It only ever runs in a"
        lc_say "throwaway environment. Set AF_LIFECYCLE_DISPOSABLE=1 to confirm."
        lc_say "On a dev box use: make lifecycle-container"
        return 1
    fi

    local disposable=no reason=""
    if [ -f /.dockerenv ]; then
        disposable=yes
        reason="container (/.dockerenv)"
    elif [ "${CI:-}" = "true" ]; then
        disposable=yes
        reason="CI runner (CI=true)"
    fi
    if [ "$disposable" != yes ]; then
        lc_say "REFUSING: no disposable environment detected (no /.dockerenv, CI!=true)."
        lc_say "This looks like a real machine. Use: make lifecycle-container"
        return 1
    fi

    # A disposable environment is necessary but not sufficient: inside a
    # container someone can still bind-mount a real home in and point the
    # workspace at it. Every AF home this harness uses is built under
    # $LC_WORKSPACE, so validating the workspace covers all of them.
    if [ -z "${LC_WORKSPACE:-}" ]; then
        lc_say "REFUSING: LC_WORKSPACE is unset."
        return 1
    fi
    case "$LC_WORKSPACE" in
    "$HOME/.agent-factory" | "$HOME/.agent-factory"/* | "$HOME" | /)
        lc_say "REFUSING: workspace '$LC_WORKSPACE' is (or is inside) the real AF home."
        return 1
        ;;
    esac
    if [ -n "${AGENT_FACTORY_HOME:-}" ] && [ "$AGENT_FACTORY_HOME" = "$HOME/.agent-factory" ]; then
        lc_say "REFUSING: AGENT_FACTORY_HOME is the real default home."
        return 1
    fi

    lc_say "isolation: $reason; workspace=$LC_WORKSPACE"
    return 0
}

# ----------------------------------------------------------------------------
# Daemon introspection.
#
# Everything below reads the machine directly rather than asking af, because
# these are the assertions that must still hold when af is the thing that is
# broken. Where af DOES have an answer (daemon status --json), we use it — but
# never as the only witness for "is there exactly one daemon".
# ----------------------------------------------------------------------------

# lc_assert_virgin <home> <what> — prove this home is untouched, RIGHT BEFORE the
# probe that depends on it.
#
# Exists because an observation that CREATES the state it observes is not an
# observation. `af doctor` and `af daemon status` both materialize the home
# directory and agent-factory.log (measured; `af version` touches nothing) — so a
# scenario that probes doctor and then claims to boot the TUI "on a virgin home"
# is measuring a home its own earlier probe built. Asserting virginity once at
# the top of a scenario only ever covers the FIRST probe.
#
# Each probe therefore gets its own pristine home and calls this immediately
# before touching it, which holds regardless of which command materializes what —
# including on the far side of an upgrade, where the answer may differ.
lc_assert_virgin() {
    local home="$1" what="$2" dirty=""
    [ -e "$home" ] && dirty="home directory exists"
    [ -e "$home/config.toml" ] && dirty="${dirty:+$dirty; }config.toml exists"
    [ -e "$home/state.json" ] && dirty="${dirty:+$dirty; }state.json exists"
    if [ -n "$dirty" ]; then
        lc_fail "$what: home '$home' is NOT virgin ($dirty) — this probe's premise is already broken"
        find "$home" -maxdepth 1 -mindepth 1 2>/dev/null | sed 's/^/[lifecycle]   | /' >&2
        return 1
    fi
    local n
    n="$(lc_daemon_count "$home")"
    if [ "$n" != "0" ]; then
        lc_fail "$what: $n daemon(s) already serve '$home'"
        return 1
    fi
    lc_pass "$what: home is virgin (no dir, no config, no state, no daemon)"
    return 0
}

# lc_daemon_pids <home> — pids of af daemons serving exactly this AF home.
#
# Three filters, each closing a false positive we actually hit while building
# this harness:
#   * argv[1] == "--daemon" exactly, and argv[0]'s basename is an af binary —
#     `pgrep -f "af --daemon"` matches the harness's own shell command line,
#     because that string appears in it.
#   * environ AGENT_FACTORY_HOME == our home — a global scan otherwise adopts
#     daemons from other homes (and, on a shared box, other users' daemons).
#     /proc/<pid>/environ is owner-only, so another user's daemon is invisible
#     here rather than miscounted — the same rule af's own scan learned in
#     #1920.
lc_daemon_pids() {
    local home="$1" d pid argv0 argv1 dhome
    for d in /proc/[0-9]*; do
        [ -r "$d/cmdline" ] || continue
        argv1=$(tr '\0' '\n' <"$d/cmdline" 2>/dev/null | sed -n '2p')
        [ "$argv1" = "--daemon" ] || continue
        argv0=$(tr '\0' '\n' <"$d/cmdline" 2>/dev/null | sed -n '1p')
        case "$(basename "${argv0:-}")" in
        af | agent-factory) ;;
        *) continue ;;
        esac
        dhome=$(tr '\0' '\n' <"$d/environ" 2>/dev/null | sed -n 's/^AGENT_FACTORY_HOME=//p' | head -1)
        [ "$dhome" = "$home" ] || continue
        pid=${d#/proc/}
        printf '%s\n' "$pid"
    done
}

lc_daemon_count() { lc_daemon_pids "$1" | wc -l | tr -d ' '; }

# lc_daemon_version <pid> — the version of the image the daemon is ACTUALLY
# running, by copying /proc/<pid>/exe and asking it.
#
# This is a real query, not an inference from the binary on disk — which is the
# whole point: after an upgrade the on-disk binary is N while a daemon that was
# never restarted is still executing the N-1 image, and /proc/<pid>/exe still
# resolves to those bytes even though the path now reads "(deleted)".
#
# Linux-only (needs /proc). macOS has no equivalent, which is one reason the
# macOS leg is not wired yet — see the header of scripts/lifecycle.sh.
#
# SWAP POINT (#1920): once `af doctor --json` ships a `daemon version` check,
# this can become a plain query of the daemon's own reported version. Until
# then this is the only way to learn a running daemon's version.
lc_daemon_version() {
    local pid="$1" tmp out
    tmp="$(mktemp "${TMPDIR:-/tmp}/lc-daemon-image.XXXXXX")" || return 1
    if ! cp "/proc/$pid/exe" "$tmp" 2>/dev/null; then
        rm -f "$tmp"
        return 1
    fi
    chmod +x "$tmp" 2>/dev/null || true
    out=$("$tmp" version 2>/dev/null | sed -n 's/^agent-factory version //p' | head -1)
    rm -f "$tmp"
    [ -n "$out" ] || return 1
    printf '%s\n' "$out"
}

# lc_client_version <bin> — the version the installed binary reports.
lc_client_version() {
    "$1" version 2>/dev/null | sed -n 's/^agent-factory version //p' | head -1
}

# lc_status_field <bin> <jq-path> — a field from `af daemon status --json`,
# which is wrapped in the shared {data,error} envelope.
lc_status_field() {
    local bin="$1" path="$2"
    "$bin" daemon status --json 2>/dev/null | jq -r "$path" 2>/dev/null
}

# lc_doctor_fail_count <bin> — how many checks doctor reports as FAIL.
#
# Feature-detects `--json` so this auto-upgrades the moment #1920 lands: today
# it parses the "Summary: 9 PASS, 7 WARN, 0 FAIL" line, and the instant doctor
# grows a --json flag it reads the structured summary instead. No follow-up
# edit needed here.
lc_doctor_fail_count() {
    local bin="$1" out n
    if "$bin" doctor --help 2>&1 | grep -q -- '--json'; then
        out=$("$bin" doctor --json 2>/dev/null)
        # #1920's envelope: {data:{summary:{fail:N}}}. Fall through to the text
        # parser if the shape is not what we expect rather than silently
        # reporting 0 FAILs — a wrong 0 here would make the whole gate a no-op.
        n=$(printf '%s' "$out" | jq -r '.data.summary.fail // empty' 2>/dev/null)
        if [ -n "$n" ]; then
            printf '%s\n' "$n"
            return 0
        fi
    fi
    out=$("$bin" doctor 2>&1)
    n=$(printf '%s' "$out" | sed -n 's/.*Summary:.*[, ]\([0-9]\{1,\}\) FAIL.*/\1/p' | head -1)
    [ -n "$n" ] || return 1
    printf '%s\n' "$n"
}

# lc_doctor_dump <bin> — doctor's FAIL lines, for the log when a check trips.
lc_doctor_dump() {
    "$1" doctor 2>&1 | grep -E '^\s*(FAIL|Summary)' || true
}

# ----------------------------------------------------------------------------
# Service manager (assertion #4).
# ----------------------------------------------------------------------------
LC_UNIT_NAME="agent-factory-daemon.service"
# launchd's equivalent (daemon/autostart.go: autostartLaunchdLabel). af restarts
# it as gui/<uid>/<label>, so that is the domain to interrogate — a launchd
# agent loaded OUTSIDE that domain is one of the ways supervision silently
# stops being real (#1920 checks the same thing).
LC_LAUNCHD_LABEL="com.agent-factory.daemon"

# lc_supervisor_available — can this environment actually supervise a daemon?
#
# The test container cannot: it has no systemd (PID 1 is docker-init), so
# `af daemon install` fails outright with "systemctl: executable file not
# found". That is why the supervision assertion is SKIPPED on container runs
# and only really runs on the CI runner, which has a real systemd user manager.
lc_supervisor_available() {
    case "$(uname -s)" in
    Linux)
        command -v systemctl >/dev/null 2>&1 || return 1
        systemctl --user show-environment >/dev/null 2>&1 || return 1
        return 0
        ;;
    Darwin)
        command -v launchctl >/dev/null 2>&1 || return 1
        return 0
        ;;
    *) return 1 ;;
    esac
}

# lc_unit_active — is the autostart unit currently supervising anything?
# Prints "active" when it is, anything else when it is not, on BOTH platforms —
# so assertion #4 asserts one property, not one per OS.
lc_unit_active() {
    case "$(uname -s)" in
    Linux)
        systemctl --user is-active "$LC_UNIT_NAME" 2>/dev/null
        ;;
    Darwin)
        # `launchctl print` succeeding is NOT "running" — it prints happily for a
        # loaded-but-stopped agent. The state field is the answer (the same trap
        # #1920 hit: split Loaded from Active).
        local out
        out="$(launchctl print "gui/$(id -u)/$LC_LAUNCHD_LABEL" 2>/dev/null)" || {
            printf 'inactive\n'
            return 0
        }
        if printf '%s' "$out" | grep -qE '^[[:space:]]*state = running'; then
            printf 'active\n'
        else
            printf 'inactive\n'
        fi
        ;;
    *) printf 'unsupported\n' ;;
    esac
}

# lc_unit_main_pid — the pid systemd believes it supervises, or 0.
#
# This is the assertion that catches the demotion: when respawnDaemonAfterUpgrade
# falls back to an ad-hoc child, a daemon is still running and still answers
# pings — but it is nobody's child, and the unit's MainPID no longer matches it
# (typically 0, with the unit inactive/dead). Comparing "the daemon that is
# running" against "the daemon systemd owns" is the only way to tell a
# supervised daemon from an orphan that happens to work.
lc_unit_main_pid() {
    case "$(uname -s)" in
    Linux)
        systemctl --user show -p MainPID --value "$LC_UNIT_NAME" 2>/dev/null | tr -d ' '
        ;;
    Darwin)
        # launchctl print emits "pid = 1234" for a running job; absent when the
        # job is loaded but not running — which is precisely the demoted case.
        launchctl print "gui/$(id -u)/$LC_LAUNCHD_LABEL" 2>/dev/null |
            sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\).*/\1/p' | head -1
        ;;
    *) printf '' ;;
    esac
}
