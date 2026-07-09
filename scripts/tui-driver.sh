#!/usr/bin/env bash
# tui-driver.sh — a deterministic, scriptable driver for the real Agent
# Factory TUI (#1161). Source it INSIDE the #1130 container sandbox and drive
# the live TUI through a private tmux session, syncing every action on a
# screen marker instead of a blind `sleep`.
#
#   docker exec "$AF_PLAYTEST_NAME" bash -lc \
#     'source /src/scripts/tui-driver.sh && af_boot && af_new_instance alpha'
#
# (The sandbox container name is unique per run — #1171; pin AF_PLAYTEST_NAME
# to target it. `make tui-driver-selftest` handles all of this for you.)
#
# The container already solves ISOLATION (throwaway home, mock repo, private
# tmux, pids/memory caps — scripts/testbox.sh + docs/container-testing.md).
# This library is the DRIVING + ASSERTION layer on top of it: the piece whose
# absence caused the #1156 mis-drive (keys landing in a live pane as text)
# and the #1155 hand-rolled-harness death.
#
# Design rules:
#   * Every action helper is SELF-SYNCHRONIZING: it returns only once the
#     screen shows its completion marker (af_wait_for), never after a fixed
#     sleep. The only sleeps here are short poll intervals between captures.
#   * af_ensure_nav forces a known focus state (Ctrl-]) so a scenario can
#     never mistake interactive mode for nav mode — the #1156 root cause.
#   * Nothing touches the host: it talks only to the container's own tmux
#     server. It never runs `tmux kill-server`; it only ever kills its OWN
#     named driver session.
#
# See docs/tui-manual-testing.md for the interaction model and gate recipes.

# ----------------------------------------------------------------------------
# Configuration (override via environment before sourcing / calling af_boot).
# ----------------------------------------------------------------------------
: "${AF_DRIVER_SESSION:=drive}"                 # tmux session the TUI runs in
: "${AF_DRIVER_COLS:=100}"                       # launch width
: "${AF_DRIVER_ROWS:=30}"                        # launch height
: "${AF_DRIVER_REPO:=$HOME/sandbox/mock-repo}"   # cwd the TUI launches in
: "${AGENT_FACTORY_HOME:=$HOME/sandbox/home}"    # sandbox AF home
: "${AF_DRIVER_BIN:=}"                            # af path ("" → auto-resolve)
: "${AF_DRIVER_TIMEOUT:=25}"                      # default wait timeout (s)
: "${AF_DRIVER_POLL:=0.25}"                       # capture-pane poll interval
: "${AF_DRIVER_DETACH_KEY:=C-w}"                 # tmux key that detaches attach
: "${AF_DRIVER_STATE_DIR:=${TMPDIR:-/tmp}/af-driver}"  # cross-call scratch
: "${AGENT_FACTORY_AUTO_UPDATE:=false}"          # disable startup auto-update: a
                                                 # branch binary built behind the
                                                 # latest release would try to
                                                 # self-update mid-test and time
                                                 # out instance creation.

export AGENT_FACTORY_HOME
export AGENT_FACTORY_AUTO_UPDATE

# ----------------------------------------------------------------------------
# Low-level plumbing.
# ----------------------------------------------------------------------------
_af_log()  { printf '[tui-driver] %s\n' "$*" >&2; }
_af_fail() { printf '[tui-driver] FAIL: %s\n' "$*" >&2; return 1; }

# _af_now — seconds since epoch (its own function so a test can stub it).
_af_now() { date +%s; }

# af_capture — the current TUI screen as plain text.
af_capture() { tmux capture-pane -p -t "$AF_DRIVER_SESSION" 2>/dev/null; }

# af_send <key>... — deliver key(s) to the TUI (tmux key names: Enter, Tab,
# Escape, 'C-]', or literal chars). Keys are interpreted by tmux.
af_send() { tmux send-keys -t "$AF_DRIVER_SESSION" "$@"; }

