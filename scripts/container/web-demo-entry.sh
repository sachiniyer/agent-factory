#!/usr/bin/env bash
# Runs INSIDE the web-selftest container (see scripts/testbox.sh web-demo, or
# `make demo-assets`): build af from the mounted source, bring up a REAL af
# daemon on a throwaway home with a loopback HTTP listener, seed a demo-shaped
# project, then drive the embedded SPA in a headless Chromium via Playwright
# while recording video and stills (web/selftest/web-demo.spec.ts), and convert
# the recording into the media docs/ ships.
#
# It is the RECORDER, not a gate. Everything about the sandbox — the same image,
# the same daemon-on-a-throwaway-home shape, the same loopback tokenless browser
# — is borrowed from web-selftest-entry.sh so the demo shows the product the
# self-test asserts on. What it deliberately does NOT share is that script's
# fixture zoo: the gate seeds probe-a, probe-noserver, a dead port and a
# URL-less tab, all of which exist to make failures reachable and none of which
# belong in the picture on the README. So this file seeds its own small,
# plausible project instead, and CI never runs it.
#
# Everything here — the tmux server, the daemon, the AF home, the sessions, the
# browser — lives and dies with the container. Teardown is `docker rm -f`, not a
# checklist. Nothing touches the host tmux server or the real ~/.agent-factory.
set -euo pipefail

# --- writable working copy (the /src bind mount is read-only) ---------------
# shellcheck source=scripts/container/copy-src.sh
. /src/scripts/container/copy-src.sh
copy_src_tree /src /work --exclude=web/node_modules --exclude=web/test-results
cd /work

# The ONLY path out of the container: testbox.sh bind-mounts docs/assets/web
# here. Nothing is written into it until every conversion below has succeeded
# (the media is staged first), so a run that dies halfway leaves the committed
# assets exactly as it found them rather than half-replaced.
OUT=/work/demo-out
STAGE=/work/demo-stage
SHOTS=$STAGE/stills
VIDEO_DIR=$STAGE/video
MARKERS=/work/markers
mkdir -p "$STAGE" "$SHOTS" "$VIDEO_DIR" "$MARKERS"

# A real-looking HOME, because the Config view PRINTS the config path and the
# Accounts section prints each account's directory — and those strings end up in
# a screenshot on a user-facing docs page. `/work/afhome` is honest but reads
# like harness plumbing; a home directory reads like a machine, and af shortens
# a path under $HOME to `~` where it can. Everything else in the container is
# already pointed somewhere explicit (GOMODCACHE/GOCACHE by the image,
# PLAYWRIGHT_BROWSERS_PATH by the base image), so moving HOME only moves npm's
# cache, which is written fresh either way.
export HOME=/work/demo-home
HOME_DIR="$HOME/.agent-factory"
MOCK=/work/todo-cli
BIN=/work/bin/af
LISTEN=127.0.0.1:8899
BASE_URL="http://${LISTEN}"

# The seeded sessions. Names a reader can tell apart at a glance, and — the
# constraint the gate learned the hard way — no name is a substring of another,
# so a locator that filters a rail row by title can never match two.
SESSION_JSON=add-json-export
SESSION_USAGE=fix-empty-add
SESSION_DOCS=document-cli
# The session the RECORDING creates through the new-session modal. Seeded
# nowhere; it exists only because the demo made it.
SESSION_NEW=tidy-tests

# The recorded viewport. 1440x900 is a laptop, which is what the web client is
# for, and it is 16:10 — the same aspect the converted video and poster keep.
VIEW_W="${AF_DEMO_WIDTH:-1440}"
VIEW_H="${AF_DEMO_HEIGHT:-900}"
# Delivered media geometry and budgets. The mp4 is the README's target and the
# gif is its fallback, so both are capped and both caps are enforced below.
MEDIA_W="${AF_DEMO_MEDIA_WIDTH:-1280}"
TARGET_SECONDS="${AF_DEMO_TARGET_SECONDS:-32}"
MAX_MP4_BYTES="${AF_DEMO_MAX_MP4_BYTES:-8000000}"
MAX_GIF_BYTES="${AF_DEMO_MAX_GIF_BYTES:-4000000}"
MAX_SECONDS="${AF_DEMO_MAX_SECONDS:-60}"
# The geometry the seeded panes are given before recording — see the resize block
# below for why they need one. Comfortably wider and taller than tmux's 80x24
# default and close to what the recorded viewport's pane works out to, so nothing
# in the frame is wrapped by a width the recording does not have.
PANE_COLS="${AF_DEMO_PANE_COLS:-168}"
PANE_ROWS="${AF_DEMO_PANE_ROWS:-44}"

