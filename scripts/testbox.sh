#!/usr/bin/env bash
# testbox.sh — run the agent-factory test suite or a TUI play-test sandbox
# inside a container, fenced off from the host tmux server, the real
# ~/.agent-factory, and this repo's worktrees (#1123).
#
# Usage:
#   scripts/testbox.sh test [go-test-args...]  full suite (default ./...)
#   scripts/testbox.sh playtest                interactive sandbox shell
#   scripts/testbox.sh playtest -d             detached; drive via `docker exec`
#   scripts/testbox.sh selftest                run the TUI driver self-test (#1161)
#   scripts/testbox.sh drive                   boot af via the driver + attach
#   scripts/testbox.sh lifecycle [scenario]    clean-install / install->upgrade gate
#   scripts/testbox.sh build                   (re)build the image only
#   scripts/testbox.sh clean                   reclaim this harness's disk (#2133)
#
# The container gets: its own tmux server, a throwaway AF home, pids/memory
# caps so a runaway generator (the 2026-07-03 outage class) suffocates
# inside the container instead of taking the box down, and the source
# mounted READ-ONLY. Nothing it does can touch the host environment.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${AF_TESTBOX_IMAGE:-agent-factory-testbox}"
# The web-driver-selftest uses a SEPARATE, heavier image (Go + Node + Chromium);
# see scripts/container/Dockerfile.web-selftest. Kept distinct so it never bloats
# the plain testbox image every other gate shares.
WEB_IMAGE="${AF_WEB_TESTBOX_IMAGE:-agent-factory-web-selftest}"
# The lifecycle gate's image tag is UNIQUE PER RUN.
#
# An image tag is a global name on the docker daemon, and this box runs many
# worktrees at once. A fixed tag has two failure modes, and we have now eaten
# both:
#   * a SIBLING gate rebuilds the shared `agent-factory-testbox` tag from THEIR
#     checkout's Dockerfile, so whichever build ran last wins — that silently
#     reverted the image under this gate mid-run ("missing required tool: jq");
#   * two CONCURRENT lifecycle runs from different worktrees clobber each
#     other's tag. That is exactly #1171 (the fixed `af-playtest` name), fixed in
#     #1166 with per-run-unique names. Re-introducing it in the sibling harness
#     would be repeating a bug we already paid for.
#
# So the tag carries the same per-run token the container name does, and is
# removed on exit. Layer cache is keyed on Dockerfile content, not the tag, so a
# unique tag costs nothing on a warm cache. Pin AF_LIFECYCLE_IMAGE to reuse one.
LIFECYCLE_IMAGE="${AF_LIFECYCLE_IMAGE:-}"

# _uniq — a per-invocation token (pid + a little randomness) for container
# names, so two runs never share a name.
_uniq() {
    local r=""
    [ -r /dev/urandom ] && r="$(od -An -N3 -tx1 /dev/urandom 2>/dev/null | tr -d ' ')"
    printf '%s' "$$${r:+-$r}"
}

RUN_TOKEN="$(_uniq)"

# The sandbox container name defaults to a UNIQUE per-run value so concurrent
# play-tests can't `docker rm -f` each other mid-run (#1171). Pin it with
# AF_PLAYTEST_NAME to reuse/target a specific container. Whatever the value,
# every message, teardown hint, and `docker exec`/`rm` below uses THIS
# resolved name — never a hardcoded 'af-playtest'.
PLAYTEST_NAME="${AF_PLAYTEST_NAME:-af-playtest-$RUN_TOKEN}"

# ---------------------------------------------------------------- disk (#2133)
#
# This harness once filled a 2TB box. Every image, container, and cache volume
# it creates therefore carries LABEL, so cleanup can filter on it and can never
# reach an artifact the harness did not build. Same shape as the docker
# backend's own `af.session` label (session/backend_docker.go).
LABEL="af.harness=testbox"

# The named cache volumes the harness mounts, listed explicitly rather than
# matched by prefix: these are the exact names mounted below, so `clean` removes
# what this script created and never something that merely looks like it. They
# are the single largest thing the harness holds (a Go build cache reaches tens
# of GB on a busy box) — Go's own 5-day cache trim bounds them, `clean` empties
# them.
CACHE_VOLUMES=(
    af-testbox-gomod
    af-testbox-gobuild
    af-web-selftest-gomod
    af-web-selftest-gobuild
)