# af_send_literal <string> — deliver a string verbatim; also the injection path
# for raw SGR mouse sequences. Use a tmux paste buffer so the payload is stdin
# data, not tmux command syntax (`-foo`, `;;`, and friends stay literal).
af_send_literal() {
    local text="$1" buffer
    [ -n "$text" ] || return 0
    buffer="af-driver-literal-${BASHPID:-$$}-${RANDOM}"
    printf '%s' "$text" | tmux load-buffer -b "$buffer" - || return 1
    tmux paste-buffer -r -d -t "$AF_DRIVER_SESSION" -b "$buffer"
}

# _af_resolve_bin — the af binary to launch/readiness-check. Prefer PATH, fall
# back to the container's build output.
_af_resolve_bin() {
    if [ -n "$AF_DRIVER_BIN" ]; then printf '%s' "$AF_DRIVER_BIN"
    elif command -v af >/dev/null 2>&1; then command -v af
    else printf '%s' "$HOME/bin/af"; fi
}

# _af_regex_escape <s> — escape ERE metacharacters so a literal can be waited
# on with af_wait_for.
_af_regex_escape() { printf '%s' "$1" | sed -E 's/[.[\({*+?^$|)}\]\\]/\\&/g'; }

# ----------------------------------------------------------------------------
# Anti-flake core — wait on the screen, never on the clock.
# ----------------------------------------------------------------------------

