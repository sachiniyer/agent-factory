#!/usr/bin/env bash
# record-demo.sh — regenerate docs/assets/demo.* from real Agent Factory use.
#
# The recording runs in the same isolated sandbox as `make
# playtest-container`: a disposable mock repository, throwaway AF home, and
# private tmux server. The source checkout is mounted read-only. Three genuine
# Codex sessions work concurrently in AF-owned worktrees while the deterministic
# TUI driver moves between them; a final Terminal tab runs one worktree's real
# tests. Nothing in the recorded panes is a transcript or mockup.
#
# The play-test harness intentionally never mounts host credentials. Recording
# therefore requires an explicit auth file, copied into only the throwaway
# container and removed with it:
#
#   AF_DEMO_CODEX_AUTH_FILE="$HOME/.codex/auth.json" \
#     scripts/container/record-demo.sh
set -euo pipefail

COLS="${DEMO_COLS:-168}"
ROWS="${DEMO_ROWS:-32}"
AGG_FONT_SIZE="${DEMO_FONT_SIZE:-14}"
AGG_THEME="${DEMO_THEME:-dracula}"
AGG_FPS="${DEMO_FPS:-12}"
AGG_IDLE="${DEMO_IDLE:-3.0}"
AGG_SPEED="${DEMO_SPEED:-1.8}"
TARGET_DURATION="${DEMO_TARGET_DURATION:-24}"
GIFSICLE_LOSSY="${DEMO_LOSSY:-110}"
GIFSICLE_COLORS="${DEMO_COLORS:-48}"
MAX_GIF_BYTES="${DEMO_MAX_GIF_BYTES:-2000000}"