# Ceiling for the shared builder cache. Unlike everything else here this cannot
# be scoped to the harness — BuildKit cache records carry no labels — so it is
# deliberately a ceiling and not a wipe: docker evicts least-recently-used
# records until the total fits, which leaves a co-tenant's warm cache alone as
# long as it is warmer than ours, and everything evicted is rebuildable. The
# default holds one warm generation of every image here (the web-selftest build
# alone is ~4GB of cache) with room for churn. Set it to `off` to leave the
# build cache untouched.
CACHE_MAX="${AF_TESTBOX_CACHE_MAX:-10GB}"

# Free space below which a run says something before it starts. What actually
# broke in #2133 was not a slow test — it was the af daemon on the same
# filesystem losing the ability to persist session state. Set to 0 to silence.
MIN_FREE_GB="${AF_TESTBOX_MIN_FREE_GB:-20}"

# docker-or-podman autodetect (docker on the current dev box; podman kept
# as a fallback for other boxes).
if command -v docker >/dev/null 2>&1; then
    ENGINE=docker
elif command -v podman >/dev/null 2>&1; then
    ENGINE=podman
else
    echo "testbox: need docker or podman on PATH" >&2
    exit 1
fi

# ---------------------------------------------------------------- image lease
#
# IMAGE and WEB_IMAGE are stable, daemon-global tags shared by every checkout on
# this box. A labelled `image prune -a` or exact-tag `rmi` from `testbox-clean`
# can therefore delete a sibling's freshly-built image in the gap before its
# first container exists. The engine has no reference to protect during that
# gap; once the container is RUNNING, it does.
#
# Serialize only that critical section — build through the engine's positive
# running observation — against tagged cleanup. The lock is host-global (not
# under REPO_ROOT), so sibling worktrees share it. A background observer releases
# it as soon as the named container is running; suites remain concurrent after
# startup. This also stops two sibling Dockerfile builds from retagging the
# stable name between another run's build and container creation.
#
# util-linux flock is the normal path on the shared Linux boxes. The mkdir
# fallback keeps Docker Desktop/other hosts working without adding a dependency;
# it records the owner PID and reaps a lock whose shell no longer exists.
IMAGE_LOCK_PATH="${AF_TESTBOX_IMAGE_LOCK:-${TMPDIR:-/tmp}/agent-factory-testbox-image-${UID}.lock}"
IMAGE_LOCK_DIR="${IMAGE_LOCK_PATH}.d"
IMAGE_LOCK_REAPER="${IMAGE_LOCK_PATH}.reap"
IMAGE_LOCK_HELD=no
IMAGE_LOCK_KIND=""
IMAGE_LOCK_FD=""
IMAGE_LOCK_WATCHER=""

acquire_image_lock_mkdir() {
    local owner="" announced=no
    while ! mkdir "$IMAGE_LOCK_DIR" 2>/dev/null; do
        owner="$(cat "$IMAGE_LOCK_DIR/pid" 2>/dev/null || true)"
        case "$owner" in
        '' | *[!0-9]*) ;;
        *)
            if ! kill -0 "$owner" 2>/dev/null && mkdir "$IMAGE_LOCK_REAPER" 2>/dev/null; then
                # Re-check under the reaper mutex. Another waiter may have
                # already replaced the stale owner before we acquired it.
                if [ "$(cat "$IMAGE_LOCK_DIR/pid" 2>/dev/null || true)" = "$owner" ] &&
                    ! kill -0 "$owner" 2>/dev/null; then
                    rm -f "$IMAGE_LOCK_DIR/pid"
                    rmdir "$IMAGE_LOCK_DIR" 2>/dev/null || true
                fi
                rmdir "$IMAGE_LOCK_REAPER" 2>/dev/null || true
                continue
            fi
            ;;
        esac
        if [ "$announced" = no ]; then
            echo "testbox: waiting for another harness invocation to finish image startup..." >&2
            announced=yes
        fi
        sleep 0.1
    done
    printf '%s\n' "$$" >"$IMAGE_LOCK_DIR/pid"
    IMAGE_LOCK_KIND="mkdir"
    IMAGE_LOCK_HELD=yes
}