# af_wait_for <regex> [timeout_s] [label] — poll capture-pane until the screen
# matches <regex>. Returns 0 on match, 1 on timeout (dumping the last screen).
af_wait_for() {
    local re="$1" timeout="${2:-$AF_DRIVER_TIMEOUT}" label="${3:-$1}" screen
    local deadline; deadline=$(( $(_af_now) + timeout ))
    while :; do
        screen="$(af_capture)"
        if printf '%s\n' "$screen" | grep -qE -- "$re"; then
            return 0
        fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log "TIMEOUT ${timeout}s waiting for: $label"
            _af_log "----- last screen -----"
            printf '%s\n' "$screen" >&2
            _af_log "-----------------------"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# af_wait_gone <regex> [timeout_s] [label] — the inverse: wait until <regex> is
# absent from the screen.
af_wait_gone() {
    local re="$1" timeout="${2:-$AF_DRIVER_TIMEOUT}" label="${3:-!$1}" screen
    local deadline; deadline=$(( $(_af_now) + timeout ))
    while :; do
        screen="$(af_capture)"
        if ! printf '%s\n' "$screen" | grep -qE -- "$re"; then
            return 0
        fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log "TIMEOUT ${timeout}s waiting for absence of: $label"
            printf '%s\n' "$screen" >&2
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# _af_strip_screen_frame — remove the TUI frame columns from captured rows so
# a terminal line wrapped inside a framed pane can be joined back together.
_af_strip_screen_frame() {
    sed -E 's/\r//g;
            s/^[^│║┃┆┇┊┋]*[│║┃┆┇┊┋]//;
            s/[│║┃┆┇┊┋][^│║┃┆┇┊┋]*$//;
            s/^[[:space:]│║┃┆┇┊┋╭╮╰╯┌┐└┘╔╗╚╝╠╣╦╩╬─═]+//;
            s/[[:space:]│║┃┆┇┊┋╭╮╰╯┌┐└┘╔╗╚╝╠╣╦╩╬─═]+$//'
}

_af_join_lines_tight() {
    tr -d '\n'
}

_af_join_lines_spaced() {
    awk 'NR > 1 { printf " " } { printf "%s", $0 }'
}

_af_squash_ws() {
    tr '\n\r\t' '   ' | sed -E 's/[[:space:]]+/ /g; s/^ //; s/ $//'
}

# _af_wait_for_pane_echo <literal> [timeout_s] [label] — wait for pane input
# echo without assuming the marker remains on one captured row. The embedded
# pane is framed by the TUI, so normalize both "wrap inside a word" and "wrap
# at whitespace" cases before falling back to a whitespace-squashed comparison.
_af_wait_for_pane_echo() {
    local text="$1" timeout="${2:-8}" label="${3:-pane echo}" screen
    local tight spaced spaced_ws text_ws
    local deadline; deadline=$(( $(_af_now) + timeout ))
    text_ws="$(printf '%s' "$text" | _af_squash_ws)"
    while :; do
        screen="$(af_capture)"
        if printf '%s\n' "$screen" | grep -Fq -- "$text"; then
            return 0
        fi

        tight="$(printf '%s\n' "$screen" | _af_strip_screen_frame | _af_join_lines_tight)"
        if printf '%s' "$tight" | grep -Fq -- "$text"; then
            return 0
        fi

        spaced="$(printf '%s\n' "$screen" | _af_strip_screen_frame | _af_join_lines_spaced)"
        if printf '%s' "$spaced" | grep -Fq -- "$text"; then
            return 0
        fi

        spaced_ws="$(printf '%s' "$spaced" | _af_squash_ws)"
        if [ -n "$text_ws" ] && printf '%s' "$spaced_ws" | grep -Fq -- "$text_ws"; then
            return 0
        fi

        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log "TIMEOUT ${timeout}s waiting for: $label"
            _af_log "----- last screen -----"
            printf '%s\n' "$screen" >&2
            _af_log "-----------------------"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# af_ensure_nav — force a known focus state. Ctrl-] exits interactive mode (a
# no-op in nav mode). This one primitive fixes the #1156 class: after it, keys
# are guaranteed to reach the host, not a live pane as literal text.
af_ensure_nav() {
    af_send 'C-]'
    sleep "$AF_DRIVER_POLL"
}

# af_resize <cols> <rows> — force the driver session to an exact geometry and
# MAKE IT STICK. A DETACHED tmux session defaults to `window-size latest`,
# which snaps the window back to the last-attached client (80x23) and IGNORES
# the `new-session -x/-y` request — so tiny-size gates (60x15, 40x10) silently
# ran at 80x23 (#1174 item 2). Pinning `window-size manual` + an explicit
# `resize-window` makes the geometry deterministic.
#
# FAILS LOUDLY (non-zero + message) if tmux rejects the resize (invalid dims,
# missing session) OR the window's actual size doesn't match what was
# requested — a testing helper that lied about a resize would let a tiny-size
# gate keep running at the wrong size while believing it resized (Greptile,
# #1201). AF_DRIVER_COLS/ROWS are updated ONLY after the size is confirmed, so
# they never advertise a geometry that isn't live.
af_resize() {
    local cols="$1" rows="$2"
    if [ -z "$cols" ] || [ -z "$rows" ]; then
        _af_fail "af_resize: <cols> <rows> required"; return 1
    fi
    # window-size is a session option in modern tmux; -w covers older builds.
    tmux set-option -t "$AF_DRIVER_SESSION" window-size manual >/dev/null 2>&1 \
        || tmux set-option -w -t "$AF_DRIVER_SESSION" window-size manual >/dev/null 2>&1 \
        || true
    if ! tmux resize-window -t "$AF_DRIVER_SESSION" -x "$cols" -y "$rows" 2>/dev/null; then
        _af_fail "af_resize: tmux rejected resize to ${cols}x${rows} (invalid size, or missing session '$AF_DRIVER_SESSION')"
        return 1
    fi
    # Let tmux apply the resize and the TUI observe SIGWINCH / repaint, then
    # verify the window actually took the requested geometry.
    sleep "$AF_DRIVER_POLL"
    local actual
    actual="$(tmux display-message -p -t "$AF_DRIVER_SESSION" '#{window_width}x#{window_height}' 2>/dev/null)"
    if [ "$actual" != "${cols}x${rows}" ]; then
        _af_fail "af_resize: requested ${cols}x${rows} but window is ${actual:-unknown} — resize did not stick"
        return 1
    fi
    AF_DRIVER_COLS="$cols"
    AF_DRIVER_ROWS="$rows"
}

# af_focus_tree — put ring focus on the instances tree (the state whose menu
# advertises `n new`). Checks BEFORE pressing, so it never Tabs off the tree
# when already there. Assumes nav mode (call af_ensure_nav first).
af_focus_tree() {
    local _
    for _ in $(seq 1 8); do
        if af_capture | grep -qE -- 'n new'; then
            return 0
        fi
        af_send Tab
        sleep "$AF_DRIVER_POLL"
    done
    _af_fail "could not focus the tree ('n new' hint never appeared)"
}

# ----------------------------------------------------------------------------
# Scenario helpers — each encodes the nav/interactive model and self-syncs.
# ----------------------------------------------------------------------------

# af_reset_sandbox — return the sandbox to a clean, instance-free state so a
# scenario is deterministic across reruns in a REUSED container (the self-test
# calls this first). Strictly scoped to the container's own sandbox: it stops
# the sandbox daemon, kills the af_* instance sessions on THIS
# container-private tmux server, and wipes the instance store. It never
# touches the driver's own session and NEVER runs kill-server. Fails closed if
# AGENT_FACTORY_HOME does not look like a sandbox path, so it can never wipe a
# real ~/.agent-factory.
# _af_is_throwaway_sandbox <dir> — POSITIVE proof a path is a disposable driver
# sandbox before any destructive cleanup runs against it. Fails CLOSED: a real
# dev checkout (any repo with a remote), or any path missing the scaffolding's
# sentinel, is rejected. A weak `*/sandbox/*` substring guard once let this
# cleanup delete real dev worktrees + every non-master branch (#1303); this hard
# gate replaces it. All three conditions must hold.
_af_is_throwaway_sandbox() {
    local dir="$1"
    [ -n "$dir" ] || return 1
    # 1) The sandbox scaffolding writes this sentinel; a real checkout never has it.
    [ -f "$dir/.af-throwaway-sandbox" ] || return 1
    # 2) A throwaway mock repo has NO remotes; a real checkout has 'origin'.
    if [ -d "$dir/.git" ] && [ -n "$(git -C "$dir" remote 2>/dev/null)" ]; then
        return 1
    fi
    # 3) Belt-and-suspenders: still require a sandbox-shaped path.
    case "$dir" in
        */sandbox/*|*/sandbox|/tmp/*|*/af-driver*) return 0 ;;
        *) return 1 ;;
    esac
}

af_reset_sandbox() {
    case "$AGENT_FACTORY_HOME" in
        */sandbox/*|*/sandbox|/tmp/*|*/af-driver*) ;;
        *) _af_fail "af_reset_sandbox: refusing — '$AGENT_FACTORY_HOME' is not a sandbox path"; return 1 ;;
    esac
    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    if [ -f "$AGENT_FACTORY_HOME/daemon.pid" ]; then
        kill "$(cat "$AGENT_FACTORY_HOME/daemon.pid")" 2>/dev/null || true
    fi
    local s
    for s in $(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep '^af_' || true); do
        tmux kill-session -t "$s" 2>/dev/null || true
    done
    rm -rf "$AGENT_FACTORY_HOME/instances" 2>/dev/null || true
    rm -f "$AGENT_FACTORY_HOME/daemon.pid" "$AGENT_FACTORY_HOME/daemon.sock" \
          "$AGENT_FACTORY_HOME/state.json" 2>/dev/null || true
    # Remove leftover instance worktrees + branches in the mock repo, else a
    # re-created instance of the same name collides on the existing branch.
    # DESTRUCTIVE (rm -rf glob + `branch -D` every non-master ref): fail CLOSED,
    # only ever against a PROVEN throwaway sandbox. The old `*/sandbox/*` guard
    # honored an AF_DRIVER_REPO pointed at the real dev repo and deleted every
    # sibling worktree + session branch (#1303).
    if _af_is_throwaway_sandbox "$AF_DRIVER_REPO"; then
        rm -rf "${AF_DRIVER_REPO}"-* 2>/dev/null || true
        if [ -d "$AF_DRIVER_REPO/.git" ]; then
            git -C "$AF_DRIVER_REPO" worktree prune 2>/dev/null || true
            local b
            for b in $(git -C "$AF_DRIVER_REPO" for-each-ref \
                --format='%(refname:short)' refs/heads/ 2>/dev/null \
                | grep -vE '^(master|main)$' || true); do
                git -C "$AF_DRIVER_REPO" branch -D "$b" 2>/dev/null || true
            done
        fi
    else
        _af_log "af_reset_sandbox: SKIP worktree/branch cleanup — '$AF_DRIVER_REPO' is not a proven throwaway sandbox (needs a .af-throwaway-sandbox marker + no remote); refusing destructive rm/branch -D (#1303)"
    fi
    sleep 0.5
}