# ---------------------------------------------------------------------------
# Container side — invoked after the real-agent play-test sandbox is ready.
# ---------------------------------------------------------------------------
if [ "${AF_DEMO_INNER:-}" = 1 ]; then
    export AF_DRIVER_COLS="$COLS" AF_DRIVER_ROWS="$ROWS"
    export AF_DRIVER_BIN=/home/dev/bin/af
    export AGENT_FACTORY_HOME="$HOME/sandbox/home"

    sandbox="$HOME/sandbox"
    out="$HOME/out"
    cast=/tmp/demo.cast
    raw_gif=/tmp/demo-raw.gif
    out_gif="$out/demo.gif"
    out_webm="$out/demo.webm"
    out_mp4="$out/demo.mp4"
    out_poster="$out/demo-poster.png"
    codex_wrapper="$HOME/bin/codex"
    real_codex="$HOME/bin/codex-real"
    tab_command="$HOME/bin/demo-tab"
    mkdir -p "$out/frames"

    if [ "$(cat "$sandbox/playtest-agent-kind" 2>/dev/null || true)" != real ]; then
        echo "record-demo: play-test sandbox is not using a real agent" >&2
        exit 1
    fi
    "$codex_wrapper" login status >/dev/null

    # AF recognizes agents from the configured command's executable basename.
    # Keep `codex` as the wrapper name so prompt readiness uses Codex-specific
    # composer/trust handling, and move the installed CLI behind that wrapper.
    mv "$codex_wrapper" "$real_codex"
    cp /src/docs/assets/demo-agent.sh "$codex_wrapper"
    cp /src/docs/assets/demo-tab.sh "$tab_command"
    chmod +x "$codex_wrapper" "$real_codex" "$tab_command"

    # Use the real Codex wrapper without hand-editing the sandbox config. These
    # are ordinary AF settings and take effect when af_boot starts the daemon.
    "$AF_DRIVER_BIN" config set default_program codex >/dev/null
    "$AF_DRIVER_BIN" config set program_overrides.codex "$codex_wrapper" >/dev/null

    # shellcheck disable=SC1091
    source /src/scripts/tui-driver.sh
    af_reset_sandbox
    af_boot
    tmux set-option -g status off

    create_session() {
        local name="$1" prompt="$2"
        (
            cd "$AF_DRIVER_REPO"
            "$AF_DRIVER_BIN" sessions create "$name" --prompt "$prompt" >/dev/null
        )
    }

    # Each command creates a real AF session, branch, and isolated worktree. The
    # prompts are deliberately small enough to finish during a README demo while
    # still requiring genuine repository inspection, edits, and verification.
    create_session validate-add \
        "Fix todo.sh so ./todo.sh add with no text exits nonzero and prints a concise usage message. Add regression coverage to test.sh, run ./test.sh, and summarize the result."
    create_session json-output \
        "Add a json command to todo.sh that prints the todo items as a valid JSON array without adding dependencies. Add focused coverage to test.sh, run it, and summarize the result."
    create_session document-cli \
        "Improve README.md with concise examples for list, add, and done, including the behavior for an invalid item number. Keep it accurate to todo.sh, inspect the script first, and summarize the documentation change."

    af_wait_for 'Sessions \(3\)' 15 'three real sessions in the rail'
    af_wait_for 'OpenAI Codex|gpt-5' 30 'real Codex pane output'

    # Record a read-only mirror. The geometry belongs to this container's tmux
    # server; the maintainer's tmux socket is never visible here.
    tmux kill-session -t rec 2>/dev/null || true
    tmux new-session -d -s rec -x "$COLS" -y "$ROWS"
    tmux set-option -t rec window-size manual >/dev/null 2>&1 || true
    tmux resize-window -t rec -x "$COLS" -y "$ROWS" 2>/dev/null || true
    tmux send-keys -t rec \
        "asciinema rec --overwrite -c 'env TMUX= tmux attach -t drive -r' $cast" Enter
    for _ in $(seq 1 50); do
        tmux list-clients -t drive 2>/dev/null | grep -q read-only && break
        sleep 0.2
    done
    if ! tmux list-clients -t drive 2>/dev/null | grep -q read-only; then
        echo "record-demo: recorder did not attach to the isolated TUI" >&2
        exit 1
    fi

    beat() { sleep "$1"; }

    # The operator moves across three agents while their real turns continue in
    # parallel. The rail keeps each branch/worktree visible, and every selection
    # shows that agent's own live Codex pane.
    beat 1.2
    af_select validate-add
    beat 3.0
    af_select json-output
    beat 4.0
    af_select document-cli
    beat 4.0

    # Let the agents reach their mechanically observed idle state while the
    # selected pane continues repainting. This waits on real AF state, not a
    # scripted transcript duration.
    for name in validate-add json-output document-cli; do
        (
            cd "$AF_DRIVER_REPO"
            timeout 180 "$AF_DRIVER_BIN" sessions watch "$name" >/dev/null
        )
    done

    # Briefly review each completed result, then open a genuine Terminal tab in
    # one worktree and run that branch's tests beside its Agent pane.
    af_select validate-add
    beat 2.4
    af_select json-output
    beat 2.4
    af_select document-cli
    beat 2.4
    af_select validate-add
    af_new_tab
    af_enter_interactive
    af_send_literal "clear; $tab_command"
    af_send Enter
    af_wait_for 'Demo worktree tests pass' 20 'real worktree tests'
    beat 4.0
    af_exit_interactive
    beat 2.0

    # Interrupt only the recorder so asciinema saves the cast without painting a
    # detached banner into the last frame.
    pkill -INT -f 'asciinema rec' 2>/dev/null || true
    for _ in $(seq 1 50); do
        pgrep -f 'asciinema rec' >/dev/null || break
        sleep 0.3
    done
    if [ ! -s "$cast" ]; then
        echo "record-demo: cast was not written" >&2
        exit 1
    fi

    # Treat DEMO_SPEED as a floor and adapt upward when real agents take longer.
    # Targeting 24s leaves six seconds of headroom below the README limit even
    # before agg trims static gaps with --idle-time-limit.
    cast_duration="$(tail -n 1 "$cast" | jq -r '.[0]')"
    if ! render_speed="$(awk \
        -v floor="$AGG_SPEED" -v duration="$cast_duration" -v target="$TARGET_DURATION" \
        'BEGIN {
            if (floor <= 0 || duration <= 0 || target <= 0) exit 1
            adaptive = duration / target
            printf "%.3f", (adaptive > floor ? adaptive : floor)
        }')"; then
        echo "record-demo: render speed and target duration must be positive" >&2
        exit 2
    fi

    agg --font-family "JetBrains Mono" --font-size "$AGG_FONT_SIZE" \
        --theme "$AGG_THEME" --fps-cap "$AGG_FPS" \
        --idle-time-limit "$AGG_IDLE" --speed "$render_speed" \
        "$cast" "$raw_gif"
    gifsicle -O3 --lossy="$GIFSICLE_LOSSY" --colors "$GIFSICLE_COLORS" \
        "$raw_gif" -o "$out_gif"

    ffmpeg -y -loglevel error -i "$out_gif" \
        -vf "fps=$AGG_FPS,scale=trunc(iw/2)*2:trunc(ih/2)*2" \
        -c:v libvpx-vp9 -b:v 0 -crf 34 -an "$out_webm"
    ffmpeg -y -loglevel error -i "$out_gif" \
        -vf "fps=$AGG_FPS,scale=trunc(iw/2)*2:trunc(ih/2)*2" \
        -movflags +faststart -pix_fmt yuv420p -c:v libx264 -crf 28 -an "$out_mp4"

    duration="$(ffprobe -v error -show_entries format=duration \
        -of default=nw=1:nk=1 "$out_gif")"
    if ! awk -v value="$duration" 'BEGIN { exit !(value < 30) }'; then
        echo "record-demo: GIF is ${duration}s; README hero must stay under 30s" >&2
        exit 1
    fi
    gif_bytes="$(wc -c <"$out_gif" | tr -d ' ')"
    if [ "$gif_bytes" -gt "$MAX_GIF_BYTES" ]; then
        echo "record-demo: GIF is $gif_bytes bytes; limit is $MAX_GIF_BYTES" >&2
        exit 1
    fi

    # Nine review frames plus an information-dense poster from the middle of the
    # real recording. Dividing by 11 keeps the last sample away from container
    # duration padding, where ffmpeg can succeed without emitting a frame.
    for i in $(seq 1 9); do
        timestamp="$(awk -v d="$duration" -v n="$i" 'BEGIN { printf "%.3f", d * n / 11 }')"
        ffmpeg -y -loglevel error -ss "$timestamp" -i "$out_gif" -frames:v 1 \
            "$(printf '%s/frames/frame-%02d.png' "$out" "$i")"
    done
    cp "$out/frames/frame-05.png" "$out_poster"

    ls -lh "$out_gif" "$out_webm" "$out_mp4" "$out_poster"
    printf 'record-demo: %.2fs · %s-byte GIF · real Codex in three AF worktrees\n' \
        "$duration" "$gif_bytes"
    exit 0