acquire_image_lock() {
    if [ "$IMAGE_LOCK_HELD" = yes ]; then
        return 0
    fi
    if command -v flock >/dev/null 2>&1 && [ "${AF_TESTBOX_FORCE_MKDIR_LOCK:-0}" != 1 ]; then
        # shellcheck disable=SC3045 # dynamic FDs are a bash feature; this is a bash script.
        exec {IMAGE_LOCK_FD}>"$IMAGE_LOCK_PATH"
        if ! flock -n "$IMAGE_LOCK_FD"; then
            echo "testbox: waiting for another harness invocation to finish image startup..." >&2
            flock "$IMAGE_LOCK_FD"
        fi
        IMAGE_LOCK_KIND=flock
        IMAGE_LOCK_HELD=yes
        return 0
    fi
    acquire_image_lock_mkdir
}

release_image_lock() {
    if [ "$IMAGE_LOCK_HELD" != yes ]; then
        return 0
    fi
    case "$IMAGE_LOCK_KIND" in
    flock)
        flock -u "$IMAGE_LOCK_FD" 2>/dev/null || true
        ;;
    mkdir)
        if [ "$(cat "$IMAGE_LOCK_DIR/pid" 2>/dev/null || true)" = "$$" ]; then
            rm -f "$IMAGE_LOCK_DIR/pid"
            rmdir "$IMAGE_LOCK_DIR" 2>/dev/null || true
        fi
        ;;
    esac
    IMAGE_LOCK_HELD=no
}

# release_image_lock_when_running <container-name> — called in a subshell. Bash
# keeps $$ equal to the parent shell there, so both flock and the mkdir owner
# check release the parent's lease. If docker run fails before reaching Running,
# the parent releases after the command returns instead.
release_image_lock_when_running() {
    local name="$1"
    while :; do
        if [ "$("$ENGINE" inspect -f '{{.State.Running}}' "$name" 2>/dev/null || true)" = true ]; then
            release_image_lock
            return 0
        fi
        sleep 0.05
    done
}

watch_image_start() {
    release_image_lock_when_running "$1" &
    IMAGE_LOCK_WATCHER=$!
}

finish_image_start() {
    if [ -n "$IMAGE_LOCK_WATCHER" ]; then
        kill "$IMAGE_LOCK_WATCHER" 2>/dev/null || true
        wait "$IMAGE_LOCK_WATCHER" 2>/dev/null || true
        IMAGE_LOCK_WATCHER=""
    fi
    release_image_lock
}

build_image() {
    # Dockerfile via stdin: no build context is sent (the Dockerfile has no
    # COPY), so this is instant when layers are cached.
    "$ENGINE" build -q --label "$LABEL" -t "$IMAGE" - <"$REPO_ROOT/scripts/container/Dockerfile.test" >/dev/null
}

# ensure_cache_volumes — create the named cache volumes up front, labelled.
#
# `docker run -v name:/path` auto-creates a volume with no labels, which cleanup
# then has no precise way to recognize. Creating them here means every volume
# this harness owns is self-identifying. Idempotent, and it deliberately does
# NOT relabel a volume that already exists — `clean` matches CACHE_VOLUMES by
# name too, so the ones predating this became sweepable without being touched.
ensure_cache_volumes() {
    local v
    for v in "$@"; do
        "$ENGINE" volume inspect "$v" >/dev/null 2>&1 ||
            "$ENGINE" volume create --label "$LABEL" "$v" >/dev/null 2>&1 || true
    done
}

# reap_dangling_images — drop images this harness built that no longer carry a
# tag.
#
# Every build target here (re)builds a STABLE tag, so any edit to a Dockerfile —
# or simply a sibling worktree whose Dockerfile differs — moves the tag off the
# previous image. Whether that image then lingers is an ENGINE property, not a
# harness one: with the classic overlay2 image store it becomes a dangling
# <none> that nothing collects, at ~1.2GB (testbox) / ~4GB (web-selftest) a
# copy; with the containerd snapshotter (docker 29's default, and what this box
# runs) the engine collects it itself. Measured both ways for #2133 — this reap
# is cheap insurance for the stores that need it, not the whole fix.
#
# Safe while a sibling run is in flight, twice over: `image prune` without `-a`
# considers only UNTAGGED images, and docker refuses to remove an image any
# container still references — so another run's image, tagged and in use, is
# doubly untouchable. The label filter is the outer wall: it cannot match an
# image built by anything but this harness.
reap_dangling_images() {
    "$ENGINE" image prune -f --filter "label=$LABEL" >/dev/null 2>&1 || true
}

