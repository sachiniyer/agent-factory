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
# idle-time-limit caps *static* gaps. It must stay ABOVE the scenario's
# deliberate "hold" beats (the automations overlay, the full-screen attach, the
# finale) — those are held on a still screen, so a lower cap would trim the very
# shots the demo is meant to linger on (e.g. the overlay collapsing to a
# ~0.3s flash). 3.0 preserves every hold; --speed then does the global compress.
AGG_IDLE="${DEMO_IDLE:-3.0}"           # preserve deliberate holds; still trims dead air
AGG_SPEED="${DEMO_SPEED:-2.2}"         # playback speed-up (keeps the richer flow ~22-28s)
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

    # A tab is a real process in the agent's worktree, not another agent. Put a
    # fake dev-server (demo-tab.sh) on PATH as `dev` so the demo can open a
    # second tab beside an agent and stream `npm run dev`-style output; and a
    # long-lived `watch-ci` stub so the seeded watch task shows [watching], not
    # a "command not found" error (see the Automations seed below).
    DEV_BIN=/home/dev/bin
    mkdir -p "$DEV_BIN"
    cp /src/docs/assets/demo-tab.sh "$DEV_BIN/dev"
    chmod +x "$DEV_BIN/dev"
    printf '#!/usr/bin/env bash\nexec sleep infinity\n' >"$DEV_BIN/watch-ci"
    chmod +x "$DEV_BIN/watch-ci"

    # shellcheck disable=SC1091
    source /src/scripts/tui-driver.sh
    af_reset_sandbox

    # Seed the Automations rail with a believable schedule (a cron "triage every
    # morning", a nightly test run, and a watch task) so the demo shows real
    # scheduling, not an empty section. Written AFTER af_reset_sandbox (which
    # wipes the sandbox home) and BEFORE af_boot (the TUI reads tasks.json from
    # disk at startup — #960 the daemon is the only *writer*, but reads are from
    # disk, and the demo runs no daemon). ProjectPath must equal the mock-repo
    # root so LoadTasksForCurrentRepo() surfaces them.
    REPO_ROOT="$(git -C "$AF_DRIVER_REPO" rev-parse --show-toplevel 2>/dev/null || echo "$AF_DRIVER_REPO")"
    cat >"$AGENT_FACTORY_HOME/tasks.json" <<EOF
[
 {"id":"a1a1a1a1","name":"Triage new issues","prompt":"Triage new GitHub issues: label, dedupe, and reply","cron_expr":"0 9 * * *","project_path":"$REPO_ROOT","program":"claude","enabled":true,"created_at":"2026-07-01T09:00:00Z"},
 {"id":"b2b2b2b2","name":"Nightly test + coverage","prompt":"Run the full suite and post the coverage delta","cron_expr":"0 2 * * *","project_path":"$REPO_ROOT","program":"claude","enabled":true,"created_at":"2026-07-01T09:00:00Z"},
 {"id":"c3c3c3c3","name":"Rerun failing CI","prompt":"A CI run failed: {{line}} — reproduce and fix","watch_cmd":"watch-ci","project_path":"$REPO_ROOT","program":"claude","enabled":true,"created_at":"2026-07-01T09:00:00Z"}
]
EOF

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
    # Surface-width note: an agent's transcript is pre-streamed at native
    # (full-preview / full-screen) width, so re-opening it in a narrower tiled
    # pane reflows and mangles it. The scenario therefore only ever shows the
    # AGENT transcript on those two native surfaces — and reserves the tiled
    # split (Act 4) for a *tab* whose content (the dev-server) is generated
    # live at the pane's current width, so it renders clean.
    #
    # Act 1 — create one agent and watch it work (the hero streaming shot).
    af_new_instance fix-auth-timeout
    beat 2.2
    # Act 2 — spin up the fleet; the sidebar fills with agents, a running
    # spinner on the newest and ● ready dots on the rest, while the live
    # preview keeps streaming. The Automations rail sits seeded below.
    af_new_instance add-dark-mode;            beat 0.9
    af_new_instance refactor-api-client;      beat 1.4
    # Act 3 — spotlight the Automations: open the task manager (S) over the
    # fleet to show the cron/watch schedule ("triage every morning", nightly
    # tests, rerun-failing-CI), then dismiss it (a modal overlay: clean open +
    # Escape close, no pane-focus state to strand).
    af_open_tasks
    beat 2.6
    af_close_tasks
    beat 0.6
    # Act 4 — create the last agent, let its (fuller) transcript finish
    # streaming while the fleet sits ready, then dive full-screen into it (o).
    # Attaching only AFTER it settles matters: a settled pane renders clean and
    # fills the screen, where mid-stream would strand a reflowing pane. No
    # typing, so no interactive-mode toggle strands the menu bar; af_attach
    # syncs on the TUI chrome vanishing, so the recording lands on the fully-
    # attached state.
    af_new_instance write-integration-tests
    beat 3.0                                     # fleet-ready beat + transcript settles
    af_attach
    beat 2.2                                     # hold on the clean, full agent session
    af_detach
    beat 0.9
    # Act 5 (finale) — a tab is a real process in the worktree, not another
    # agent. Open a second tab on the first agent (t auto-tiles a
    # Preview | Terminal split), step in (Enter → interact in-pane, no full-
    # screen), and run the dev server — it streams live beside the agent's own
    # transcript. This is the closing shot: an agent and a running dev-server
    # side by side in one worktree. Ending here (rather than tearing the split
    # back down) keeps the scenario to only the clean surfaces — the fragile
    # part was collapsing a two-tab pane back to the tree, which we now avoid.
    af_select fix-auth-timeout
    af_new_tab                                  # auto-opens the tiled Terminal split
    af_enter_interactive
    af_send_to_pane "dev"
    beat 3.6                                     # dev-server boots + serves requests
    af_exit_interactive                          # both panes back to the calm single border
    beat 1.6

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
    for f in $(seq 1 9); do
        n=$(( (total * f) / 10 ))
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
