#!/usr/bin/env bash
# record-demo.sh — regenerate the README demo GIF (docs/assets/demo.gif, #1032)
# entirely inside a throwaway container. Never runs af, tmux, or a recorder on
# the host.
#
#   Regenerate the GIF (one command):
#       scripts/container/record-demo.sh
#
# What it does, all inside an `agent-factory-demo` container (built from
# scripts/container/Dockerfile.demo):
#   1. build af + the play-test sandbox (scripts/container/playtest-entry.sh);
#   2. point the sandbox's fake agent at docs/assets/demo-agent.sh so every
#      instance's pane streams a realistic "agent working" transcript;
#   3. drive the real af TUI through scripts/tui-driver.sh — marker-synchronized,
#      so it never mis-drives af's stateful focus model the way blind key-timing
#      would (that is why this uses the driver, not a blind vhs .tape);
#   4. record a read-only tmux mirror of the driven session with asciinema, at a
#      pinned 140x36 geometry, so the .cast is exactly what the TUI showed;
#   5. render the .cast to GIF with agg, optimize with gifsicle, and pull sample
#      frames with ffmpeg.
#
# The GIF and frames are copied back to the host under docs/assets/ (and, if
# DEMO_FRAMES_DIR is set, sample PNG frames there too).
set -euo pipefail

# ---- geometry / render knobs (override via env) ----------------------------
COLS="${DEMO_COLS:-140}"
ROWS="${DEMO_ROWS:-36}"
AGG_FONT_SIZE="${DEMO_FONT_SIZE:-14}"
AGG_THEME="${DEMO_THEME:-dracula}"
AGG_FPS="${DEMO_FPS:-15}"
AGG_IDLE="${DEMO_IDLE:-1.5}"           # trim static gaps; animation is untouched
AGG_SPEED="${DEMO_SPEED:-1.9}"         # playback speed-up (keeps GIF ~15-20s)
GIFSICLE_LOSSY="${DEMO_LOSSY:-80}"
GIFSICLE_COLORS="${DEMO_COLORS:-64}"

# ===========================================================================
# CONTAINER SIDE — runs inside the demo container (re-exec of this same file).
# ===========================================================================
if [ "${AF_DEMO_INNER:-}" = "1" ]; then
    export AF_DRIVER_COLS="$COLS" AF_DRIVER_ROWS="$ROWS" AF_DRIVER_BIN=/home/dev/bin/af
    SANDBOX="$HOME/sandbox"
    export AGENT_FACTORY_HOME="$SANDBOX/home"
    OUT=/home/dev/out
    CAST=/tmp/demo.cast
    RAW_GIF=/tmp/demo-raw.gif
    mkdir -p "$OUT/frames"

    # Fake coding-agent transcript in place of a bare shell (see demo-agent.sh).
    cp /src/docs/assets/demo-agent.sh "$SANDBOX/demo-agent.sh"
    chmod +x "$SANDBOX/demo-agent.sh"
    cat >"$AGENT_FACTORY_HOME/config.toml" <<EOF
default_program = "claude"