# cap_build_cache — hold the builder cache under CACHE_MAX. See the CACHE_MAX
# comment for why this one is a ceiling rather than a label-scoped removal.
cap_build_cache() {
    if [ "$CACHE_MAX" = off ]; then
        return 0
    fi
    # docker >= 28 renamed --keep-storage to --max-used-space, and podman's
    # `builder prune` has neither. Probe the help text rather than parsing
    # version numbers, and do nothing at all if no ceiling flag exists.
    local flag
    case "$("$ENGINE" builder prune --help 2>&1)" in
    *--max-used-space*) flag=--max-used-space ;;
    *--keep-storage*) flag=--keep-storage ;;
    *) return 0 ;;
    esac
    "$ENGINE" builder prune -f "$flag" "$CACHE_MAX" >/dev/null 2>&1 || true
}

# warn_low_disk — say something before a run that the box may not have room for.
#
# Advisory only, and deliberately so: it warns and continues rather than
# refusing, because nothing here knows how much room this particular run needs,
# and a harness that refuses to run is its own kind of outage. It is equally
# deliberate that every way of NOT knowing — a remote engine, an unreadable
# root, a df that does not parse — exits quietly instead of guessing. A probe
# that answers anyway is how a warning becomes noise and then gets ignored.
warn_low_disk() {
    if [ "$MIN_FREE_GB" = 0 ]; then
        return 0
    fi
    local root free_kb want_kb
    root="$("$ENGINE" info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
    if [ -z "$root" ] || [ ! -d "$root" ]; then
        return 0
    fi
    free_kb="$(df -Pk "$root" 2>/dev/null | awk 'NR==2 {print $4}')"
    case "$free_kb" in
    '' | *[!0-9]*) return 0 ;;
    esac
    want_kb=$((MIN_FREE_GB * 1024 * 1024))
    if [ "$free_kb" -lt "$want_kb" ]; then
        echo "testbox: $((free_kb / 1024 / 1024))G free on $root, under the ${MIN_FREE_GB}G mark." >&2
        echo "testbox: a full disk stops the af daemon persisting session state, not just this run." >&2
        echo "testbox: reclaim this harness's share with: make testbox-clean" >&2
    fi
}

# Teardown runs on EVERY exit path — pass, test failure, Ctrl-C — so the run
# that leaves residue behind can never be the red one. Nothing below may `exec`
# for that reason: exec replaces this shell and the trap never fires.
teardown() {
    finish_image_start
    reap_dangling_images
    cap_build_cache
}
trap teardown EXIT

# lifecycle_teardown — the lifecycle gate's extra step, chained onto the shared
# one rather than replacing it: an EXIT trap is single-slot, so installing a
# bare `rmi` over it would silently disarm the disk reap for that target alone.
# Dropping the per-run tag is also precisely what leaves an image dangling, so
# the reap has to run after it.
# shellcheck disable=SC2317  # invoked via trap, below
lifecycle_teardown() {
    "$ENGINE" rmi -f "$LIFECYCLE_IMAGE" >/dev/null 2>&1 || true
    teardown
}

# Flags shared by every run:
# - source mounted read-only; in-container scripts copy it before writing
# - named volumes for module/build caches so the second run is fast
# - no host $HOME, $TMUX, or TMUX_TMPDIR passthrough; no published ports
# - bounded pids + memory (override: AF_TESTBOX_PIDS / AF_TESTBOX_MEMORY)
RUN_FLAGS=(
    # --rm also drops any anonymous volume the container created, so the only
    # volumes that outlive a run are the named caches below (#2133).
    --rm
    --label "$LABEL"
    # --init: a real PID 1 (tini) that reaps orphans. Without it, processes
    # the suite kills linger as zombies and the reaping tests see them as
    # "still alive".
    --init
    -v "$REPO_ROOT":/src:ro
    -v af-testbox-gomod:/cache/gomod
    -v af-testbox-gobuild:/cache/gobuild
    --pids-limit "${AF_TESTBOX_PIDS:-1024}"
    --memory "${AF_TESTBOX_MEMORY:-8g}"
)