fi

# ---------------------------------------------------------------------------
# Host side — start the sanctioned play-test sandbox and copy only media out.
# ---------------------------------------------------------------------------
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
container_name="${AF_DEMO_NAME:-af-demo-$$}"
codex_release="${AF_PLAYTEST_CODEX_RELEASE:-0.147.0}"
auth_file="${AF_DEMO_CODEX_AUTH_FILE:-}"
frames_dir="${DEMO_FRAMES_DIR:-}"

if [ -z "$auth_file" ] || [ ! -f "$auth_file" ]; then
    echo "record-demo: set AF_DEMO_CODEX_AUTH_FILE to a readable Codex auth.json" >&2
    exit 2
fi

cleanup() {
    docker rm -f "$container_name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if docker container inspect "$container_name" >/dev/null 2>&1; then
    echo "record-demo: container '$container_name' already exists" >&2
    exit 2
fi

printf '>>> starting isolated real-agent play-test sandbox …\n'
AF_PLAYTEST_NAME="$container_name" \
AF_PLAYTEST_AGENT=codex \
AF_PLAYTEST_CODEX_RELEASE="$codex_release" \
    make -C "$repo_root" playtest-container-detached
docker exec "$container_name" sh -c \
    'until [ -f /home/dev/sandbox/playtest-ready ]; do sleep 1; done'

# Recording tools live only in the throwaway container. Installing them here
# keeps the recording on the exact `make playtest-container` path instead of a
# parallel demo harness.
printf '>>> installing the recording stack in the sandbox …\n'
docker exec --user root -e DEBIAN_FRONTEND=noninteractive "$container_name" \
    sh -c 'apt-get update >/dev/null && apt-get install -y --no-install-recommends asciinema ffmpeg gifsicle fonts-jetbrains-mono >/dev/null && rm -rf /var/lib/apt/lists/*'
docker exec --user root "$container_name" sh -c \
    'curl -fsSL -o /usr/local/bin/agg https://github.com/asciinema/agg/releases/download/v1.9.0/agg-x86_64-unknown-linux-musl && chmod +x /usr/local/bin/agg'

# Copy one credential file after the sandbox is fully scaffolded. It is never
# mounted into /src, copied back out, or retained after the container teardown.
docker exec --user root "$container_name" mkdir -p /home/dev/.codex
docker cp "$auth_file" "$container_name:/home/dev/.codex/auth.json" >/dev/null
docker exec --user root "$container_name" chown -R dev:dev /home/dev/.codex
docker exec "$container_name" codex login status >/dev/null

printf '>>> recording three real Codex sessions …\n'
docker exec \
    -e AF_DEMO_INNER=1 \
    -e "DEMO_COLS=$COLS" -e "DEMO_ROWS=$ROWS" \
    -e "DEMO_FONT_SIZE=$AGG_FONT_SIZE" -e "DEMO_THEME=$AGG_THEME" \
    -e "DEMO_FPS=$AGG_FPS" -e "DEMO_IDLE=$AGG_IDLE" \
    -e "DEMO_SPEED=$AGG_SPEED" -e "DEMO_TARGET_DURATION=$TARGET_DURATION" \
    -e "DEMO_LOSSY=$GIFSICLE_LOSSY" \
    -e "DEMO_COLORS=$GIFSICLE_COLORS" \
    -e "DEMO_MAX_GIF_BYTES=$MAX_GIF_BYTES" \
    "$container_name" bash /src/scripts/container/record-demo.sh

printf '>>> copying generated media …\n'
docker cp "$container_name:/home/dev/out/demo.gif" "$repo_root/docs/assets/demo.gif"
docker cp "$container_name:/home/dev/out/demo.webm" "$repo_root/docs/assets/demo.webm"
docker cp "$container_name:/home/dev/out/demo.mp4" "$repo_root/docs/assets/demo.mp4"
docker cp "$container_name:/home/dev/out/demo-poster.png" "$repo_root/docs/assets/demo-poster.png"
if [ -n "$frames_dir" ]; then
    mkdir -p "$frames_dir"
    docker cp "$container_name:/home/dev/out/frames/." "$frames_dir/"
fi

ls -lh \
    "$repo_root/docs/assets/demo.gif" \
    "$repo_root/docs/assets/demo.webm" \
    "$repo_root/docs/assets/demo.mp4" \
    "$repo_root/docs/assets/demo-poster.png"
printf '>>> done · container teardown removes the copied auth and sandbox\n'