export AGENT_FACTORY_HOME="$HOME_DIR"
# Belt and braces beside the stand-in's own default: if this does survive into
# the pane, it names the same directory.
export AF_DEMO_MARKER_DIR="$MARKERS"
# A container binary is built at the branch version; without this it would
# self-update on boot and restart the daemon mid-run (#1596).
export AGENT_FACTORY_AUTO_UPDATE=false
mkdir -p "$HOME" "$HOME_DIR" /work/bin

echo ">>> building af from /work ..."
go build -buildvcs=false -o "$BIN" .

# --- the mock project -------------------------------------------------------
# A real git repo with a real program and a real test script, so every edit the
# stand-in agent makes is a real edit and the diff the demo shows is real. It is
# deliberately tiny: the point of the frame is the web client around it.
echo ">>> creating the mock project at $MOCK ..."
mkdir -p "$MOCK"
(
    cd "$MOCK"
    git init -q -b master
    cat >todo.sh <<'EOF'
#!/bin/bash
# todo — a tiny shell todo list: list, add <text>, done <n>
TODO_FILE="${TODO_FILE:-todo.txt}"
touch "$TODO_FILE"
case "$1" in
  add) shift; printf '%s\n' "$*" >>"$TODO_FILE" ;;
  done) shift; sed -i "${1}d" "$TODO_FILE" ;;
  *) nl -ba "$TODO_FILE" ;;
esac
EOF
    cat >test.sh <<'EOF'
#!/bin/bash
set -e
TODO_FILE="$(mktemp)"
export TODO_FILE
trap 'rm -f "$TODO_FILE"' EXIT

./todo.sh add "write the release notes"
./todo.sh | grep -q "write the release notes"
echo "ok · add and list"
EOF
    cat >README.md <<'EOF'
# todo-cli

A tiny shell todo list. It is the sandbox project the Agent Factory web demo
is recorded against — see docs/demo-assets.md.
EOF
    chmod +x todo.sh test.sh
    git add -A
    git commit -qm "todo-cli: list, add and done"
)

# --- the stand-in agent -----------------------------------------------------
# Committed and reviewable (scripts/container/web-demo-agent.sh); its header
# carries the reasoning for why the demo runs a stand-in at all.
DEMO_AGENT=/work/bin/demo-agent
cp /work/scripts/container/web-demo-agent.sh "$DEMO_AGENT"
chmod +x "$DEMO_AGENT"
chmod 0777 "$MARKERS"

# --- a stand-in `gh`, so the PR badge has something to discover -------------
# The daemon is the sole producer of the pr_info projection and it produces it
# by running `gh pr list` in the session's repo (session/git/github.go). The
# sandbox has no GitHub remote and no network, so without a stand-in the demo
# could never show the badge — one of the few places the web client puts the
# normal review path in front of you.
#
# It answers for ONE branch, so exactly one session carries a badge, which is
# also the honest picture: you open a PR when the work is ready, not on create.
# The URL points at an org that does not exist; nothing in the recording is a
# link to somebody's real pull request.
cat >/usr/local/bin/gh <<EOF
#!/bin/sh
# Stand-in for the GitHub CLI inside the demo sandbox. See web-demo-entry.sh.
case "\$*" in
    *"pr list"*"$SESSION_JSON"*)
        printf '%s\n' '[{"number":128,"title":"Add a json command to todo.sh","url":"https://github.com/agent-factory-demo/todo-cli/pull/128","state":"OPEN"}]'
        ;;
    *"pr list"*)
        printf '%s\n' '[]'
        ;;
    *)
        echo "demo gh stand-in: unsupported command: \$*" >&2
        exit 1
        ;;
esac
EOF
chmod +x /usr/local/bin/gh

