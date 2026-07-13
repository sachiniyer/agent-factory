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
#   scripts/testbox.sh build                   (re)build the image only
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

# _uniq — a per-invocation token (pid + a little randomness) for container
# names, so two runs never share a name.
_uniq() {
    local r=""
    [ -r /dev/urandom ] && r="$(od -An -N3 -tx1 /dev/urandom 2>/dev/null | tr -d ' ')"
    printf '%s' "$$${r:+-$r}"
}

# The sandbox container name defaults to a UNIQUE per-run value so concurrent
# play-tests can't `docker rm -f` each other mid-run (#1171). Pin it with
# AF_PLAYTEST_NAME to reuse/target a specific container. Whatever the value,
# every message, teardown hint, and `docker exec`/`rm` below uses THIS
# resolved name — never a hardcoded 'af-playtest'.
PLAYTEST_NAME="${AF_PLAYTEST_NAME:-af-playtest-$(_uniq)}"

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

build_image() {
    # Dockerfile via stdin: no build context is sent (the Dockerfile has no
    # COPY), so this is instant when layers are cached.
    "$ENGINE" build -q -t "$IMAGE" - <"$REPO_ROOT/scripts/container/Dockerfile.test" >/dev/null
}

# Flags shared by every run:
# - source mounted read-only; in-container scripts copy it before writing
# - named volumes for module/build caches so the second run is fast
# - no host $HOME, $TMUX, or TMUX_TMPDIR passthrough; no published ports
# - bounded pids + memory (override: AF_TESTBOX_PIDS / AF_TESTBOX_MEMORY)
RUN_FLAGS=(
    --rm
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
fix_cache_perms() {
    "$ENGINE" run --rm --user 0 \
        -v af-testbox-gomod:/cache/gomod \
        -v af-testbox-gobuild:/cache/gobuild \
        "$IMAGE" chown -R dev:dev /cache >/dev/null
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
    "$ENGINE" run -d \
        "${RUN_FLAGS[@]}" \
        --name "$PLAYTEST_NAME" \
        -e AGENT_FACTORY_HOME=/home/dev/sandbox/home \
        -e "AGENT_FACTORY_AUTO_UPDATE=${AGENT_FACTORY_AUTO_UPDATE:-false}" \
        "$IMAGE" bash /src/scripts/container/playtest-entry.sh hold >/dev/null
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
    "$ENGINE" exec "$PLAYTEST_NAME" sh -c 'until [ -x /home/dev/bin/af ]; do sleep 1; done'
}

cmd="${1:-test}"
[ $# -gt 0 ] && shift

case "$cmd" in
build)
    "$ENGINE" build -t "$IMAGE" - <"$REPO_ROOT/scripts/container/Dockerfile.test"
    ;;
test)
    build_image
    fix_cache_perms
    # The one sanctioned home for a bare full-suite run on a shared box.
    exec "$ENGINE" run "${RUN_FLAGS[@]}" "$IMAGE" \
        bash /src/scripts/container/run-tests.sh "$@"
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
        exec "$ENGINE" run -it \
            "${RUN_FLAGS[@]}" \
            --name "$PLAYTEST_NAME" \
            -e AGENT_FACTORY_HOME=/home/dev/sandbox/home \
            -e "AGENT_FACTORY_AUTO_UPDATE=${AGENT_FACTORY_AUTO_UPDATE:-false}" \
            "$IMAGE" bash /src/scripts/container/playtest-entry.sh
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
drive)
    # Bring up a uniquely-named sandbox (#1171), boot af through the driver,
    # then attach you interactively to the live driver session so you can
    # watch/drive it by hand. Detach with your tmux prefix + d.
    ensure_playtest_up
    "$ENGINE" exec "$PLAYTEST_NAME" bash -lc \
        'source /src/scripts/tui-driver.sh && af_boot' >&2
    echo "testbox: af is up in session 'drive'; attaching (detach with prefix+d)." >&2
    echo "testbox: tear down with: $ENGINE rm -f $PLAYTEST_NAME" >&2
    exec "$ENGINE" exec -it "$PLAYTEST_NAME" tmux attach -t drive
    ;;
web-selftest)
    # Playwright web-driver-selftest (#1592 Phase 5 PR6): build the dedicated
    # Go+Node+Chromium image, then run the whole harness in ONE ephemeral
    # container — it builds af, boots a real daemon on a loopback TLS+token
    # listener, and drives the embedded SPA in a headless Chromium. Everything
    # (daemon, tmux, browser) lives on 127.0.0.1 inside the container: no
    # published ports, no host tmux, no real AF home.
    "$ENGINE" build -q -t "$WEB_IMAGE" - <"$REPO_ROOT/scripts/container/Dockerfile.web-selftest" >/dev/null
    # Dedicated cache volumes (not the shared testbox ones): this container runs
    # as root, so mixing caches would leave root-owned files the dev-user testbox
    # can't write. Chromium wants more memory + pids than the default suite.
    exec "$ENGINE" run --rm --init \
        -v "$REPO_ROOT":/src:ro \
        -v af-web-selftest-gomod:/cache/gomod \
        -v af-web-selftest-gobuild:/cache/gobuild \
        --pids-limit "${AF_TESTBOX_PIDS:-2048}" \
        --memory "${AF_WEB_TESTBOX_MEMORY:-4g}" \
        "$WEB_IMAGE" bash /src/scripts/container/web-selftest-entry.sh
    ;;
*)
    echo "testbox: unknown command '$cmd' (want: test | playtest | selftest | drive | web-selftest | build)" >&2
    exit 1
    ;;
esac