# af_boot — launch af at a fixed size in a fresh driver session and wait for
# the first frame. Pre-seeds help_screens_seen so the one-time overlays
# (instance-created, attach, interactive) never appear — the single biggest
# source of driver non-determinism.
af_boot() {
    mkdir -p "$AF_DRIVER_STATE_DIR" "$AGENT_FACTORY_HOME"
    local bin; bin="$(_af_resolve_bin)"

    # Wait for the container's on-boot `go build` to finish.
    local deadline; deadline=$(( $(_af_now) + 180 ))
    while [ ! -x "$bin" ]; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "af binary not found/executable at '$bin'"; return 1
        fi
        sleep 1
    done

    # Suppress every first-time help overlay (bits: 1 start | 2 attach |
    # 4 interactive, plus the general help bit) BEFORE the TUI reads state.
    printf '{\n  "help_screens_seen": 15\n}\n' > "$AGENT_FACTORY_HOME/state.json"

    # Fresh driver session. Kill ONLY our own named session — never the
    # server (the container hosts the daemon's sessions too).
    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    tmux new-session -d -s "$AF_DRIVER_SESSION" -x "$AF_DRIVER_COLS" -y "$AF_DRIVER_ROWS"
    # A detached session ignores -x/-y under `window-size latest`; pin it so the
    # TUI reads the requested geometry on startup (#1174 item 2). Honors any
    # AF_DRIVER_COLS/ROWS override set before af_boot, including tiny sizes.
    # af_resize verifies the geometry took, so a bad size fails the boot loudly.
    af_resize "$AF_DRIVER_COLS" "$AF_DRIVER_ROWS" || return 1

    af_send_literal "cd $AF_DRIVER_REPO && $bin"
    af_send Enter
    af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'af first frame' || return 1
    af_wait_for 'Instances \(' "$AF_DRIVER_TIMEOUT" 'instances header' || return 1
    af_focus_tree
}