# --- the review tab's command ----------------------------------------------
# A process tab that prints the worktree's own diff and then holds the pane
# open. This is an ordinary AF process tab running an ordinary git command in
# the session's worktree — the web client has no diff view of its own, and
# inventing one for a recording would be showing a product that does not exist.
cat >/usr/local/bin/demo-diff <<'EOF'
#!/usr/bin/env bash
# Runs in an AF-owned worktree as a process tab; see web-demo-entry.sh.
#
# The line budget is load-bearing, not tidiness. This pane is a couple of dozen
# rows in the recorded viewport, and a `git diff` that overflows it scrolls the
# summary off the top — so the frame opens on the middle of a hunk with no idea
# what it is looking at. Everything below fits in 19 lines, and the remainder is
# COUNTED rather than silently dropped.
#
# The SIGWINCH redraw is for the same reason web-demo-agent.sh has one: this runs
# while the session is being seeded, so its output is emitted into tmux's default
# 80-column pane and lands in scrollback wrapped at 80. tmux does not reflow
# history, so without the repaint the recording shows an 80-column diff inside a
# much wider pane.
DIFF_LINES=10
full="$(mktemp)"
git --no-pager diff >"$full"
total="$(wc -l <"$full" | tr -d ' ')"
render() {
    printf '$ git diff --stat\n'
    git --no-pager diff --stat
    printf '\n$ git diff\n'
    head -n "$DIFF_LINES" "$full"
    if [ "$total" -gt "$DIFF_LINES" ]; then
        printf '… %s more lines\n' "$((total - DIFF_LINES))"
    fi
    printf '\n· the agent works on its own branch · review it like any other\n'
}
body="$(render)"
printf '%s\n' "$body"
trap 'printf "\033[2J\033[H%s\n" "$body"' WINCH
# `sleep` in the foreground would swallow the signal until it returned; bash runs
# a trap while it is blocked in `wait`, so the repaint is immediate.
while :; do
    sleep 3600 &
    wait $!
done
EOF
chmod +x /usr/local/bin/demo-diff

# --- the throwaway AF home --------------------------------------------------
# branch_prefix is pinned so the branch names in the recording are a property of
# this script rather than of whichever user the container happens to run as.
cat >"$HOME_DIR/config.json" <<EOF
{
  "default_program": "claude",
  "program_overrides": { "claude": "$DEMO_AGENT" },
  "branch_prefix": "demo/",
  "listen_addr": "$LISTEN"
}
EOF

# --- credential accounts for the Config view's Accounts section -------------
# Registered before the daemon starts: an account is a directory under the
# throwaway home and `af accounts add` writes it directly. Two states so the
# section has both to render.
#
# NOTHING HERE IS CREDENTIAL MATERIAL. `logged_in` is a stat of a path af never
# opens, so an empty-of-secrets `{}` is exactly as much as this needs to make
# one row report "logged in" and the other not.
"$BIN" accounts add claude personal >/dev/null
"$BIN" accounts add claude work >/dev/null
printf '{}\n' >"$HOME_DIR/accounts/claude/personal/.credentials.json"

start_daemon() {
    "$BIN" --daemon >>/work/daemon.log 2>&1 &
    DAEMON_PID=$!
}

wait_for_listener() {
    for _ in $(seq 1 60); do
        if curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/" 2>/dev/null | grep -q '^200$'; then
            return 0
        fi
        if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
            echo "daemon exited before binding the listener; see log:" >&2
            cat /work/daemon.log >&2
            exit 1
        fi
        sleep 1
    done
    echo "timed out waiting for $BASE_URL" >&2
    cat /work/daemon.log >&2
    exit 1
}

echo ">>> starting af daemon (listen_addr=$LISTEN) ..."
start_daemon

cleanup() {
    rc=$?
    echo ">>> tearing down (rc=$rc) ..."
    for s in "$SESSION_JSON" "$SESSION_USAGE" "$SESSION_DOCS" "$SESSION_NEW"; do
        "$BIN" sessions kill "$s" >/dev/null 2>&1 || true
    done
    kill "$DAEMON_PID" >/dev/null 2>&1 || true
    for _ in $(seq 1 50); do
        kill -0 "$DAEMON_PID" 2>/dev/null || break
        sleep 0.1
    done
    if [ "$rc" -ne 0 ]; then
        echo "===== agent-factory.log (tail) =====" >&2
        tail -n 60 "$HOME_DIR/agent-factory.log" >&2 || true
    fi
    # This container runs as root and writes into a bind mount inside the
    # developer's checkout, so hand the media back to whoever owns /src. Purely
    # best-effort: rc is already decided.
    chown -R "$(stat -c '%u:%g' /src)" "$OUT" 2>/dev/null || true
}
trap cleanup EXIT

