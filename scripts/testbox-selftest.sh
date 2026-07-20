#!/usr/bin/env bash
# testbox-selftest.sh — tests for the testbox harness's OWN disk hygiene (#2133).
#
# scripts/testbox.sh proves things about af. This proves things about the
# harness, and like scripts/lifecycle-selftest.sh it is the cheap half: every
# case here runs against a FAKE docker that only records its argv, so there is
# no container, no image, no daemon, and the whole file runs on the host in
# about a second and can gate every PR.
#
# It exists because this harness's failure mode is invisible in a passing run.
# `make test-container` went green for months while leaving a dangling 1.2GB
# image behind on every Dockerfile change and never once pruning a build cache,
# until the box hit 100% and the af daemon could no longer persist state. Green
# was never the signal. So the properties asserted here are the two that the
# incident turned on:
#
#   * cleanup HAPPENS — including on the failing run, which is the run that
#     used to skip it (the old code `exec`ed docker, so no trap could fire);
#   * cleanup is SCOPED — every prune is filtered to this harness's own label,
#     and the volumes removed are the exact names the harness mounts. A blanket
#     `docker system prune -af` would also have fixed the disk, and would have
#     deleted co-tenants' images off a shared box to do it.
#
# Each case was watched failing against the pre-fix script before the fix landed.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Overridable so the cases can be watched failing against a pre-fix copy of the
# script — the only way to know this file gates anything.
TESTBOX="${AF_TESTBOX_SCRIPT:-$HERE/testbox.sh}"
LABEL="af.harness=testbox"
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

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# A fake docker that executes nothing and records every invocation, one argv
# per line, in $DOCKER_LOG. Enough of the real CLI's behaviour is faked that
# testbox.sh takes its normal path: builds read their Dockerfile from stdin,
# `builder prune --help` advertises a ceiling flag, and `volume inspect` misses
# so the labelled create is exercised.
mkdir -p "$WORK/bin"
cat >"$WORK/bin/docker" <<'SHIM'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$DOCKER_LOG"
for a in "$@"; do
    case "$a" in
    build) exec cat >/dev/null ;;
    esac
done
case "$1 ${2:-}" in
"info --format") printf '%s\n' "${FAKE_INFO_ROOT:-}" ;;
"builder prune")
    case "${*}" in
    *--help*) echo "  --max-used-space bytes  Maximum amount of disk space allowed to keep for cache" ;;
    esac
    ;;
"volume inspect") exit 1 ;;
esac
# Only the run under test fails, and only when asked to: a failing
# fix_cache_perms would abort the script before the interesting part.
case "${*}" in
*run-tests.sh*) exit "${FAKE_RUN_RC:-0}" ;;
esac
exit 0
SHIM
chmod +x "$WORK/bin/docker"

# run_testbox <log-name> <expected-rc> [args...] — run testbox.sh against the
# fake engine. Returns the log path in $LOG.
run_testbox() {
    local name="$1" want="$2"
    shift 2
    LOG="$WORK/$name.log"
    : >"$LOG"
    local rc=0
    env PATH="$WORK/bin:$PATH" DOCKER_LOG="$LOG" FAKE_RUN_RC="${FAKE_RUN_RC:-0}" \
        AF_TESTBOX_IMAGE=af-selftest-image \
        bash "$TESTBOX" "$@" >/dev/null 2>&1 || rc=$?
    if [ "$rc" != "$want" ]; then
        no "$name: testbox exited $rc, want $want"
        return 1
    fi
    return 0
}

# grep_log <log> <pattern> — a line matching pattern exists.
has() { grep -qE "$2" "$1"; }

printf '\n=== every artifact the harness creates is labelled ===\n'

FAKE_RUN_RC=0 run_testbox pass 0 test ./config/...
if has "$LOG" "^build .*--label $LABEL"; then
    ok "the image build carries the harness label"