# Speed: when the host has a warm Go module cache, expose its download dir
# as a read-only file:// module proxy — the first container run then needs
# no network at all. go.sum still verifies every module.
if command -v go >/dev/null 2>&1; then
    HOST_MODCACHE="$(go env GOMODCACHE 2>/dev/null || true)"
    if [ -n "$HOST_MODCACHE" ] && [ -d "$HOST_MODCACHE/cache/download" ]; then
        RUN_FLAGS+=(
            -v "$HOST_MODCACHE/cache/download":/hostproxy:ro
            -e "GOPROXY=file:///hostproxy,https://proxy.golang.org,direct"
        )
    fi
fi

# Cache volumes created by older harness versions (or manual root runs)
# can be root-owned, which the non-root test user can't write to. A
# one-shot root chown makes the harness self-healing.
# $1: image to run the chown in (defaults to the shared testbox image). The
# lifecycle gate passes its own so it never depends on the shared tag existing
# or being current.
fix_cache_perms() {
    ensure_cache_volumes af-testbox-gomod af-testbox-gobuild
    "$ENGINE" run --rm --label "$LABEL" --user 0 \
        -v af-testbox-gomod:/cache/gomod \
        -v af-testbox-gobuild:/cache/gobuild \
        "${1:-$IMAGE}" chown -R dev:dev /cache >/dev/null
}

# start_playtest_detached — run the play-test sandbox container detached, so a
# driver can work it over `docker exec`. Shared by `playtest -d`, `selftest`,
# and `drive`.
#
# AGENT_FACTORY_AUTO_UPDATE=false is passed at the CONTAINER level (not just
# exported in a driver shell) so it reaches EVERY process in the container —
# the tmux server, the `af` TUI it launches, and the daemon `af` spawns —
# exactly like AGENT_FACTORY_HOME does. `docker exec` inherits `docker run -e`
# vars but NOT a transient exec shell's exports, so a driver-shell-only export
# reaches `af` only when the tmux server happens to fork from that shell —
# flaky. Without this, a container binary built behind the latest release
# self-updates and restarts the daemon mid-selftest, racing instance creation
# (#1596, regression of the #1498 opt-out). Override to `true` to exercise the
# real auto-update path in the sandbox.
start_playtest_detached() {
    local rc=0
    watch_image_start "$PLAYTEST_NAME"
    "$ENGINE" run -d \
        "${RUN_FLAGS[@]}" \
        --name "$PLAYTEST_NAME" \
        -e AGENT_FACTORY_HOME=/home/dev/sandbox/home \
        -e "AGENT_FACTORY_AUTO_UPDATE=${AGENT_FACTORY_AUTO_UPDATE:-false}" \
        "$IMAGE" bash /src/scripts/container/playtest-entry.sh hold >/dev/null || rc=$?
    finish_image_start
    return "$rc"
}

# ensure_playtest_up — start the detached sandbox if it is not already
# running, then block until af has finished building inside it.
ensure_playtest_up() {
    if ! "$ENGINE" inspect -f '{{.State.Running}}' "$PLAYTEST_NAME" 2>/dev/null | grep -q true; then
        "$ENGINE" rm -f "$PLAYTEST_NAME" >/dev/null 2>&1 || true
        build_image
        fix_cache_perms
        echo "testbox: starting sandbox '$PLAYTEST_NAME' (af builds on boot)..." >&2
        start_playtest_detached
    fi
    # An existing sandbox already protects the image; a newly started one was
    # positively observed by docker run -d / the startup watcher above.
    finish_image_start
    "$ENGINE" exec "$PLAYTEST_NAME" sh -c 'until [ -x /home/dev/bin/af ]; do sleep 1; done'
}

cmd="${1:-test}"
[ $# -gt 0 ] && shift

# Every command can build/use a stable image tag, or (`clean`) remove one. Hold
# the same cross-worktree lease until the case either observes its first running
# container or completes tagged cleanup.
acquire_image_lock

# Every command but `clean` is about to consume disk; `clean` is the answer to
# the warning, so warning there would just be noise.
if [ "$cmd" != clean ]; then
    warn_low_disk
fi

case "$cmd" in
build)
    "$ENGINE" build --label "$LABEL" -t "$IMAGE" - <"$REPO_ROOT/scripts/container/Dockerfile.test"
    finish_image_start
    ;;
