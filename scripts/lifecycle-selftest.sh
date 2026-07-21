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
SELFTEST_TMP="$(mktemp -d)"
trap 'rm -rf "$SELFTEST_TMP"' EXIT

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
AF_LIFECYCLE_WORKSPACE="$SELFTEST_TMP/never-created"
export AF_LIFECYCLE_WORKSPACE
# shellcheck source=scripts/lifecycle-lib.sh
. "$HERE/lifecycle-lib.sh"
# Source the production scenario functions without executing main. This makes
# the selftest drive the same upgrade classifier the real lifecycle gate calls.
# shellcheck source=scripts/lifecycle.sh
. "$HERE/lifecycle.sh"

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

printf '\n=== external release availability is not a product failure ===\n'

# The authenticated release-list probe must carry an unavailable result as a
# distinct status. Returning ordinary failure here makes scenario_b record a
# product FAIL for GitHub's shared-runner quota (#2262).
curl() {
    printf '{"message":"API rate limit exceeded"}\n403'
}
release_rc=0
lc_two_newest_stable >/dev/null 2>&1 || release_rc=$?
unset -f curl
if [ "$release_rc" = "2" ]; then
    ok "an API 403 is classified as could-not-verify"
else
    no "an API 403 returned $release_rc, want could-not-verify status 2"
fi

# Drive scenario_b itself, not only its helper: it must consume status 2 as one
# explicit SKIP and stop before any install/daemon work. A malformed release
# set (status 1) remains a product/release failure and must never be pardoned.
if (
    LC_PASS=0
    LC_FAIL=0
    LC_SKIP=0
    lc_mock_repo() { return 0; }
    lc_two_newest_stable() { return 2; }
    scenario_b upgrade-cmd >/dev/null 2>&1
    [ "$LC_SKIP" = 1 ] && [ "$LC_FAIL" = 0 ]
); then
    ok "scenario B records release API unavailability as SKIP"
else
    no "scenario B did not preserve the could-not-verify outcome"
fi
if (
    LC_PASS=0
    LC_FAIL=0
    LC_SKIP=0
    lc_mock_repo() { return 0; }
    lc_two_newest_stable() { return 1; }
    scenario_b upgrade-cmd >/dev/null 2>&1
); then
    no "scenario B pardoned a malformed/unusable release set"
else
    ok "scenario B keeps malformed/unusable release data red"
fi

# The published N-1 binary performs its own lookup. Until every supported N-1
# knows how to consume GITHUB_TOKEN, its exact quota error must get the same
# distinct result instead of becoming "the upgrade step itself failed".
fake_af="$SELFTEST_TMP/af-403"
printf '%s\n' '#!/usr/bin/env bash' \
    'printf "%s\\n" "Error: failed to fetch latest release: GitHub API returned 403" >&2' \
    'exit 1' >"$fake_af"
chmod +x "$fake_af"
upgrade_rc=0
lc_do_upgrade upgrade-cmd "$fake_af" unused-home unused-repo v1.0.2 >/dev/null 2>&1 || upgrade_rc=$?
if [ "$upgrade_rc" = "2" ]; then
    ok "an N-1 af upgrade quota error is classified as could-not-verify"
else
    no "an N-1 af upgrade quota error returned $upgrade_rc, want could-not-verify status 2"
fi
if lc_release_lookup_unavailable \
    "Error: failed to fetch latest release: GitHub API returned 404"; then
    no "a 404 was mislabeled as transient GitHub unavailability"
else
    ok "non-transient release API errors remain failures"
fi

# ALLOW_PARTIAL acknowledges SKIPs only; it must make an otherwise partial run
# green, but never pardon a genuine product assertion failure.
if (LC_FAIL=0; LC_SKIP=1; AF_LIFECYCLE_ALLOW_PARTIAL=1; lc_summary >/dev/null 2>&1); then
    ok "AF_LIFECYCLE_ALLOW_PARTIAL=1 permits a skip-only partial run"
else
    no "AF_LIFECYCLE_ALLOW_PARTIAL=1 still failed a skip-only partial run"
fi
if (LC_FAIL=1; LC_SKIP=1; LC_FAILED_NAMES=("real failure"); AF_LIFECYCLE_ALLOW_PARTIAL=1; lc_summary >/dev/null 2>&1); then
    no "AF_LIFECYCLE_ALLOW_PARTIAL=1 pardoned a genuine FAIL"
else
    ok "AF_LIFECYCLE_ALLOW_PARTIAL=1 does not pardon genuine FAILs"
fi

printf '\n=== the kill path refuses an empty home ===\n'

# lc_daemon_pids feeds lc_teardown_home, which SIGKILLs what it returns. With an
# empty home its match becomes [ "$dhome" = "" ], which matches every daemon
# whose environ carries NO AGENT_FACTORY_HOME — exactly how a real default-home
# daemon runs. This is the difference between a scoped teardown and shooting the
# maintainer's daemon.
if lc_daemon_pids "" >/dev/null 2>&1; then
    no "lc_daemon_pids accepted an empty home (would match the real default-home daemon)"
else
    ok "lc_daemon_pids refuses an empty home"
fi
# ...and it must print nothing, since the caller pipes its output into kill.
if [ -z "$(lc_daemon_pids "" 2>/dev/null)" ]; then
    ok "lc_daemon_pids emits no pids for an empty home"
else
    no "lc_daemon_pids emitted pids for an empty home — those would be killed"
fi
# A home nothing serves must simply be empty, not an error.
if [ -z "$(lc_daemon_pids "/tmp/lc-selftest-no-such-home" 2>/dev/null)" ]; then
    ok "a home with no daemons yields no pids"
else
    no "a home with no daemons yielded pids"
fi

printf '\n=== the disposable guard defaults to NO ===\n'

# These drive the REAL lc_detect_disposable by pointing its markers at temp
# paths. An earlier version of this test re-implemented the detection and
# grepped the source for "/run/.containerenv" — and PASSED against code with
# podman detection deleted, because that string also lives in the guard's error
# message. Watched failing is the only reason we know this one works.
probe="$SELFTEST_TMP/disposable"
mkdir -p "$probe"
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