# af_new_instance <name> — create an instance and wait until it is ready
# (its row shows the ● ready dot). Cheap-instance config makes `bash` the
# program, so ready arrives in seconds.
af_new_instance() {
    local name="$1"
    [ -n "$name" ] || { _af_fail "af_new_instance: name required"; return 1; }
    af_ensure_nav
    af_focus_tree || return 1
    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'name prompt' || return 1
    af_send_literal "$name"
    af_send Enter
    af_wait_for "${name}.*●" "$AF_DRIVER_TIMEOUT" "instance '${name}' ready" || return 1
}

# af_select <name> — put the tree cursor ON <name>'s row (so it is the *real*
# GetSelectedInstance(), not merely display-selected). Robust to any starting
# cursor position: anchor at the top (k is idempotent there), then step down
# until BOTH conditions hold — <name>'s row carries the ▾ selected/expanded
# arrow AND the menu advertises a row-scoped verb (`D kill`).
#
# The two-part success condition is the #1174-item-1 / #1199 fix. The sticky
# ▾ is a DISPLAY-selection: a SINGLE auto-selected instance renders ▾ while the
# tree cursor still sits on the `Instances` section header, so GetSelected-
# Instance() is nil and every cursor-driven verb (o attach, D kill, and
# af_attach/af_open_pane which run after af_select) silently no-ops. A ▾-only
# check returns on iteration 0 in that case — a false positive that can make a
# play-test wrongly "pass" a nav action that never fired. `D kill` appears in
# the menu ONLY when an instance is actually under the cursor (non-nil
# GetSelectedInstance()), so requiring it forces `j` past the header until the
# cursor truly lands on the row.
af_select() {
    local name="$1" _ screen name_re
    [ -n "$name" ] || { _af_fail "af_select: name required"; return 1; }
    name_re="$(_af_regex_escape "$name")"
    af_ensure_nav
    af_focus_tree || return 1
    for _ in $(seq 1 30); do af_send k; done
    sleep "$AF_DRIVER_POLL"
    for _ in $(seq 1 40); do
        screen="$(af_capture)"
        if printf '%s\n' "$screen" | grep -qE -- "▾[[:space:]]+${name_re}([[:space:]]|\$)" \
           && printf '%s\n' "$screen" | grep -qE -- 'D kill'; then
            return 0
        fi
        af_send j
        sleep "$AF_DRIVER_POLL"
    done
    _af_log "could not select '${name}' (need ▾ on its row AND cursor-on-row, i.e. 'D kill' in the menu)"
    printf '%s\n' "$screen" >&2
    return 1
}