[program_overrides]
claude = "$SANDBOX/demo-agent.sh"
EOF

    # shellcheck disable=SC1091
    source /src/scripts/tui-driver.sh
    af_reset_sandbox
    af_boot                                   # af, empty TUI, in tmux session 'drive'
    # Hide every tmux status bar (the driver's 'drive' session AND each
    # instance's own af_* session) so the recording shows only af's chrome, not
    # a leaking green tmux bar. Global default -> new instance sessions inherit.
    tmux set-option -g status off

    # ---- start the recorder: a read-only mirror of 'drive' inside a pinned
    # ---- 140x36 'rec' session, so asciinema's pty matches the TUI exactly.
    tmux kill-session -t rec 2>/dev/null || true
    tmux new-session -d -s rec -x "$COLS" -y "$ROWS"
    tmux set-option -t rec window-size manual >/dev/null 2>&1 || true
    tmux resize-window -t rec -x "$COLS" -y "$ROWS" 2>/dev/null || true
    tmux send-keys -t rec \
        "asciinema rec --overwrite -c 'env TMUX= tmux attach -t drive -r' $CAST" Enter
    # Wait until the mirror client is actually attached to 'drive'.
    for _ in $(seq 1 50); do
        tmux list-clients -t drive 2>/dev/null | grep -q read-only && break
        sleep 0.2
    done
    sleep 0.8                                  # a beat of empty TUI on screen

    beat() { sleep "$1"; }

    # ---- the demo scenario -------------------------------------------------
    # Only auto-opened (on-creation) previews and full-screen attach render af's
    # panes at their native width; re-opening/tiling a pane reflows it to a
    # narrower width and mangles the transcript, so the scenario deliberately
    # sticks to those two clean surfaces.
    #
    # Act 1 — create one agent and watch it work (the hero streaming shot).
    af_new_instance fix-auth-timeout
    beat 2.5
    # Act 2 — spin up the fleet; the sidebar fills with agents + ready dots
    # while the live preview keeps showing the first agent's result.
    af_new_instance add-dark-mode;            beat 1.0
    af_new_instance refactor-api-client;      beat 1.0
    # Act 3 — create the last agent, let its (fuller) transcript finish
    # streaming in the background while the fleet sits ready, then dive
    # full-screen into it. Attaching only AFTER it settles matters: attaching
    # mid-stream leaves the pane transiently reflowing (content jumps, menu
    # strands), whereas a settled pane renders clean and fills the screen. No
    # typing, so there is no interactive-mode toggle to strand af's menu bar;
    # af_attach syncs on the TUI chrome vanishing, so the recording only lands
    # on the fully-attached state.
    af_new_instance write-integration-tests
    beat 3.5                                    # fleet-ready beat + transcript settles
    af_attach
    beat 2.2                                    # hold on the clean, full agent session
    af_detach
    beat 1.2

    # ---- stop recording cleanly: kill asciinema (which saves the cast). Do
    # NOT `tmux detach-client`, which would paint a "[detached]" frame into the
    # final GIF.
    pkill -INT -f 'asciinema rec' 2>/dev/null || true
    for _ in $(seq 1 50); do
        pgrep -f 'asciinema rec' >/dev/null || break
        sleep 0.3
    done
    [ -s "$CAST" ] || { echo "record-demo: cast not written" >&2; exit 1; }

    # ---- render + optimize -------------------------------------------------
    # --speed compresses the wall-clock pacing (a constantly-animating sidebar
    # spinner means there are no idle gaps for --idle-time-limit to trim, so
    # speed is what keeps the GIF in the ~15-20s README range).
    agg --font-family "JetBrains Mono" --font-size "$AGG_FONT_SIZE" \
        --theme "$AGG_THEME" --fps-cap "$AGG_FPS" --idle-time-limit "$AGG_IDLE" \
        --speed "$AGG_SPEED" \
        "$CAST" "$RAW_GIF"
    gifsicle -O3 --lossy="$GIFSICLE_LOSSY" --colors "$GIFSICLE_COLORS" \
        "$RAW_GIF" -o "$OUT/demo.gif"

    # ---- sample frames for review -----------------------------------------
    ffmpeg -y -loglevel error -i "$OUT/demo.gif" \
        -vf "select='not(mod(n\,(max(1\,floor(t/1))+0)))'" -vsync vfr /dev/null 2>/dev/null || true
    # 6 evenly spaced frames.
    total="$(ffmpeg -i "$OUT/demo.gif" -map 0:v:0 -c copy -f null /dev/null 2>&1 | grep -oE 'frame= *[0-9]+' | tail -1 | grep -oE '[0-9]+')"
    total="${total:-60}"
    i=1
    for f in $(seq 1 6); do
        n=$(( (total * f) / 7 ))
        [ "$n" -lt 1 ] && n=1
        ffmpeg -y -loglevel error -i "$OUT/demo.gif" -vf "select=eq(n\,$n)" -vframes 1 \
            "$(printf "%s/frames/frame-%02d.png" "$OUT" "$i")"
        i=$((i+1))
    done

    ls -l "$OUT/demo.gif"
    echo "record-demo(inner): done"
    exit 0
fi

# ===========================================================================
# HOST SIDE — orchestrate the container.
# ===========================================================================
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE="${AF_DEMO_IMAGE:-agent-factory-demo}"
NAME="${AF_DEMO_NAME:-af-demo-$$}"
OUT_GIF="$REPO_ROOT/docs/assets/demo.gif"
FRAMES_DIR="${DEMO_FRAMES_DIR:-}"

engine() { docker "$@"; }
cleanup() { engine rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

if ! engine image inspect "$IMAGE" >/dev/null 2>&1; then
    echo ">>> building $IMAGE ..."
    engine build -t "$IMAGE" - <"$REPO_ROOT/scripts/container/Dockerfile.demo"
fi

echo ">>> starting container $NAME ..."
engine run -d --rm --init \
    -v "$REPO_ROOT":/src:ro \
    --pids-limit 1024 --memory 8g \
    --name "$NAME" \
    -e AGENT_FACTORY_HOME=/home/dev/sandbox/home \
    "$IMAGE" bash /src/scripts/container/playtest-entry.sh hold >/dev/null
engine exec "$NAME" sh -c 'until [ -x /home/dev/bin/af ]; do sleep 1; done'

echo ">>> recording ..."
engine exec \
    -e AF_DEMO_INNER=1 \
    -e "DEMO_COLS=$COLS" -e "DEMO_ROWS=$ROWS" \
    -e "DEMO_FONT_SIZE=$AGG_FONT_SIZE" -e "DEMO_THEME=$AGG_THEME" \
    -e "DEMO_FPS=$AGG_FPS" -e "DEMO_IDLE=$AGG_IDLE" -e "DEMO_SPEED=$AGG_SPEED" \
    -e "DEMO_LOSSY=$GIFSICLE_LOSSY" -e "DEMO_COLORS=$GIFSICLE_COLORS" \
    "$NAME" bash /src/scripts/container/record-demo.sh

echo ">>> copying artifacts out ..."
mkdir -p "$(dirname "$OUT_GIF")"
engine cp "$NAME:/home/dev/out/demo.gif" "$OUT_GIF"
if [ -n "$FRAMES_DIR" ]; then
    mkdir -p "$FRAMES_DIR"
    engine cp "$NAME:/home/dev/out/frames/." "$FRAMES_DIR/"
fi

ls -lh "$OUT_GIF"
echo ">>> done: $OUT_GIF"