echo ">>> waiting for the HTTP listener ..."
wait_for_listener
echo ">>> daemon up at $BASE_URL (loopback ⇒ the browser needs no token)"

# --- seed the project's three in-flight sessions ----------------------------
# --prompt is what makes the pane show a prompt arriving the way a real one
# does: the daemon types it in once the workspace reports ready, and the
# stand-in reads it off its own tty.
seed() {
    local name=$1 prompt=$2
    echo ">>> creating session $name ..."
    "$BIN" sessions create --repo "$MOCK" --name "$name" --program claude --prompt "$prompt" >/dev/null
}
seed "$SESSION_JSON" "Add a json command to todo.sh that prints the items as a JSON array."
seed "$SESSION_USAGE" "Make ./todo.sh add with no text exit nonzero with a usage line, and cover it."
seed "$SESSION_DOCS" "Document list, add and done in README.md, accurately to todo.sh."

# Wait for the scripted work to FINISH rather than for a duration: each role
# drops its own marker as its last act, so the recording opens on three
# sessions that have something to show instead of three blank panes.
echo ">>> waiting for the seeded agents to finish their scripted work ..."
for _ in $(seq 1 120); do
    if [ -f "$MARKERS/json.done" ] && [ -f "$MARKERS/usage.done" ] && [ -f "$MARKERS/docs.done" ]; then
        break
    fi
    sleep 1
done
for m in json usage docs; do
    if [ ! -f "$MARKERS/$m.done" ]; then
        echo "FATAL: the $m stand-in never finished; the demo would record an empty pane" >&2
        # The pane itself, not just the log: the log says whether af launched
        # something, while only the pane says what that something printed —
        # which is where a broken stand-in actually reports itself.
        for s in "$SESSION_JSON" "$SESSION_USAGE" "$SESSION_DOCS"; do
            echo "----- $s -----" >&2
            "$BIN" sessions preview "$s" --repo "$MOCK" >&2 2>&1 || true
        done
        tail -n 40 "$HOME_DIR/agent-factory.log" >&2 || true
        exit 1
    fi
done

# The review tab, on the session that also carries the PR badge, so one frame
# shows the whole review path: the branch's diff beside a link to its PR.
"$BIN" sessions tab-create --repo "$MOCK" "$SESSION_JSON" --command demo-diff --name diff >/dev/null

# --- scheduled tasks --------------------------------------------------------
# Both are cron tasks whose next occurrence is half a day out, computed rather
# than fixed: a schedule that fell inside the recording would really fire, spawn
# a session nobody asked for, and put it in the frame (#3626 learned this the
# hard way). Local hour, because a schedule with no zone is evaluated in the
# location of the clock handed to it — the daemon's.
LATER_HOUR="$(((10#$(date +%-H) + 12) % 24))"
echo ">>> seeding scheduled tasks ..."
"$BIN" tasks add --repo "$MOCK" --name nightly-tests \
    --cron "0 $LATER_HOUR * * *" \
    --prompt "Run ./test.sh on master and open a session if it is red." >/dev/null
"$BIN" tasks add --repo "$MOCK" --name weekly-dependency-sweep \
    --cron "30 $LATER_HOUR * * 1" \
    --prompt "Check the project's dependencies and summarize what is behind." >/dev/null

# --- wait for the PR badge --------------------------------------------------
# The daemon's PR sweep runs once a minute, so the badge lands on its own — but
# only once. Waiting for it here rather than in the browser keeps the recording
# free of a minute of nothing happening.
echo ">>> waiting for the daemon's PR sweep to discover the stand-in PR ..."
for _ in $(seq 1 150); do
    if "$BIN" sessions get "$SESSION_JSON" --repo "$MOCK" 2>/dev/null | grep -q 'pull/128'; then
        break
    fi
    sleep 1