else
    no "the image build is unlabelled — cleanup would have nothing to filter on"
fi
if has "$LOG" "^run .*--label $LABEL.*run-tests\.sh"; then
    ok "the test container carries the harness label"
else
    no "the test container is unlabelled"
fi
if has "$LOG" "^run .*--rm"; then
    ok "the test container is --rm (no writable layer, no anonymous volume, survives nothing)"
else
    no "the test container is not --rm"
fi
if has "$LOG" "^volume create --label $LABEL"; then
    ok "the cache volumes are created labelled rather than auto-created bare"
else
    no "the cache volumes are auto-created unlabelled"
fi

printf '\n=== cleanup happens, on the passing run and the FAILING one ===\n'

if has "$LOG" "^image prune"; then
    ok "a passing run reaps its dangling images"
else
    no "a passing run leaves dangling images behind"
fi
if has "$LOG" "^builder prune .*(--max-used-space|--keep-storage)"; then
    ok "a passing run caps the build cache"
else
    no "a passing run leaves the build cache uncapped"
fi

# The regression that mattered: testbox.sh used to `exec` docker run, which
# replaces the shell — so no EXIT trap could fire and a red run cleaned up
# nothing. A failing suite is exactly when you re-run, i.e. exactly when the
# residue compounds fastest.
FAKE_RUN_RC=7 run_testbox fail 7 test ./config/...
if has "$LOG" "^image prune"; then
    ok "a FAILING run still reaps its dangling images"
else
    no "a failing run skips cleanup (did an 'exec' come back?)"
fi
if has "$LOG" "^builder prune"; then
    ok "a FAILING run still caps the build cache"
else
    no "a failing run skips the build-cache cap"
fi
unset FAKE_RUN_RC

printf '\n=== cleanup can never reach an artifact the harness did not create ===\n'

