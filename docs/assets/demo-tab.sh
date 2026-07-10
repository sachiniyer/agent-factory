#!/usr/bin/env bash
# demo-tab.sh — a FAKE dev-server transcript, used only when recording the
# README demo GIF (#1246). record-demo.sh installs this on PATH as `dev` inside
# the demo container, so the demo can open a *second tab* in an agent's
# worktree (the `t` shell tab) and stream a believable "npm run dev" process
# running alongside the agent — showing that a tab is a real process in the
# same worktree, not just another agent. It is NOT part of `af`.
#
# Two rendering rules keep this clean in the recording:
#   * Everything is generated live at the pane's current (tiled, ~half) width —
#     short lines, no pre-wrapped content — so it renders clean in the narrower
#     workspace pane the way the agent transcript (pre-streamed at native
#     width) would not.
#   * ASCII-only markers. The tab is a raw passthrough terminal (unlike the
#     Agent tab, which af renders itself), so multibyte glyphs like the
#     Vite "arrow" can degrade to a box/underscore depending on the pane's
#     locale. A plain ">" bullet reads just as well and never mangles.
# Nothing here talks to the network.
set -u

c_cyan=$'\033[36m'; c_green=$'\033[32m'; c_blue=$'\033[34m'
c_dim=$'\033[2m'; c_bold=$'\033[1m'; c_magenta=$'\033[35m'; c_reset=$'\033[0m'

printf '\n  %sVITE%s v5.2.0  %sready in 412 ms%s\n\n' \
    "$c_bold$c_magenta" "$c_reset" "$c_dim" "$c_reset"
sleep 0.3
printf '  %s>%s  Local:   %shttp://localhost:5173/%s\n' \
    "$c_green" "$c_reset" "$c_cyan" "$c_reset"
printf '  %s>%s  Network: %shttp://172.18.0.4:5173/%s\n' \
    "$c_green" "$c_reset" "$c_cyan" "$c_reset"
printf '  %s>%s  press %sh%s to show help\n\n' \
    "$c_green" "$c_reset" "$c_bold" "$c_reset"
sleep 0.5

req() {  # req <method> <path> <status> <ms>
    local sc="$c_green"
    [ "$3" = 304 ] && sc="$c_dim"
    printf '  %s%-4s%s %-22s %s%s%s %s%sms%s\n' \
        "$c_blue" "$1" "$c_reset" "$2" "$sc" "$3" "$c_reset" \
        "$c_dim" "$4" "$c_reset"
    sleep "${5:-0.35}"
}

printf '  %s%s [vite] hmr update /src/theme/dark.css%s\n' \
    "$c_dim" "$(date +%H:%M:%S 2>/dev/null || echo 12:04:07)" "$c_reset"
sleep 0.4
req GET  /                    200 3.1
req GET  /src/main.tsx        200 1.4
req GET  /src/theme/dark.css  200 0.8
req GET  /api/todos           200 8.6
req GET  /assets/logo.svg     304 0.3
req POST /api/todos           201 11.2

printf '\n  %scompiled ok%s %s- watching for changes%s\n\n' \
    "$c_green$c_bold" "$c_reset" "$c_dim" "$c_reset"

# Keep serving so the tab stays alive and the pane looks live: emit a slow
# trickle of requests, then drop to a shell (a program that exits would mark
# the tab dead).
paths=(/ /api/todos /src/App.tsx /api/health /assets/logo.svg)
i=0
while [ "$i" -lt 4 ]; do
    req GET "${paths[$((i % 5))]}" 200 "$(( (i * 3) % 9 + 1 )).$(( i % 9 ))" 0.7
    i=$((i + 1))
done

exec env PS1='docs@agent-factory:\W$ ' bash --noprofile --norc -i
