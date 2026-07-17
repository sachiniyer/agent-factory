#!/usr/bin/env bash
# lifecycle-selftest.sh — tests for the lifecycle harness's OWN safety logic.
#
# The gate in scripts/lifecycle.sh proves things about af. This proves things
# about the gate, and it is the cheaper half: every case here is pure logic —
# no containers, no daemons, no network, no AF home — so it runs on the host in
# about a second and can gate every PR.
#
# It exists because the harness's failure mode is not "red when it should be
# green", it is "GREEN WHEN IT PROVED NOTHING": a fault injection that never
# executes, a disposable-environment guard that says yes to a real machine, two
# concurrent runs sharing an image tag. Each of those turns a gate into a
# rubber stamp, and none of them is visible in a passing run.
#
# Every case here was watched failing against the pre-fix code before the fix
# landed. See docs/lifecycle-testing.md.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0

ok() {
    PASS=$((PASS + 1))
    printf '  PASS  %s\n' "$*"
}
no() {
    FAIL=$((FAIL + 1))
    printf '  FAIL  %s\n' "$*"
}

# Source the library in a subshell-safe way: it only defines functions and
# counters, and touches nothing outside the process.
LC_WORKSPACE="/tmp/lc-selftest-never-created"
# shellcheck source=scripts/lifecycle-lib.sh
. "$HERE/lifecycle-lib.sh"

printf '\n=== fault injections that cannot execute must FAIL the harness ===\n'

# An unregistered name matches no branch, so nothing is injected and every
# assertion passes — a green check for an experiment that never happened.
if lc_validate_injection "skip-restart" 2>/dev/null; then
    no "an unknown injection name was accepted (it would inject nothing and pass)"
else
    ok "unknown injection name is rejected before anything runs"
fi
if lc_validate_injection "skip-daemon-restart" 2>/dev/null; then
    ok "a registered injection name is accepted"
else
    no "a registered injection name was rejected"
fi
if lc_validate_injection "" 2>/dev/null; then
    ok "no injection requested is fine (the normal green path)"
else
    no "an empty injection was treated as an error"
fi

# The other half: a VALID injection that never reaches its branch — e.g.
# requested for scenario-b while only scenario-a runs.
LC_INJECT_APPLIED=0
LC_FAIL=0
if lc_assert_injection_ran "skip-daemon-restart" 2>/dev/null; then
    no "a requested-but-never-applied injection was reported as ran"
else
    ok "a requested injection that never executed FAILS the harness"
fi
LC_INJECT_APPLIED=0
LC_FAIL=0
lc_note_injection_applied
if lc_assert_injection_ran "skip-daemon-restart" 2>/dev/null; then
    ok "an injection that DID apply is accepted"
else
    no "an applied injection was reported as never run"
fi
LC_INJECT_APPLIED=0
LC_FAIL=0
if lc_assert_injection_ran "" 2>/dev/null; then
    ok "no injection requested => nothing to have run"
else
    no "the no-injection case was treated as a missing injection"
fi

printf '\n=== the disposable guard defaults to NO ===\n'

# These drive the REAL lc_detect_disposable by pointing its markers at temp
# paths. An earlier version of this test re-implemented the detection and
# grepped the source for "/run/.containerenv" — and PASSED against code with
# podman detection deleted, because that string also lives in the guard's error
# message. Watched failing is the only reason we know this one works.
probe="$(mktemp -d)"
trap 'rm -rf "$probe"' EXIT
: >"$probe/dockerenv"
: >"$probe/containerenv"

detect() { # $1=docker-marker-path $2=podman-marker-path $3=CI
    # shellcheck disable=SC2030,SC2031  # the subshell IS the isolation: these
    # overrides must not leak into the next case.
    (
        LC_MARKER_DOCKER="$1" LC_MARKER_PODMAN="$2" CI="$3"
        export LC_MARKER_DOCKER LC_MARKER_PODMAN CI
        if lc_detect_disposable >/dev/null 2>&1; then echo yes; else echo no; fi
    )
}

none="$probe/absent"
if [ "$(detect "$none" "$none" '')" = no ]; then
    ok "an unrecognized runtime (no marker at all) is NOT disposable — refuses"
else
    no "an unrecognized runtime was treated as disposable"
fi
if [ "$(detect "$probe/dockerenv" "$none" '')" = yes ]; then
    ok "docker (/.dockerenv) is recognized"
else
    no "docker was not recognized"
fi
if [ "$(detect "$none" "$probe/containerenv" '')" = yes ]; then
    ok "podman (/run/.containerenv) is recognized"
else
    no "podman was not recognized"
fi
if [ "$(detect "$none" "$none" true)" = yes ]; then
    ok "CI=true is recognized"
else
    no "CI was not recognized"
fi
# The real marker paths must be the real ones, not left pointing at a test
# fixture. Read from a clean process so the subshells above cannot colour it.
# shellcheck disable=SC2031
real_markers="$(bash -c ". '$HERE/lifecycle-lib.sh'; printf '%s %s' \"\$LC_MARKER_DOCKER\" \"\$LC_MARKER_PODMAN\"")"
if [ "$real_markers" = "/.dockerenv /run/.containerenv" ]; then
    ok "the production marker paths are the real ones"
else
    no "production marker paths are wrong ($real_markers)"
fi
# And the guard must require the explicit opt-in before any of that matters.
if printf '%s' "$(declare -f lc_guard_disposable)" | grep -q 'AF_LIFECYCLE_DISPOSABLE'; then
    ok "lc_guard_disposable requires the explicit AF_LIFECYCLE_DISPOSABLE opt-in"
else
    no "lc_guard_disposable does not require the opt-in"
fi

printf '\n=== concurrent invocations must not share an image tag ===\n'

# Two invocations of the tag-minting logic must differ. _uniq lives in
# testbox.sh, which runs a `case` at source time, so pull the two lines out
# rather than sourcing the whole script.
tag_of() { # emulate testbox.sh's per-run tag
    bash -c '
        _uniq() {
            local r=""
            [ -r /dev/urandom ] && r="$(od -An -N3 -tx1 /dev/urandom 2>/dev/null | tr -d " ")"
            printf "%s" "$$${r:+-$r}"
        }
        printf "agent-factory-lifecycle:%s\n" "$(_uniq)"
    '
}
t1="$(tag_of)"
t2="$(tag_of)"
if [ -n "$t1" ] && [ -n "$t2" ] && [ "$t1" != "$t2" ]; then
    ok "two invocations mint different image tags ($t1 != $t2)"
else
    no "two invocations share an image tag ($t1 == $t2) — concurrent runs would clobber (#1171)"
fi

# The real script must not hardcode a fixed default tag: that is the #1171 bug.
# shellcheck disable=SC2016  # matching literal shell source, expansion is wrong here
if grep -q 'LIFECYCLE_IMAGE="\${AF_LIFECYCLE_IMAGE:-}"' "$HERE/testbox.sh"; then
    ok "testbox.sh mints the lifecycle tag per run (no fixed default)"
else
    no "testbox.sh has a FIXED default lifecycle image tag — concurrent runs clobber (#1171)"
fi
# shellcheck disable=SC2016  # matching literal shell source
if grep -q 'agent-factory-lifecycle:\$lc_token' "$HERE/testbox.sh"; then
    ok "the per-run tag carries the unique token"
else
    no "the lifecycle tag does not carry a per-run token"
fi

printf '\n=== summary: %d PASS, %d FAIL ===\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