# af_open_pane — open the selected instance's active tab as a workspace pane
# (or focus its pane if already open). Precondition: an instance is selected
# (call af_select first). Syncs on the pane-focus menu (`x hide pane`).
af_open_pane() {
    af_ensure_nav
    af_focus_tree || return 1
    af_send s
    af_wait_for 'hide pane' "$AF_DRIVER_TIMEOUT" 'pane opened/focused' || return 1
}

# af_hide_pane — hide the focused pane back to the background (nothing is
# killed). Precondition: a pane is focused. Syncs on focus returning to the
# tree (`n new`).
af_hide_pane() {
    af_send x
    af_wait_for 'n new' "$AF_DRIVER_TIMEOUT" 'pane hidden' || return 1
}

# af_enter_interactive — enter the focused pane: every subsequent key forwards
# to the pane's shell/agent. Precondition: a pane is focused. Syncs on the
# interactive menu (only `ctrl+] nav mode` shows).
af_enter_interactive() {
    af_send Enter
    af_wait_for 'nav mode' "$AF_DRIVER_TIMEOUT" 'interactive mode' || return 1
}

# af_exit_interactive — Ctrl-] back to nav mode. Syncs on the interactive
# menu disappearing.
af_exit_interactive() {
    af_send 'C-]'
    af_wait_gone 'nav mode' "$AF_DRIVER_TIMEOUT" 'exit interactive' || return 1
}

# af_send_to_pane <text> — type <text> + Enter into the pane. Precondition:
# interactive mode is active (af_enter_interactive). Best-effort syncs on a
# short no-op delivery marker, not the full command text, so confirmation stays
# wrap-tolerant while the caller's command remains the final command executed.
# The CALLER should af_wait_for the command's output.
af_send_to_pane() {
    local text="$1" marker
    printf -v marker 'AF%04X%04X' "$RANDOM" "$RANDOM"
    af_send_literal ": \"$marker\"; $text"
    af_send Enter
    _af_wait_for_pane_echo "$marker" 8 "pane echoed delivery marker for '$text'" \
        || _af_log "note: delivery marker for '$text' not seen echoed (may have scrolled off)"
}

# af_attach — attach the selected instance full-screen (`o`). Precondition: an
# instance is selected. Records the attach-client baseline so af_detach can
# prove the client was reaped. Syncs on the TUI chrome vanishing.
af_attach() {
    af_ensure_nav
    af_focus_tree || return 1
    _af_client_count > "$AF_DRIVER_STATE_DIR/attach_baseline"
    af_send o
    af_wait_gone 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'full-screen attach' || return 1
    # Attach intentionally discards terminal probe bytes for ~50ms; let that
    # window pass so an immediate scripted af_detach is not swallowed.
    sleep "$AF_DRIVER_POLL"
}