test)
    build_image
    fix_cache_perms
    # The one sanctioned home for a bare full-suite run on a shared box.
    # Not `exec`: the teardown trap has to survive the run (#2133).
    rc=0
    TESTBOX_NAME="af-testbox-test-$RUN_TOKEN"
    watch_image_start "$TESTBOX_NAME"
    "$ENGINE" run "${RUN_FLAGS[@]}" --name "$TESTBOX_NAME" "$IMAGE" \
        bash /src/scripts/container/run-tests.sh "$@" || rc=$?
    finish_image_start
    exit "$rc"
    ;;
playtest)
    build_image
    fix_cache_perms
    if [ "${1:-}" = "-d" ]; then
        start_playtest_detached
        echo "playtest sandbox '$PLAYTEST_NAME' is starting (af builds on boot)."
        echo "  wait for it:  $ENGINE exec $PLAYTEST_NAME sh -c 'until [ -x /home/dev/bin/af ]; do sleep 1; done'"
        echo "  drive it:     $ENGINE exec $PLAYTEST_NAME tmux new-session -d -s drive -x 80 -y 24"
        echo "                $ENGINE exec $PLAYTEST_NAME tmux send-keys -t drive 'cd ~/sandbox/mock-repo && af' Enter"
        echo "                $ENGINE exec $PLAYTEST_NAME tmux capture-pane -p -t drive"
        echo "  tear down:    $ENGINE rm -f $PLAYTEST_NAME"
    else
        rc=0
        watch_image_start "$PLAYTEST_NAME"
        "$ENGINE" run -it \
            "${RUN_FLAGS[@]}" \
            --name "$PLAYTEST_NAME" \
            -e AGENT_FACTORY_HOME=/home/dev/sandbox/home \
            -e "AGENT_FACTORY_AUTO_UPDATE=${AGENT_FACTORY_AUTO_UPDATE:-false}" \
            "$IMAGE" bash /src/scripts/container/playtest-entry.sh || rc=$?
        finish_image_start
        exit "$rc"
    fi
    ;;
selftest)
    # Run the TUI driver self-test (#1161) in an EPHEMERAL, uniquely-named
    # dedicated sandbox: unique so concurrent gates don't clobber each other
    # (#1171), ephemeral so gates leave nothing behind. Pin AF_SELFTEST_NAME
    # to reuse a specific container (then it is NOT torn down). The self-test
    # also resets sandbox state at its start, so a pinned reused container
    # stays deterministic.
    if [ -n "${AF_SELFTEST_NAME:-}" ]; then
        PLAYTEST_NAME="$AF_SELFTEST_NAME"; teardown=no
    else
        PLAYTEST_NAME="af-driver-selftest-$(_uniq)"; teardown=yes
    fi
    ensure_playtest_up
    rc=0
    "$ENGINE" exec "$PLAYTEST_NAME" bash /src/scripts/tui-driver-selftest.sh || rc=$?
    if [ "$teardown" = yes ]; then
        "$ENGINE" rm -f "$PLAYTEST_NAME" >/dev/null 2>&1 || true
    fi
    exit "$rc"
    ;;
scenario)
    # Run ONE driver scenario script in an ephemeral sandbox — the same fence
    # `selftest` uses, but for a script the caller names instead of the fixed
    # acceptance scenario.
    #
    # This exists because there was no way to run a one-off real-TUI gate: the
    # only options were the shared selftest (which a per-bug case should not be
    # bolted onto, since destabilizing the acceptance gate is worse than the bug
    # it guards) or `drive`, which attaches a human. A regression scenario for a
    # specific fix needs neither.
    #
    #   scripts/testbox.sh scenario scripts/tui-2413-scenario.sh
    #
    # The path is repo-relative; the repo is mounted read-only at /src.
    scenario_rel="${1:-}"
    [ -n "$scenario_rel" ] || { echo "testbox: scenario needs a script path (repo-relative)" >&2; exit 2; }
    [ -f "$REPO_ROOT/$scenario_rel" ] || { echo "testbox: no such scenario script: $scenario_rel" >&2; exit 2; }
    if [ -n "${AF_SELFTEST_NAME:-}" ]; then
        PLAYTEST_NAME="$AF_SELFTEST_NAME"; teardown=no
    else
        PLAYTEST_NAME="af-driver-scenario-$(_uniq)"; teardown=yes
    fi
    ensure_playtest_up
    rc=0
    "$ENGINE" exec "$PLAYTEST_NAME" bash "/src/$scenario_rel" || rc=$?
    if [ "$teardown" = yes ]; then
        "$ENGINE" rm -f "$PLAYTEST_NAME" >/dev/null 2>&1 || true
    fi
    exit "$rc"
    ;;