done
if ! "$BIN" sessions get "$SESSION_JSON" --repo "$MOCK" 2>/dev/null | grep -q 'pull/128'; then
    echo "FATAL: no pr_info on $SESSION_JSON; the review beat would show no PR badge" >&2
    exit 1
fi

# --- size the seeded panes --------------------------------------------------
# A tmux pane opens at 80x24 when nothing is attached, and every pane above was
# created while the session was being SEEDED — minutes before a browser existed.
# The recorded viewport's pane is far wider than that, so without this the frame
# shows an 80-column transcript sitting inside it: an artifact of WHEN the
# fixture ran, not of the product. (The session the demo creates on camera is
# created with a client already attached and is sized correctly on its own —
# which is what makes the difference visible, and is why this is worth doing
# rather than shrugging at.)
#
# It reflows rather than merely widening the window under the old text: both
# stand-ins repaint on SIGWINCH (web-demo-agent.sh, and demo-diff above), and
# tmux reflows its own history on a width change.
#
# This is the container's own tmux server, which lives and dies with it.
echo ">>> sizing the seeded panes to ${PANE_COLS}x${PANE_ROWS} ..."
for w in $(tmux list-sessions -F '#{session_name}' 2>/dev/null); do
    tmux set-option -t "$w" window-size manual >/dev/null 2>&1 || true
    tmux resize-window -t "$w" -x "$PANE_COLS" -y "$PANE_ROWS" >/dev/null 2>&1 || true
done
# The repaint is a signal handler, so give it a moment to land before the browser
# starts photographing panes.
sleep 2

# --- record -----------------------------------------------------------------
echo ">>> installing web deps + recording the demo ..."
cd /work/web
export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
npm ci --no-audit --no-fund
# Whatever Playwright writes lands in the bind mount as root; a permissive umask
# keeps it removable even when the trap above never runs (a SIGKILLed container
# runs no trap at all).
umask 000

export AF_WEB_BASE_URL="$BASE_URL"
export AF_DEMO_SHOT_DIR="$SHOTS"
export AF_DEMO_VIDEO_DIR="$VIDEO_DIR"
export AF_DEMO_SESSION_JSON="$SESSION_JSON"
export AF_DEMO_SESSION_USAGE="$SESSION_USAGE"
export AF_DEMO_SESSION_DOCS="$SESSION_DOCS"
export AF_DEMO_SESSION_NEW="$SESSION_NEW"
export AF_DEMO_WIDTH="$VIEW_W"
export AF_DEMO_HEIGHT="$VIEW_H"
npx playwright test --config=playwright.demo.config.ts

# The spec saves the default-theme pass's recording under this exact name
# (Playwright's own file is a random hash, and the directory also holds the
# original), so name it rather than globbing and picking whichever `find`
# returned first.
RAW="$VIDEO_DIR/demo-raw.webm"
if [ ! -s "$RAW" ]; then
    echo "FATAL: the recorder wrote no video at $RAW" >&2
    ls -l "$VIDEO_DIR" >&2 || true
    exit 1
fi

# --- convert ----------------------------------------------------------------
cd /work
echo ">>> converting $(basename "$RAW") ..."
raw_seconds="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$RAW" || true)"
case "$raw_seconds" in
    '' | N/A)
        # A webm whose container header carries no duration would otherwise
        # reach awk as 0 and fail there, with a message about arithmetic rather
        # than about the file.
        echo "FATAL: ffprobe read no duration from $RAW" >&2
        exit 1
        ;;
esac
# The recording is as long as the flows take, which is longer than anyone will
# watch. Speed is derived from the measured length rather than pinned, so the
# delivered video lands near TARGET_SECONDS whatever the box was doing that day
# — and never slows a short recording down.
speed="$(awk -v d="$raw_seconds" -v t="$TARGET_SECONDS" \
    'BEGIN { if (d <= 0 || t <= 0) exit 1; s = d / t; if (s < 1) s = 1; printf "%.4f", s }')"
echo ">>> raw ${raw_seconds}s · speed ${speed}x · target ${TARGET_SECONDS}s"