# Assert over the union of every log this file produced: no matter the
# subcommand, a prune is either label-scoped or does not exist.
FAKE_RUN_RC=0 run_testbox clean 0 clean
ALL="$WORK/all.log"
cat "$WORK"/*.log >"$ALL"

if has "$ALL" "^system prune"; then
    no "the harness runs 'docker system prune' — that deletes co-tenants' images"
else
    ok "'docker system prune' is never issued"
fi
if has "$ALL" "^volume prune"; then
    no "the harness runs 'docker volume prune' — it would sweep unrelated volumes"
else
    ok "'docker volume prune' is never issued (volumes go by exact name)"
fi

unscoped=0
while IFS= read -r line; do
    case "$line" in
    "image prune"* | "container prune"*)
        case "$line" in
        *"--filter label=$LABEL"*) ;;
        *)
            unscoped=$((unscoped + 1))
            printf '        unscoped: %s\n' "$line"
            ;;
        esac
        ;;
    esac
done <"$ALL"
if [ "$unscoped" -eq 0 ]; then
    ok "every image/container prune is filtered to label=$LABEL"
else
    no "$unscoped prune(s) ran without the harness label filter"
fi

# The build-cache cap is the one thing that cannot be label-scoped (BuildKit
# records carry no labels), so it must be a CEILING and never a wipe: an
# unbounded `builder prune` would throw away a co-tenant's warm cache too.
capless=0
while IFS= read -r line; do
    case "$line" in
    "builder prune"*)
        case "$line" in
        *--help*) ;;
        *--max-used-space*| *--keep-storage*) ;;
        *)
            capless=$((capless + 1))
            printf '        uncapped: %s\n' "$line"
            ;;
        esac
        ;;
    esac
done <"$ALL"
if [ "$capless" -eq 0 ]; then
    ok "the build cache is only ever capped, never wiped wholesale"
else
    no "$capless build-cache prune(s) ran with no ceiling"
fi

# `clean` empties the Go caches. Those are named volumes shared by name across
# worktrees, so removing one that is not ours is the same class of mistake as
# an unscoped prune.
KNOWN=" af-testbox-gomod af-testbox-gobuild af-web-selftest-gomod af-web-selftest-gobuild "
stray=0
while IFS= read -r line; do
    case "$line" in
    "volume rm "*)
        v="${line#volume rm }"
        case "$KNOWN" in
        *" $v "*) ;;
        *)
            stray=$((stray + 1))
            printf '        stray volume: %s\n' "$v"
            ;;
        esac
        ;;
    esac
done <"$ALL"
if [ "$stray" -eq 0 ]; then
    ok "clean removes only the cache volumes the harness itself mounts"
else
    no "$stray volume(s) removed that the harness does not own"
fi

# `clean` also removes images by exact tag, because a sibling worktree on a
# checkout without the labelling rebuilds the shared tag and strips the label
# off it (observed on the dev box). That fallback has to stay pinned to the tags
# this script itself builds — a broader match is how a cleanup target starts
# eating images it does not own.
KNOWN_TAGS=" af-selftest-image agent-factory-web-selftest "
strayimg=0
while IFS= read -r line; do
    case "$line" in
    "rmi "*)
        t="${line#rmi }"
        case "$KNOWN_TAGS" in
        *" $t "*) ;;
        *)
            strayimg=$((strayimg + 1))
            printf '        stray image: %s\n' "$t"
            ;;
        esac
        ;;
    esac
done <"$ALL"
if [ "$strayimg" -eq 0 ]; then
    ok "clean removes images only by the exact tags this script builds"
else
    no "$strayimg image(s) removed by tag that the harness does not build"
fi

# A running container is somebody's in-flight run or parked play-test sandbox.
# `clean` on a shared box must not be the thing that kills it (#2175 is what
# that costs).
if has "$WORK/clean.log" "^rm -f"; then
    no "clean force-removes containers — a sibling's in-flight run looks just like one"
else
    ok "clean never force-removes a running container"
fi

printf '\n=== the low-disk probe fails safe ===\n'

# The probe reads the engine's root dir and df's it. A remote engine, or a root
# this host cannot see, means it simply does not know how much space there is —
# and the only acceptable behaviour for a probe that cannot know is silence.
# The failure worth guarding is the other one: answering anyway, and either
# crying wolf until the warning is ignored or reporting plenty right before the
# disk fills.
LOG="$WORK/nodisk.log"
: >"$LOG"
rc=0
env PATH="$WORK/bin:$PATH" DOCKER_LOG="$LOG" FAKE_INFO_ROOT=/nonexistent/docker-root \
    AF_TESTBOX_IMAGE=af-selftest-image \
    bash "$TESTBOX" test ./config/... >"$WORK/nodisk.out" 2>&1 || rc=$?
if [ "$rc" -eq 0 ]; then
    ok "an unreadable engine root does not fail the run"
else
    no "an unreadable engine root broke the run (exit $rc)"
fi
if grep -qiE "free on|under the" "$WORK/nodisk.out"; then
    no "the probe warned about free space it had no way to measure"
else
    ok "the probe says nothing when it cannot measure free space"
fi

printf '\n=== opting out ===\n'

LOG="$WORK/off.log"
: >"$LOG"
env PATH="$WORK/bin:$PATH" DOCKER_LOG="$LOG" AF_TESTBOX_CACHE_MAX=off \
    AF_TESTBOX_IMAGE=af-selftest-image \
    bash "$TESTBOX" test ./config/... >/dev/null 2>&1 || true
if has "$LOG" "^builder prune"; then
    no "AF_TESTBOX_CACHE_MAX=off still touched the build cache"
else
    ok "AF_TESTBOX_CACHE_MAX=off leaves the shared build cache alone"
fi
if has "$LOG" "^image prune"; then
    ok "AF_TESTBOX_CACHE_MAX=off still reaps our own dangling images"
else
    no "the cache opt-out also disabled the label-scoped image reap"
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