drive)
    # Bring up a uniquely-named sandbox (#1171), boot af through the driver,
    # then attach you interactively to the live driver session so you can
    # watch/drive it by hand. Detach with your tmux prefix + d.
    ensure_playtest_up
    "$ENGINE" exec "$PLAYTEST_NAME" bash -lc \
        'source /src/scripts/tui-driver.sh && af_boot' >&2
    echo "testbox: af is up in session 'drive'; attaching (detach with prefix+d)." >&2
    echo "testbox: tear down with: $ENGINE rm -f $PLAYTEST_NAME" >&2
    rc=0
    "$ENGINE" exec -it "$PLAYTEST_NAME" tmux attach -t drive || rc=$?
    exit "$rc"
    ;;
lifecycle)
    # Clean-environment install + upgrade gate. Unlike every other target here,
    # this one does not just run this tree's code: it installs REAL published
    # releases into throwaway machines and upgrades across the version boundary
    # — the seam #1921/#796 shipped through, which no single-version test can
    # reach.
    #
    # Network is required (it downloads real release tarballs), so this is the
    # one testbox target that is not hermetic. It is wired nightly rather than
    # per-PR for that reason — see .github/workflows/lifecycle.yml.
    #
    # Runs in an EPHEMERAL uniquely-named container AND under a uniquely-named
    # image tag (#1171/#1166) so concurrent runs cannot clobber each other and
    # nothing survives the gate. Built from the same Dockerfile as the testbox.
    lc_token="$(_uniq)"
    lc_teardown=no
    if [ -z "$LIFECYCLE_IMAGE" ]; then
        LIFECYCLE_IMAGE="agent-factory-lifecycle:$lc_token"
        lc_teardown=yes
    fi
    LIFECYCLE_NAME="af-lifecycle-$lc_token"
    # Remove the per-run tag however we exit; the underlying layers stay cached
    # for the next run. A pinned AF_LIFECYCLE_IMAGE is the caller's to manage.
    if [ "$lc_teardown" = yes ]; then
        trap lifecycle_teardown EXIT INT TERM
    fi
    "$ENGINE" build -q --label "$LABEL" -t "$LIFECYCLE_IMAGE" - <"$REPO_ROOT/scripts/container/Dockerfile.test" >/dev/null
    fix_cache_perms "$LIFECYCLE_IMAGE"
    rc=0
    watch_image_start "$LIFECYCLE_NAME"
    "$ENGINE" run --rm \
        "${RUN_FLAGS[@]}" \
        --name "$LIFECYCLE_NAME" \
        -e "GITHUB_TOKEN=${GITHUB_TOKEN:-}" \
        -e "AF_LIFECYCLE_INJECT=${AF_LIFECYCLE_INJECT:-}" \
        -e "AF_LIFECYCLE_ALLOW_PARTIAL=${AF_LIFECYCLE_ALLOW_PARTIAL:-1}" \
        "$LIFECYCLE_IMAGE" bash /src/scripts/container/lifecycle-entry.sh "$@" || rc=$?
    finish_image_start
    exit "$rc"
    ;;