SCALE="setpts=PTS/$speed,scale=$MEDIA_W:-2:flags=lanczos"
# -cpu-used 3 is not a detail. libvpx-vp9 at its default effort spent more than
# five minutes on a 24-second screencast on a busy box and was still on its
# first pass; at 3 the same clip converts in seconds, and the difference on flat
# UI panels — which is nearly all of this frame — is not visible at 1280 wide.
# A target a maintainer will actually run has to finish while they watch it.
ffmpeg -y -loglevel error -i "$RAW" -vf "$SCALE" -r 24 \
    -c:v libvpx-vp9 -b:v 0 -crf 36 -deadline good -cpu-used 3 -row-mt 1 \
    -an "$STAGE/demo.webm"
ffmpeg -y -loglevel error -i "$RAW" -vf "$SCALE,format=yuv420p" -r 24 \
    -movflags +faststart -c:v libx264 -preset medium -crf 28 -an "$STAGE/demo.mp4"

# The GIF is the fallback for readers whose renderer will not play a video, so
# it is allowed to be coarse — but it is NOT allowed to be enormous. Step down
# through width/fps/palette until it fits, and fail loudly rather than shipping
# a 20MB file into the repo.
make_gif() {
    local w=$1 fps=$2 colors=$3
    local vf="setpts=PTS/$speed,fps=$fps,scale=$w:-1:flags=lanczos"
    ffmpeg -y -loglevel error -i "$RAW" \
        -vf "$vf,palettegen=max_colors=$colors:stats_mode=diff" "$STAGE/palette.png"
    ffmpeg -y -loglevel error -i "$RAW" -i "$STAGE/palette.png" \
        -lavfi "${vf}[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=5" \
        -loop 0 "$STAGE/demo.gif"
}
gif_ok=no
# The first tier is where a normal run should land with room to spare, not the
# ceiling. GIF size follows frame-to-frame motion, so the same six beats have
# measured anywhere from 2.6MB to 5.6MB across runs at 960/12/128 — a first tier
# that only just fits means the next recording silently drops a tier, or worse,
# commits something enormous the day the cap moves.
for tier in "800 10 96" "720 10 72" "640 8 64" "560 8 48"; do
    # shellcheck disable=SC2086
    make_gif $tier
    bytes="$(wc -c <"$STAGE/demo.gif" | tr -d ' ')"
    echo ">>> gif · ${tier// / · } → $bytes bytes"
    if [ "$bytes" -le "$MAX_GIF_BYTES" ]; then
        gif_ok=yes
        break
    fi
done
if [ "$gif_ok" != yes ]; then
    echo "FATAL: the GIF stayed above $MAX_GIF_BYTES bytes at every tier" >&2
    exit 1
fi

# The poster is the dashboard still rather than a frame pulled back out of the
# video: same viewport, same aspect, and none of the compression the video went
# through. It is the frame a README reader sees before anything plays.
ffmpeg -y -loglevel error -i "$SHOTS/dashboard.png" \
    -vf "scale=$MEDIA_W:-2:flags=lanczos" "$STAGE/demo-poster.png"

# --- the budgets, enforced --------------------------------------------------
mp4_bytes="$(wc -c <"$STAGE/demo.mp4" | tr -d ' ')"
mp4_seconds="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "$STAGE/demo.mp4")"
if [ "$mp4_bytes" -gt "$MAX_MP4_BYTES" ]; then
    echo "FATAL: demo.mp4 is $mp4_bytes bytes; the limit is $MAX_MP4_BYTES" >&2
    exit 1
fi
if ! awk -v v="$mp4_seconds" -v m="$MAX_SECONDS" 'BEGIN { exit !(v <= m) }'; then
    echo "FATAL: demo.mp4 is ${mp4_seconds}s; the limit is ${MAX_SECONDS}s" >&2
    exit 1
fi

# --- publish ----------------------------------------------------------------
# Only now, once every conversion and budget has passed.
echo ">>> writing $OUT ..."
mkdir -p "$OUT"
cp "$STAGE/demo.webm" "$STAGE/demo.mp4" "$STAGE/demo.gif" "$STAGE/demo-poster.png" "$OUT/"
cp "$SHOTS"/*.png "$OUT/"
chmod 0644 "$OUT"/*
ls -l "$OUT"
printf '>>> done · %ss mp4 (%s bytes) · %s stills\n' \
    "$mp4_seconds" "$mp4_bytes" "$(find "$SHOTS" -name '*.png' | wc -l)"