# af_detach — detach from a full-screen attach (Ctrl-W by default) and, the
# anti-#1157-flake part, wait until the attach client is actually reaped (the
# count returns to the pre-attach baseline). A leaked client fails here.
af_detach() {
    af_send "$AF_DRIVER_DETACH_KEY"
    sleep "$AF_DRIVER_POLL"
    if ! af_capture | grep -q 'Agent Factory'; then
        af_send "$AF_DRIVER_DETACH_KEY"
    fi
    af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'detached back to TUI' || return 1
    local baseline; baseline="$(cat "$AF_DRIVER_STATE_DIR/attach_baseline" 2>/dev/null || echo 0)"
    local deadline; deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    local n
    while :; do
        n="$(_af_client_count)"
        [ "$n" -le "$baseline" ] && return 0
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log "detach: attach clients did not return to baseline ($n > $baseline) — possible #1157-class leak"
            af_ps >&2
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# af_new_tab — spawn a new shell tab in the selected instance (`t`).
# Precondition: an instance is selected. Syncs on the tab-child count rising.
af_new_tab() {
    af_ensure_nav
    af_focus_tree || return 1
    local before; before="$(_af_tab_count)"
    af_send t
    local deadline; deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while [ "$(_af_tab_count)" -le "$before" ]; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "new tab did not appear (was $before)"; return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# af_close_tab — close the active (non-agent) tab (`w`). Precondition: a
# closeable tab is active. Syncs on the tab-child count falling.
af_close_tab() {
    af_ensure_nav
    af_focus_tree || return 1
    local before; before="$(_af_tab_count)"
    af_send w
    local deadline; deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while [ "$(_af_tab_count)" -ge "$before" ]; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "tab not closed (still $before)"; return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# _af_tab_count — number of tab-child rows the selected instance shows. Only
# the selected instance auto-expands, so this counts its tabs.
_af_tab_count() {
    af_capture | grep -cE '[├└][[:space:]]+[0-9]+[[:space:]]+(Preview|Terminal|Diff)' || true
}

# af_open_tasks — open the task-manager overlay (`m`). Syncs on the overlay's
# unique `r run now` hint line.
af_open_tasks() {
    af_ensure_nav
    af_send m
    af_wait_for 'run now' "$AF_DRIVER_TIMEOUT" 'tasks overlay' || return 1
}

# af_close_tasks — dismiss the tasks overlay (Escape).
af_close_tasks() {
    af_send Escape
    af_wait_gone 'run now' 8 'tasks overlay closed' || return 1
}

# af_click <x> <y> — inject an SGR left click at 1-based screen cell (x,y).
# This is how the #1143 mouse work is codified: a real terminal would send
# these bytes; we send them straight to the TUI's stdin.
af_click() {
    local x="$1" y="$2"
    af_send_literal "$(printf '\033[<0;%d;%dM\033[<0;%d;%dm' "$x" "$y" "$x" "$y")"
    sleep "$AF_DRIVER_POLL"
}

# af_click_instance <name> — find <name>'s row on screen and click it.
af_click_instance() {
    local name="$1" line name_re
    name_re="$(_af_regex_escape "$name")"
    line="$(af_capture | grep -nE "[▾▸][[:space:]]+${name_re}([[:space:]]|\$)" | head -1 | cut -d: -f1)"
    [ -n "$line" ] || { _af_fail "instance '$name' not visible to click"; return 1; }
    af_click 14 "$line"
}

# af_scroll <up|down> [x] [y] — inject an SGR mouse-wheel event over cell
# (x,y) (defaults to the workspace pane).
af_scroll() {
    local dir="$1" x="${2:-60}" y="${3:-10}" b
    case "$dir" in
        up)   b=64 ;;
        down) b=65 ;;
        *) _af_fail "af_scroll: direction must be up|down"; return 1 ;;
    esac
    af_send_literal "$(printf '\033[<%d;%d;%dM' "$b" "$x" "$y")"
    sleep "$AF_DRIVER_POLL"
}

# af_set_config <toml-body> — replace the sandbox config.toml. NOTE: once
# config.toml exists it is canonical and config.json is ignored (#1030), so
# the driver writes TOML. Include a [program_overrides] claude = 'bash' block
# to keep instances cheap. Call af_relaunch to apply.
af_set_config() {
    printf '%s\n' "$1" > "$AGENT_FACTORY_HOME/config.toml"
}