web-selftest)
    # Playwright web-driver-selftest (#1592 Phase 5 PR6): build the dedicated
    # Go+Node+Chromium image, then run the whole harness in ONE ephemeral
    # container — it builds af, boots a real daemon on a loopback TLS+token
    # listener, and drives the embedded SPA in a headless Chromium. Everything
    # (daemon, tmux, browser) lives on 127.0.0.1 inside the container: no
    # published ports, no host tmux, no real AF home.
    "$ENGINE" build -q --label "$LABEL" -t "$WEB_IMAGE" - <"$REPO_ROOT/scripts/container/Dockerfile.web-selftest" >/dev/null
    # Dedicated cache volumes (not the shared testbox ones): this container runs
    # as root, so mixing caches would leave root-owned files the dev-user testbox
    # can't write. Chromium wants more memory + pids than the default suite.
    ensure_cache_volumes af-web-selftest-gomod af-web-selftest-gobuild
    rc=0
    WEB_SELFTEST_NAME="af-web-selftest-$RUN_TOKEN"
    watch_image_start "$WEB_SELFTEST_NAME"
    "$ENGINE" run --rm --label "$LABEL" --init \
        --name "$WEB_SELFTEST_NAME" \
        -v "$REPO_ROOT":/src:ro \
        -v af-web-selftest-gomod:/cache/gomod \
        -v af-web-selftest-gobuild:/cache/gobuild \
        --pids-limit "${AF_TESTBOX_PIDS:-2048}" \
        --memory "${AF_WEB_TESTBOX_MEMORY:-4g}" \
        "$WEB_IMAGE" bash /src/scripts/container/web-selftest-entry.sh || rc=$?
    finish_image_start
    exit "$rc"
    ;;
clean)
    # Reclaim the disk this harness holds, and only what this harness holds
    # (#2133). Every run already reaps its own dangling images and caps the
    # build cache; this is the deeper, explicit lever — it also empties the Go
    # module/build cache volumes, which are the largest thing here (tens of GB
    # on a busy box) and which no automatic step touches, because emptying them
    # costs the next run a full cold rebuild.
    #
    # A running labelled container is reported, never removed: on a box with
    # several worktrees that container is most likely a sibling's in-flight run
    # or a parked play-test sandbox, and `clean` is not allowed to be the thing
    # that kills it.
    running="$("$ENGINE" ps -q --filter "label=$LABEL" 2>/dev/null || true)"
    # `container prune` removes stopped containers only — the running ones are
    # skipped by the engine itself, not by a check here that could drift.
    echo "testbox: containers · $("$ENGINE" container prune -f --filter "label=$LABEL" 2>/dev/null |
        grep -i 'reclaimed' || echo 'nothing to reclaim')"

    # -a: tagged images too, not just dangling ones. Still label-filtered, so it
    # reaches agent-factory-testbox / -web-selftest and nothing else.
    echo "testbox: images · $("$ENGINE" image prune -af --filter "label=$LABEL" 2>/dev/null |
        grep -i 'reclaimed' || echo 'nothing to reclaim')"

    # …and by exact tag, because the label alone is not sufficient here. These
    # tags are global names on the engine, so a sibling worktree running a
    # checkout without this change rebuilds them and strips the label off —
    # observed while verifying #2133, and the same shared-tag hazard the
    # lifecycle gate already carries a unique tag to avoid. Matching the exact
    # names this script builds is precise by construction, not a guess: they are
    # defined a few lines up. An image a container is still using survives, so a
    # sibling's in-flight run is unaffected.
    for i in "$IMAGE" "$WEB_IMAGE"; do
        if "$ENGINE" rmi "$i" >/dev/null 2>&1; then
            echo "testbox: removed image $i"
        fi
    done

    for v in "${CACHE_VOLUMES[@]}"; do
        if "$ENGINE" volume rm "$v" >/dev/null 2>&1; then
            echo "testbox: removed cache volume $v"
        fi
    done

    cap_build_cache
    if [ "$CACHE_MAX" = off ]; then
        echo "testbox: left the build cache alone (AF_TESTBOX_CACHE_MAX=off)"
    else
        echo "testbox: build cache capped at $CACHE_MAX"
        echo "         it is shared with every other builder on this box, so emptying it is"
        echo "         yours to decide: $ENGINE builder prune -af"
    fi

    if [ -n "$running" ]; then
        echo
        echo "testbox: left $(printf '%s\n' "$running" | wc -l | tr -d ' ') running container(s) alone — a sibling's in-flight run or a"
        echo "         parked play-test sandbox looks exactly like this. Remove one deliberately with:"
        "$ENGINE" ps --filter "label=$LABEL" --format '           '"$ENGINE"' rm -f {{.Names}}   # {{.Status}}'
    fi
    "$ENGINE" system df
    ;;
*)
    echo "testbox: unknown command '$cmd' (want: test | playtest | selftest | drive | lifecycle | web-selftest | build | clean)" >&2
    exit 1
    ;;
esac
