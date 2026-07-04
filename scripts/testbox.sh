#!/usr/bin/env bash
# testbox.sh — run the agent-factory test suite or a TUI play-test sandbox
# inside a container, fenced off from the host tmux server, the real
# ~/.agent-factory, and this repo's worktrees (#1123).
#
# Usage:
#   scripts/testbox.sh test [go-test-args...]  full suite (default ./...)
#   scripts/testbox.sh playtest                interactive sandbox shell
#   scripts/testbox.sh playtest -d             detached; drive via `docker exec`
#   scripts/testbox.sh build                   (re)build the image only
#
# The container gets: its own tmux server, a throwaway AF home, pids/memory
# caps so a runaway generator (the 2026-07-03 outage class) suffocates
# inside the container instead of taking the box down, and the source
# mounted READ-ONLY. Nothing it does can touch the host environment.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${AF_TESTBOX_IMAGE:-agent-factory-testbox}"
PLAYTEST_NAME="${AF_PLAYTEST_NAME:-af-playtest}"

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
    "$ENGINE" build -q -t "$IMAGE" - <"$REPO_ROOT/Dockerfile.test" >/dev/null
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

cmd="${1:-test}"
[ $# -gt 0 ] && shift

case "$cmd" in
build)
    "$ENGINE" build -t "$IMAGE" - <"$REPO_ROOT/Dockerfile.test"
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
    RUN_FLAGS+=(
        --name "$PLAYTEST_NAME"
        # docker exec inherits env set at create time, so the sandbox home
        # is visible to every exec'd command, not just the entry script.
        -e AGENT_FACTORY_HOME=/home/dev/sandbox/home
    )
    if [ "${1:-}" = "-d" ]; then
        "$ENGINE" run -d "${RUN_FLAGS[@]}" "$IMAGE" \
            bash /src/scripts/container/playtest-entry.sh hold >/dev/null
        echo "playtest sandbox '$PLAYTEST_NAME' is starting (af builds on boot)."
        echo "  wait for it:  $ENGINE exec $PLAYTEST_NAME sh -c 'until [ -x /home/dev/bin/af ]; do sleep 1; done'"
        echo "  drive it:     $ENGINE exec $PLAYTEST_NAME tmux new-session -d -s drive -x 80 -y 24"
        echo "                $ENGINE exec $PLAYTEST_NAME tmux send-keys -t drive 'cd ~/sandbox/mock-repo && af' Enter"
        echo "                $ENGINE exec $PLAYTEST_NAME tmux capture-pane -p -t drive"
        echo "  tear down:    $ENGINE rm -f $PLAYTEST_NAME"
    else
        exec "$ENGINE" run -it "${RUN_FLAGS[@]}" "$IMAGE" \
            bash /src/scripts/container/playtest-entry.sh
    fi
    ;;
*)
    echo "testbox: unknown command '$cmd' (want: test | playtest | build)" >&2
    exit 1
    ;;
esac