# af_quit — quit the TUI back to a shell prompt. Dismisses any overlay and
# interactive mode first, then syncs on the on-quit log line.
af_quit() {
    af_ensure_nav
    af_send Escape
    af_send q
    af_wait_for 'wrote logs to|\$ *$' 10 'shell prompt after quit' || return 1
}

# af_relaunch — quit and relaunch af (for config/keymap play-tests). Reuses
# the existing driver session.
af_relaunch() {
    local bin; bin="$(_af_resolve_bin)"
    af_quit || return 1
    af_send_literal "cd $AF_DRIVER_REPO && $bin"
    af_send Enter
    af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'af first frame (relaunch)' || return 1
    af_wait_for 'Instances \(' "$AF_DRIVER_TIMEOUT" 'instances header (relaunch)' || return 1
    af_focus_tree
}

# ----------------------------------------------------------------------------
# Assertions.
# ----------------------------------------------------------------------------

# af_assert_screen <regex> [label] — pass iff the screen matches <regex>.
af_assert_screen() {
    local re="$1" label="${2:-$1}" screen
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE -- "$re"; then
        _af_log "assert OK: $label"
        return 0
    fi
    _af_log "ASSERT FAILED: expected screen to match: $label"
    _af_log "----- screen -----"
    printf '%s\n' "$screen" >&2
    _af_log "------------------"
    return 1
}

# af_refute_screen <regex> [label] — pass iff the screen does NOT match.
af_refute_screen() {
    local re="$1" label="${2:-$1}" screen
    screen="$(af_capture)"
    if printf '%s\n' "$screen" | grep -qE -- "$re"; then
        _af_log "REFUTE FAILED: screen unexpectedly matched: $label"
        printf '%s\n' "$screen" >&2
        return 1
    fi
    _af_log "refute OK: $label"
    return 0
}

# af_expect_selected <name> — assert <name> is the display-selected instance
# (its row carries the ▾ arrow). This is the two-line answer to the #1156
# failure: af_select + af_expect_selected.
af_expect_selected() {
    local name="$1" name_re
    name_re="$(_af_regex_escape "$name")"
    af_assert_screen "▾[[:space:]]+${name_re}([[:space:]]|\$)" \
        "instance '${name}' selected"
}

# ----------------------------------------------------------------------------
# Introspection / leak checks.
# ----------------------------------------------------------------------------

# _af_client_count — number of tmux clients attached to af_* sessions.
_af_client_count() {
    tmux list-clients 2>/dev/null | grep -c 'af_' || true
}

# af_tmux_ls — list tmux sessions on the (container's private) server.
af_tmux_ls() { tmux list-sessions 2>&1; }

# af_ps — the af process tree relevant to leaks: the daemon, and every tmux
# attach/new-session it or the TUI spawned.
af_ps() {
    # ps (not pgrep): we want the ppid+args table, and the same shape the
    # orphan check reasons about.
    # shellcheck disable=SC2009
    ps -eo pid,ppid,args 2>/dev/null \
        | grep -E 'af --daemon|/af( |$)|tmux (attach-session|new-session)' \
        | grep -v grep || true
}

# af_assert_no_orphan_clients — fail if any `tmux attach-session` client has
# been reparented to init (ppid 1): its spawning TUI/pane exited but the
# client lived on. That is the #1155/#1157 leak signature. The daemon's own
# monitor clients are parented to the daemon (ppid != 1), so they are
# correctly excluded.
af_assert_no_orphan_clients() {
    local orphans
    orphans="$(ps -eo ppid,args 2>/dev/null | awk '$1==1 && /tmux attach-session/ {print}')"
    if [ -n "$orphans" ]; then
        _af_log "ORPHAN attach clients (reparented to init):"
        printf '%s\n' "$orphans" >&2
        return 1
    fi
    _af_log "assert OK: no orphan tmux attach clients"
    return 0
}
