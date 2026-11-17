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
# tmux, pids/memory caps — scripts/testbox.sh + docs/dev/container-testing.md).
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
# See docs/dev/tui-manual-testing.md for the interaction model and gate recipes.

# ----------------------------------------------------------------------------
# Configuration (override via environment before sourcing / calling af_boot).
# ----------------------------------------------------------------------------
: "${AF_DRIVER_SESSION:=drive}"                 # tmux session the TUI runs in
: "${AF_DRIVER_COLS:=100}"                       # launch width
: "${AF_DRIVER_ROWS:=30}"                        # launch height
: "${AF_DRIVER_REPO:=$HOME/sandbox/mock-repo}"   # cwd the TUI launches in
: "${AGENT_FACTORY_HOME:=$HOME/sandbox/home}"    # sandbox AF home
: "${AF_DRIVER_BIN:=}"                            # af path ("" → auto-resolve)
# Environment assignments prefixed to the af launch, e.g.
# AF_DRIVER_LAUNCH_ENV="AF_DAEMON_URL=http://127.0.0.1:8899". Empty by default,
# so every existing scenario launches exactly the command it did before. It is a
# PREFIX on the launch line rather than an export here because af_boot starts a
# fresh tmux session, whose shell inherits the tmux SERVER's environment (set
# when the server first started) and not this script's — so exporting would work
# or not depending on whether some earlier session had already started the
# server. Scenarios that need the TUI itself pointed somewhere use this.
: "${AF_DRIVER_LAUNCH_ENV:=}"                    # env prefix for the af launch line
: "${AF_DRIVER_TIMEOUT:=25}"                      # default wait timeout (s)
: "${AF_DRIVER_POLL:=0.25}"                       # capture-pane poll interval
: "${AF_DRIVER_KEY_SETTLE_POLLS:=8}"              # polls a nav key gets to show its effect
: "${AF_DRIVER_DETACH_KEY:=C-w}"                 # tmux key that detaches attach
: "${AF_DRIVER_STATE_DIR:=${TMPDIR:-/tmp}/af-driver}"  # cross-call scratch
: "${AF_DRIVER_SEND_LINE_ATTEMPTS:=4}"           # af_send_line paste retries
# help_screens_seen af_boot writes before launching. The default 15 marks every
# one-time overlay seen, which is what makes ordinary scenarios deterministic.
# Clear a bit to drive a FIRST-RUN overlay on purpose — e.g.
# AF_DRIVER_HELP_SEEN=7 boots into the first-run interactive help, the only way
# to reach that screen from a driver scenario (#2413).
#
# The bits are app/help.go's mask() methods, and they are NOT in the order this
# file used to claim ("1 start | 2 attach | 4 interactive"): general is 1<<0,
# instance-start 1<<1, instance-attach 1<<2, interactive 1<<3. Nothing depended
# on the wrong list while the value was a hardcoded 15 — every bit was set
# either way — but it is load-bearing the moment a caller clears one.
#
#   1  general help (?)          4  instance-attach help (o)
#   2  instance-start help (n)   8  interactive-pane help (enter)
: "${AF_DRIVER_HELP_SEEN:=15}"                   # one-time overlays pre-marked seen
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
_af_regex_escape() { printf '%s' "$1" | sed 's/[][\.^$*+?(){}|\\]/\\&/g'; }

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

# _AF_PANE_BORDER_FS — the field separator that splits a captured row on ANY
# pane vertical border. A pane's frame glyph depends on its STATE, not on the
# driver's mode-agnostic view of it: ordinary/selected/preview panes render the
# rounded border │ (U+2502), while the pane that owns the keyboard in
# interactive mode renders a DOUBLE border ║ (U+2551) — ui/tabbed_window.go's
# interactiveWindowStyle, the #1089 "keystrokes go INTO this terminal" signal
# that must read without color. Splitting on only one of them makes every
# wrap-tolerant matcher below work in one mode and false-time-out in the other
# (#2145), so every border is one separator set here and no caller
# special-cases a mode.
#
# The two glyphs are joined with an ERE ALTERNATION, never a bracket expression
# ([│║]): the sandbox runs a C/POSIX locale where a bracket expression matches
# byte-by-byte, so it would match single bytes of these 3-byte glyphs and
# shred the row (the trap #1994 and _af_tab_count both document). Alternation
# matches each glyph's literal byte sequence regardless of locale.
: "${_AF_PANE_BORDER_FS:=│|║}"

# _af_pane_column — for each captured row, the workspace pane's OWN text: the
# cell between its box's two vertical borders (the second-to-last
# border-delimited field). Rows with fewer than two borders — sidebar-only
# rows, the box's ─/═ top/bottom, an unframed full-screen capture — contribute
# nothing. Joined across rows (via _af_join_lines_*), this reconstructs a line
# that wrapped INSIDE the pane.
#
# Taking $(NF - 1) — rather than stripping the first and last border — is what
# makes the extra borders on a row harmless: the sidebar's own tree-guide │,
# and the mixed row where a │-bordered sidebar/pane sits left of a ║-bordered
# interactive pane. The old _af_strip_screen_frame removed only the FIRST and
# LAST border, so a sidebar tree-guide │ became the "first border" and its cell
# text leaked between the marker's halves — one of the two ways #1994's split
# marker went unrecognized (its bracket-expression matching, see
# _AF_PANE_BORDER_FS, was the other).
_af_pane_column() {
    awk -F"$_AF_PANE_BORDER_FS" '
        NF >= 3 {
            cell = $(NF - 1)
            gsub(/\r/, "", cell)
            gsub(/^[[:space:]]+/, "", cell)
            gsub(/[[:space:]]+$/, "", cell)
            print cell
        }'
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
# pane is framed by the TUI, so reconstruct the pane's own column and normalize
# both "wrap inside a word" (tight join) and "wrap at whitespace" (spaced join)
# before falling back to a whitespace-squashed comparison.
#
# The reconstruction is border-glyph agnostic (_AF_PANE_BORDER_FS): the caller
# is usually af_send_to_pane, which by definition runs against the INTERACTIVE
# pane and its ║ frame, so a │-only split matched nothing there and every
# successful command paid the full 8s timeout (#2145).
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

        tight="$(printf '%s\n' "$screen" | _af_pane_column | _af_join_lines_tight)"
        if printf '%s' "$tight" | grep -Fq -- "$text"; then
            return 0
        fi

        spaced="$(printf '%s\n' "$screen" | _af_pane_column | _af_join_lines_spaced)"
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

# _af_line_echo_count <text> — how many times the WHOLE of <text> is currently
# echoed on an UNFRAMED (full-screen attach) screen.
#
# Both sides are stripped of ALL whitespace before comparing, which buys three
# things at once and is why this is not a plain `grep -F` on the raw capture:
#   * a line that WRAPPED at the terminal edge still matches (the wrap is just a
#     newline between two halves of the same string);
#   * a space that fell exactly ON the wrap column is not fatal — capture-pane
#     right-trims every row, so that space is simply absent from the capture
#     (observed live: a 128-char line echoes as 127 captured characters, missing
#     the space between `echo` and its argument);
#   * the compare is byte-wise on the stripped strings, so it is independent of
#     the sandbox's C/POSIX locale (cf. _af_tab_count).
#
# Matching the WHOLE text — not a suffix of it — is the #2147 correction. The
# old check matched a 24-character TAIL, on the theory that a racing Enter can
# only truncate the END of a paste. That theory does not hold on the full-screen
# attach path: whole interior chunks of a paste can go missing while the tail
# lands (reproduced live — a 128-char line echoed as chunks 1, 2 and 4 with
# chunk 3 dropped), and a tail-only check CONFIRMS that mangled line. A probe
# that cannot see the middle of the line must not get to vouch for it.
_af_line_echo_count() {
    local text_ws screen_ws count
    text_ws="$(printf '%s' "$1" | tr -d '[:space:]')"
    if [ -z "$text_ws" ]; then printf '0'; return 0; fi
    screen_ws="$(af_capture | tr -d '[:space:]')"
    count="$(printf '%s' "$screen_ws" | grep -oF -- "$text_ws" | wc -l)" || count=0
    count="${count//[^0-9]/}"
    printf '%s' "${count:-0}"
    return 0
}

# _af_wait_for_line_echo <text> [timeout_s] [baseline] — wait until the
# screen shows MORE than <baseline> (default 0) complete echoes of <text>. This
# is the full-screen counterpart to _af_wait_for_pane_echo: there is no pane box
# to reconstruct, so the whitespace-stripped compare above is sufficient.
#
# The baseline is what keeps the confirmation honest across repeats. A command
# the driver already ran once is still on screen in the scrollback, so a bare
# "is it there?" check would confirm the NEXT paste of the same text before a
# single byte of it had landed. Callers pass the count they measured immediately
# before pasting, so what is confirmed is that THIS paste added an occurrence.
#
# Deliberately silent: af_send_line retries around it and owns the diagnostics,
# so a recovered attempt does not print a TIMEOUT banner into a passing run.
# Returns 0 on match, 1 on timeout.
_af_wait_for_line_echo() {
    local text="$1" timeout="${2:-8}" baseline="${3:-0}"
    local deadline; deadline=$(( $(_af_now) + timeout ))
    while :; do
        if [ "$(_af_line_echo_count "$text")" -gt "$baseline" ]; then
            return 0
        fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# _af_clear_input_line — wipe whatever is sitting on the attached shell's input
# line (ctrl-u, readline's unix-line-discard). Used by af_send_line both BETWEEN
# retries — so a re-paste appends to an empty line instead of concatenating onto
# a partial one — and on give-up, so a failed delivery leaves the shell at a
# clean prompt rather than holding an unbalanced fragment.
_af_clear_input_line() {
    af_send C-u
    sleep "$AF_DRIVER_POLL"
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

# _af_menu_row — isolate the unframed navigation status-bar menu. Tree and
# pane menus end with the same help/quit signature. Reject pane-border rows
# even if their output contains a complete copied menu. Keep the final match:
# the status bar is below the workspace, but notices can follow its menu.
_af_menu_row() {
    awk -v borders="$_AF_PANE_BORDER_FS" '
        $0 !~ borders && /(^|[[:space:]])[?] help · q quit[[:space:]]*$/ { row = $0 }
        END { if (row != "") print row }'
}

# _af_rail_cursor — the rail's expanded session row, as "<screen row>:▾ <first
# word of its title>", or nothing when no session row is expanded. Reads a
# screen on stdin. Vertical tree navigation stops only on tab rows, and the
# session that owns the cursor's tab is the one drawn ▾, so this changes
# exactly when a key carries the cursor onto another session. Only the rail
# cell (left of the first pane border) is read, so pane text cannot pose as a
# row, and the title is cut at its first word, so a status glyph repainting
# beside it does not read as the cursor moving.
_af_rail_cursor() {
    awk -F"$_AF_PANE_BORDER_FS" '
        match($1, /▾[[:space:]]+[^[:space:]]+/) { print NR ":" substr($1, RSTART, RLENGTH); exit }'
}

# _af_rail_top_selected — succeeds when the rail's FIRST session row is the
# expanded one. Reads a screen on stdin. Rows the cursor cannot stop on (an
# in-flight create or delete) draw no arrow, so the first ▸/▾ row is the first
# session the cursor can stop on.
#
# The rail windows its rows around the cursor and says how many it hides above
# with a `▲ N more` row (ui/sidebar_render.go). Above the first session there
# is only the Sessions header (rebuildVisibleItems), which the window scrolls
# away when the cursor climbs to that session, so `▲ 1 more` still shows the
# first session. A larger count can hide a session, and the first VISIBLE row
# then proves nothing about the top (#7197 review).
_af_rail_top_selected() {
    awk -F"$_AF_PANE_BORDER_FS" '
        match($1, /▲[[:space:]]+[0-9]+/) {
            n = substr($1, RSTART, RLENGTH); gsub(/[^0-9]/, "", n); hidden = n + 0
        }
        $1 ~ /(▾|▸)[[:space:]]+[^[:space:]]/ { top = $1; exit }
        END { exit (hidden <= 1 && top ~ /▾/) ? 0 : 1 }'
}

# The instance-scoped verb the status-bar menu offers only while a session is
# under the tree cursor (`D delete session`, or legacy `D kill`).
_AF_INSTANCE_VERB_RE='(^|[[:space:]])D (kill|delete session)([[:space:]]|$)'

# _af_select_state — what one af_select scan key can visibly change: which
# session the rail expands (_af_rail_cursor), and whether the menu offers the
# instance verb. The second covers the first `j` off a section header, which
# lands on the session the header already shows ▾ for.
_af_select_state() {
    local screen
    screen="$(cat)"
    printf '%s\n' "$screen" | _af_rail_cursor
    if printf '%s\n' "$screen" | _af_menu_row | grep -qE -- "$_AF_INSTANCE_VERB_RE"; then
        printf 'verb\n'
    fi
}

# _af_anchor_at_first_session — af_select's anchor: press k until the tree
# cursor can climb no further, waiting for that on the screen rather than for
# one fixed poll (#7197), so that no k is still queued when the scan's first
# `j` goes out. Sets _AF_ANCHOR_BURSTS to the bursts it sent, which af_select
# uses to size its scan.
#
# k goes out in bursts of 30. The anchor is done when:
#   * the first session is the expanded one (_af_rail_top_selected). k cannot
#     move the cursor off it, so a k still queued then is harmless. This is
#     the ordinary case.
#   * a whole burst changed nothing the scan can see (_af_select_state) for
#     four times AF_DRIVER_KEY_SETTLE_POLLS. The cursor is then where k cannot
#     climb from: a section header, a folded session or section, or the first
#     session with in-flight rows hidden above it (#7198 review). The scan's
#     `j` presses take it from there, as the old fixed-poll scan's did. The
#     long quiet keeps a TUI that has not yet started on the burst from being
#     taken for one that finished it.
# A burst that moved the cursor and then stopped short of the top started more
# than 30 stops down, so another burst goes out, up to 10. Each burst gets
# AF_DRIVER_TIMEOUT.
_af_anchor_at_first_session() {
    local burst k screen state before last still deadline
    _AF_ANCHOR_BURSTS=0
    for ((burst = 0; burst < 10; burst++)); do
        _AF_ANCHOR_BURSTS=$((burst + 1))
        before="$(af_capture | _af_select_state)"
        for ((k = 0; k < 30; k++)); do af_send k; done
        deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
        last="$before" still=0
        while :; do
            screen="$(af_capture)"
            printf '%s\n' "$screen" | _af_rail_top_selected && return 0
            state="$(printf '%s\n' "$screen" | _af_select_state)"
            if [ "$state" = "$last" ]; then
                still=$((still + 1))
            else
                still=0 last="$state"
            fi
            if [ "$state" = "$before" ]; then
                [ "$still" -ge $((4 * AF_DRIVER_KEY_SETTLE_POLLS)) ] && return 0
            elif [ "$still" -ge "$AF_DRIVER_KEY_SETTLE_POLLS" ]; then
                break
            fi
            if [ "$(_af_now)" -ge "$deadline" ]; then
                break 2
            fi
            sleep "$AF_DRIVER_POLL"
        done
    done
    _af_log "the k anchor never settled"
    printf '%s\n' "$screen" >&2
    return 1
}

# _af_wait_key_effect <filter> <before> — after a navigation key, wait until
# the key shows: poll until `af_capture | <filter>` differs from <before>, the
# same filter's output on the frame the key was sent from. Returns 1 after
# AF_DRIVER_KEY_SETTLE_POLLS polls with no change, which is what a key with no
# visible effect looks like (a scan already at its last stop).
#
# A fixed poll here is what #7197 was. When the TUI falls behind the keys, a
# frame captured one poll after a key can predate it. A scan that reads such a
# frame accepts a target the cursor is about to leave, with that key still on
# its way, or sends its next key before the last one has landed, so the TUI
# can act on both between two captures and the scan never sees the row it was
# looking for. Waiting for the key's own effect keeps at most one key in
# flight. The bound is a poll COUNT rather than a deadline so a stubbed
# `sleep` returns at once.
_af_wait_key_effect() {
    local filter="$1" before="$2" n
    for ((n = 0; n < AF_DRIVER_KEY_SETTLE_POLLS; n++)); do
        sleep "$AF_DRIVER_POLL"
        [ "$(af_capture | "$filter")" != "$before" ] && return 0
    done
    return 1
}

# af_focus_tree — put ring focus on the instances tree (the state whose menu
# advertises `n new`). Checks BEFORE pressing, so it never Tabs off the tree
# when already there. Assumes nav mode (call af_ensure_nav first). Each Tab
# moves the focus ring one region and swaps the status-bar menu, so wait for
# that swap before judging the next frame: a frame that still shows the old
# menu would send a second Tab, and the two together can carry focus past the
# tree (#7197).
af_focus_tree() {
    local _ menu
    for _ in $(seq 1 8); do
        menu="$(af_capture | _af_menu_row)"
        if printf '%s\n' "$menu" | grep -qE -- 'n new'; then
            return 0
        fi
        af_send Tab
        _af_wait_key_effect _af_menu_row "$menu" || true
    done
    _af_fail "could not focus the tree ('n new' hint never appeared)"
}

# ----------------------------------------------------------------------------
# Scenario helpers — each encodes the nav/interactive model and self-syncs.
# ----------------------------------------------------------------------------

# af_reset_sandbox — return the sandbox to a clean, instance- AND task-free
# state so a scenario is deterministic across reruns in a REUSED container (the
# self-test calls this first). Strictly scoped to the container's own sandbox:
# it stops the sandbox daemon, kills the af_* instance sessions on THIS
# container-private tmux server, and wipes the session/task state the same way
# `af reset` defines it (see the rm block below). It never touches the driver's
# own session and NEVER runs kill-server. Fails closed if AGENT_FACTORY_HOME
# does not look like a sandbox path, so it can never wipe a real
# ~/.agent-factory.
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
    # Session + task state under the sandbox AF home.
    #
    # This mirrors `af reset`'s resetWipePaths (commands/reset.go) — the
    # canonical split between session/task STATE (wiped) and CONFIGURATION
    # (kept: config.toml, repos/, daemon-token) — plus the daemon runtime files
    # the sandbox reset owns. `af reset` deliberately preserves daemon.pid/sock
    # and the token so an already-configured daemon keeps its identity; a
    # throwaway sandbox has no identity worth keeping, and we just killed its
    # daemon, so those go too.
    #
    # This list used to be just instances/ + state.json, which made the reset a
    # HALF reset: tasks.json survived it (#1824). The self-test seeds
    # `selftest-task`, so a REUSED container's second run found that task
    # already present, `m` dropped into the EDIT form instead of the create
    # form (#1249/#1757), and af_add_task typed into the existing task and
    # failed at "seed a task via the create form". One surviving state file is
    # enough to make a "reset" sandbox non-deterministic — so reset what
    # `af reset` resets, and keep the two definitions in step.
    #
    # The *.lock files (tasks.json.lock, config.toml.lock, daemon-token.lock)
    # are deliberately kept: they are content-free flock sentinels, they carry
    # no state that can bleed into the next run, and unlinking one out from
    # under a starting daemon breaks the lock's mutual exclusion.
    rm -rf "$AGENT_FACTORY_HOME/instances" \
           "$AGENT_FACTORY_HOME/worktrees" \
           "$AGENT_FACTORY_HOME/events" \
           "$AGENT_FACTORY_HOME/logs" \
           "$AGENT_FACTORY_HOME/locks" 2>/dev/null || true
    rm -f "$AGENT_FACTORY_HOME/daemon.pid" "$AGENT_FACTORY_HOME/daemon.sock" \
          "$AGENT_FACTORY_HOME/state.json" \
          "$AGENT_FACTORY_HOME/tui-state.json" \
          "$AGENT_FACTORY_HOME/tasks.json" 2>/dev/null || true
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

# _AF_RAIL_MARKER — proof that the instance rail has painted, i.e. that af is
# past its first frame and not merely showing chrome. Used as the second boot
# gate by af_boot and af_relaunch.
#
# It accepts EITHER the section header OR the rail's scrolled-up indicator,
# because the header is not chrome: it is the rail's first windowed ROW
# (ui/sidebar_render.go String()), so it scrolls out of the window like any
# other row. At 80x24 with three sessions and the middle or lower one selected,
# the rail scrolls and renders `▲ 2 more` where the header used to be — the TUI
# has booted perfectly, every session is alive, and a header-only gate waits the
# full timeout and reports a boot failure that never happened (#2148).
#
# The two alternatives are exhaustive rather than a widened net: the rail draws
# the ▲ indicator exactly when it has hidden rows above (hiddenAbove > 0), and
# when it has not, row 0 — the header — is on screen. One of them is always
# present the instant the rail paints, whichever session the restored selection
# lands on. Neither is present before af paints, so this still fails a boot that
# did not happen: a shell prompt, a build error, or a hung launch matches
# nothing here. (`Agent Factory`, the sidebar's title chip, is NOT sufficient on
# its own — it is static chrome that paints with the frame, so it would go on
# matching an af that rendered its shell and no rail at all.)
#
# The indicator is anchored to the sidebar's left edge — only leading whitespace
# may precede it — the same anchor _af_tab_count uses, so a workspace pane that
# happens to print "▲ 3 more" cannot satisfy the boot gate: pane text is always
# preceded on its row by the sidebar columns and the pane's own border. `▲` is
# matched as a literal byte sequence rather than inside a bracket expression,
# which the sandbox's C/POSIX locale would match byte-wise (#1994).
: "${_AF_RAIL_MARKER:=Sessions \(|^[[:space:]]*▲ [0-9]+ more}"

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

    # Suppress every first-time help overlay BEFORE the TUI reads state (see
    # AF_DRIVER_HELP_SEEN above for the bits, and for clearing one on purpose).
    printf '{\n  "help_screens_seen": %s\n}\n' "$AF_DRIVER_HELP_SEEN" > "$AGENT_FACTORY_HOME/state.json"

    # Fresh driver session. Kill ONLY our own named session — never the
    # server (the container hosts the daemon's sessions too).
    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    tmux new-session -d -s "$AF_DRIVER_SESSION" -x "$AF_DRIVER_COLS" -y "$AF_DRIVER_ROWS"
    # A detached session ignores -x/-y under `window-size latest`; pin it so the
    # TUI reads the requested geometry on startup (#1174 item 2). Honors any
    # AF_DRIVER_COLS/ROWS override set before af_boot, including tiny sizes.
    # af_resize verifies the geometry took, so a bad size fails the boot loudly.
    af_resize "$AF_DRIVER_COLS" "$AF_DRIVER_ROWS" || return 1

    af_send_literal "cd $AF_DRIVER_REPO && ${AF_DRIVER_LAUNCH_ENV:+$AF_DRIVER_LAUNCH_ENV }$bin"
    af_send Enter
    af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'af first frame' || return 1
    af_wait_for "$_AF_RAIL_MARKER" "$AF_DRIVER_TIMEOUT" \
        'instance rail painted (header or scrolled ▲ indicator)' || return 1
    af_focus_tree
}

# ----------------------------------------------------------------------------
# Session-name identity on the rail (#7485).
#
# The rail ellipsizes a title that outgrows its cell: a row draws
# ` ▾  a-very-long-… ●`, never the full name — so a literal-name match times
# out on any descriptive title. The helpers therefore key on the RENDERED
# title: the name itself while it fits, else its kept prefix + '…', derived
# from the rail's own width math — never a hard-coded length — so an
# af_resize keeps them correct at a different geometry.
#
# Two sessions can legitimately draw the SAME truncated title
# ('a-very-long-…' on two rows). A helper that cannot tell its target's row
# from the sibling's must fail loudly and name the collision, never pick one
# silently.
# ----------------------------------------------------------------------------

# _af_rail_title_avail — the title cell's width in display cells at the
# current driver geometry. ui/layout/grid.go clamps the rail itself to
# clamp(⌊cols·25%⌋, 22, 36); sidebar.contentWidth drops the row's 2-cell
# padding; ui/tree/render.go's Render then subtracts the 3-cell right status
# affix, the 3-cell ' ▾ ' prefix and its separator — so the title gets the
# rail width minus 9 cells. A wider title renders its first avail-1 cells +
# '…' (runewidth.Truncate's tail).
_af_rail_title_avail() {
    local w=$(( AF_DRIVER_COLS * 25 / 100 ))
    (( w < 22 )) && w=22
    (( w > 36 )) && w=36
    printf '%s' "$(( w - 9 ))"
}

# _af_draw_title <name> — print "<cell-width>\n<title bytes>" for the title
# cell the rail draws for <name>: the name itself while its display width
# fits _af_rail_title_avail, else the kept grapheme prefix + '…'. The model
# replicates render.go's draw under the sandbox's non-EastAsian locale —
# runewidth's per-cluster truncation for the ellipsis, then the layout
# contract's x/ansi clip to the cell — over a codepoint model generated
# from the very libraries the TUI links (go-runewidth, uax29 graphemes,
# displaywidth; see the table comment). Returns 1 when a codepoint cannot
# be measured reliably — invalid UTF-8, a rune whose two measures disagree
# (ZWJ, emoji modifiers, tag letters, VS16), or an Indic linker whose
# conjunct clustering needs the full InCB tables — so callers refuse
# loudly, naming the name, rather than guess a rendering.
_af_draw_title() {
    local name="$1" avail
    avail="$(_af_rail_title_avail)"
    printf '%s\n' "$name" | LC_ALL=C awk -v avail="$avail" '# _af_cell_draw — replicates the TUI rail title draw under the sandbox
# non-EastAsian locale:
#   1. runewidth.Truncate-style: while the total runewidth would exceed
#      avail-1, drop trailing GRAPHEME CLUSTERS and append "…" (U+2026).
#   2. x/ansi contract-clip: keep the longest grapheme prefix of rt whose
#      x/ansi (displaywidth) width fits avail.
# Prints one line "<cells> <title>" — the x/ansi cell width of the drawn
# title and its bytes (empty when the name draws only padding).
# Exit 1 with the offending char on stderr when the input is not valid UTF-8
# or contains a codepoint the two measures are known to score differently
# (ZWJ, VS16, emoji modifiers, tag chars) or an InCB=Linker virama whose
# conjunct segmentation we cannot model without the full InCB tables.
BEGIN {
	for (i = 0; i < 256; i++) ORD[sprintf("%c", i)] = i
	# Codepoint range tables (decimal lo-hi[-class]), generated from the
	# measure stack the rail itself links: mattn/go-runewidth v0.0.23 for
	# the ellipsis truncation, clipperhouse/displaywidth v0.11.0 (via
	# charmbracelet/x/ansi v0.11.7) for the contract cell width, and
	# clipperhouse/uax29 v2.7.0 for grapheme clustering.
	TW = 	"0-31-0 127-159-0 173-173-0 768-879-0 1155-1161-0 1425-1469-2 1471-1471-2 1473-1474-2 1476-1477-2 1479-1479-2 " \
		"1536-1541-2 1552-1562-2 1564-1564-2 1611-1631-2 1648-1648-2 1750-1757-2 1759-1764-2 1767-1768-2 1770-1773-2 " \
		"1807-1807-0 1809-1809-2 1840-1866-2 1958-1968-2 2027-2035-0 2045-2045-2 2070-2073-2 2075-2083-2 2085-2087-2 " \
		"2089-2093-2 2137-2139-2 2192-2193-2 2200-2207-2 2250-2306-2 2362-2362-2 2364-2364-2 2369-2376-2 2381-2381-2 " \
		"2385-2391-2 2402-2403-2 2433-2433-2 2492-2492-2 2497-2500-2 2509-2509-2 2530-2531-2 2558-2558-2 2561-2562-2 " \
		"2620-2620-2 2625-2626-2 2631-2632-2 2635-2637-2 2641-2641-2 2672-2673-2 2677-2677-2 2689-2690-2 2748-2748-2 " \
		"2753-2757-2 2759-2760-2 2765-2765-2 2786-2787-2 2810-2815-2 2817-2817-2 2876-2876-2 2879-2879-2 2881-2884-2 " \
		"2893-2893-2 2901-2902-2 2914-2915-2 2946-2946-2 3008-3008-2 3021-3021-2 3072-3072-0 3076-3076-0 3132-3132-2 " \
		"3134-3136-2 3142-3144-2 3146-3149-2 3157-3158-2 3170-3171-2 3201-3201-2 3260-3260-2 3263-3263-2 3270-3270-2 " \
		"3276-3277-2 3298-3299-2 3315-3315-1 3328-3329-0 3387-3388-2 3393-3396-2 3405-3405-2 3426-3427-2 3457-3457-2 " \
		"3530-3530-2 3538-3540-2 3542-3542-2 3633-3633-2 3636-3642-2 3655-3662-2 3761-3761-2 3764-3772-2 3784-3790-2 " \
		"3864-3865-2 3893-3893-2 3895-3895-2 3897-3897-2 3953-3966-2 3968-3972-2 3974-3975-2 3981-3991-2 3993-4028-2 " \
		"4038-4038-2 4141-4144-2 4146-4151-2 4153-4154-2 4157-4158-2 4184-4185-2 4190-4192-2 4209-4212-2 4226-4226-2 " \
		"4229-4230-2 4237-4237-2 4253-4253-2 4352-4447-5 4957-4959-0 5906-5908-2 5938-5939-2 5970-5971-2 6002-6003-2 " \
		"6068-6069-2 6071-6077-2 6086-6086-2 6089-6099-2 6109-6109-2 6155-6159-0 6277-6278-2 6313-6313-2 6432-6434-2 " \
		"6439-6440-2 6450-6450-2 6457-6459-2 6679-6680-2 6683-6683-2 6742-6742-2 6744-6750-2 6752-6752-2 6754-6754-2 " \
		"6757-6764-2 6771-6780-2 6783-6783-0 6832-6862-0 6863-6877-1 6880-6891-1 6912-6915-2 6964-6964-2 6966-6970-2 " \
		"6972-6972-2 6978-6978-2 7019-7027-0 7040-7041-2 7074-7077-2 7080-7081-2 7083-7085-2 7142-7142-2 7144-7145-2 " \
		"7149-7149-2 7151-7153-2 7212-7219-2 7222-7223-2 7376-7378-2 7380-7392-2 7394-7400-2 7405-7405-2 7412-7412-2 " \
		"7416-7417-2 7616-7679-0 8203-8207-0 8232-8238-0 8288-8292-2 8294-8297-2 8298-8303-0 8400-8432-0 8986-8987-5 " \
		"9001-9002-5 9193-9196-5 9200-9200-5 9203-9203-5 9725-9726-5 9748-9749-5 9776-9783-5 9800-9811-5 9855-9855-5 " \
		"9866-9871-5 9875-9875-5 9889-9889-5 9898-9899-5 9917-9918-5 9924-9925-5 9934-9934-5 9940-9940-5 9962-9962-5 " \
		"9970-9971-5 9973-9973-5 9978-9978-5 9981-9981-5 9989-9989-5 9994-9995-5 10024-10024-5 10060-10060-5 " \
		"10062-10062-5 10067-10069-5 10071-10071-5 10133-10135-5 10160-10160-5 10175-10175-5 11035-11036-5 " \
		"11088-11088-5 11093-11093-5 11503-11505-0 11647-11647-2 11744-11775-0 11904-11929-5 11931-12019-5 " \
		"12032-12245-5 12272-12329-5 12330-12333-4 12334-12350-5 12353-12438-5 12441-12442-0 12443-12543-5 " \
		"12549-12591-5 12593-12686-5 12688-12773-5 12783-12830-5 12832-12871-5 12880-42124-5 42128-42182-5 " \
		"42607-42610-0 42612-42621-0 42654-42655-0 42736-42737-0 43010-43010-2 43014-43014-2 43019-43019-2 " \
		"43045-43046-2 43052-43052-2 43204-43205-2 43232-43249-0 43263-43263-2 43302-43309-2 43335-43345-2 " \
		"43360-43388-5 43392-43394-2 43443-43443-2 43446-43449-2 43452-43453-2 43493-43493-2 43561-43566-2 " \
		"43569-43570-2 43573-43574-2 43587-43587-2 43596-43596-2 43644-43644-2 43696-43696-2 43698-43700-2 " \
		"43703-43704-2 43710-43711-2 43713-43713-2 43756-43757-2 43766-43766-2 44005-44005-2 44008-44008-2 " \
		"44013-44013-2 44032-55203-5 63744-64255-5 64286-64286-2 65024-65039-0 65040-65049-5 65056-65071-0 " \
		"65072-65106-5 65108-65126-5 65128-65131-5 65279-65279-0 65281-65376-5 65504-65510-5 65529-65531-0 " \
		"65534-65535-0 66045-66045-0 66272-66272-2 66422-66426-0 68097-68099-2 68101-68102-2 68108-68111-2 " \
		"68152-68154-2 68159-68159-2 68325-68326-2 68900-68903-2 69291-69292-0 69373-69375-2 69446-69456-0 " \
		"69506-69509-0 69633-69633-2 69688-69702-2 69744-69744-2 69747-69748-2 69759-69761-2 69811-69814-2 " \
		"69817-69818-2 69821-69821-2 69826-69826-2 69837-69837-2 69888-69890-2 69927-69931-2 69933-69940-2 " \
		"70003-70003-2 70016-70017-2 70070-70078-2 70089-70092-2 70095-70095-2 70191-70193-2 70196-70196-2 " \
		"70198-70199-2 70206-70206-2 70209-70209-2 70367-70367-2 70371-70378-2 70400-70401-0 70459-70460-0 " \
		"70464-70464-2 70502-70508-0 70512-70516-0 70712-70719-2 70722-70724-2 70726-70726-2 70750-70750-2 " \
		"70835-70840-2 70842-70842-2 70847-70848-2 70850-70851-2 71090-71093-2 71100-71101-2 71103-71104-2 " \
		"71132-71133-2 71219-71226-2 71229-71229-2 71231-71232-2 71339-71339-2 71341-71341-2 71344-71349-2 " \
		"71351-71351-2 71453-71455-2 71458-71461-2 71463-71467-2 71727-71735-2 71737-71738-2 71995-71996-2 " \
		"71998-71998-2 72003-72003-2 72148-72151-2 72154-72155-2 72160-72160-2 72193-72202-2 72243-72248-2 " \
		"72251-72254-2 72263-72263-2 72273-72278-2 72281-72283-2 72330-72342-2 72344-72345-2 72752-72758-2 " \
		"72760-72765-2 72767-72767-2 72850-72871-2 72874-72880-2 72882-72883-2 72885-72886-2 73009-73014-2 " \
		"73018-73018-2 73020-73021-2 73023-73029-2 73031-73031-2 73104-73105-2 73109-73109-2 73111-73111-2 " \
		"73459-73460-2 73472-73473-2 73526-73530-2 73536-73536-2 73538-73538-2 78896-78912-2 78919-78933-2 " \
		"92912-92916-0 92976-92982-2 94031-94031-2 94095-94098-2 94176-94179-5 94180-94180-4 94192-94198-5 " \
		"94208-101589-5 101631-101662-5 101760-101874-5 110576-110579-5 110581-110587-5 110589-110590-5 " \
		"110592-110882-5 110898-110898-5 110928-110930-5 110933-110933-5 110948-110951-5 110960-111355-5 " \
		"113821-113822-2 113824-113827-2 118528-118573-0 118576-118598-0 119141-119142-1 119143-119145-0 " \
		"119149-119154-1 119155-119162-2 119163-119170-0 119173-119179-0 119210-119213-0 119362-119364-0 " \
		"119552-119638-5 119648-119670-5 121344-121398-2 121403-121452-2 121461-121461-2 121476-121476-2 " \
		"121499-121503-2 121505-121519-2 122880-122886-0 122888-122904-0 122907-122913-0 122915-122916-0 " \
		"122918-122922-0 123023-123023-0 123184-123190-2 123566-123566-2 123628-123631-2 124140-124143-2 " \
		"125136-125142-0 125252-125258-2 126980-126980-5 127183-127183-5 127374-127374-5 127377-127386-5 " \
		"127462-127487-3 127488-127490-5 127504-127547-5 127552-127560-5 127568-127569-5 127584-127589-5 " \
		"127744-127776-5 127789-127797-5 127799-127868-5 127870-127891-5 127904-127946-5 127951-127955-5 " \
		"127968-127984-5 127988-127988-5 127992-128062-5 128064-128064-5 128066-128252-5 128255-128317-5 " \
		"128331-128334-5 128336-128359-5 128378-128378-5 128405-128406-5 128420-128420-5 128507-128591-5 " \
		"128640-128709-5 128716-128716-5 128720-128722-5 128725-128728-5 128732-128735-5 128747-128748-5 " \
		"128756-128764-5 128992-129003-5 129008-129008-5 129292-129338-5 129340-129349-5 129351-129535-5 " \
		"129648-129660-5 129664-129674-5 129678-129734-5 129736-129736-5 129741-129756-5 129759-129770-5 " \
		"129775-129784-5 131072-196605-5 196608-262141-5 917505-917505-2 917536-917631-2 917760-917999-0 "
	TJ = 	"768-879 1155-1161 1425-1469 1471-1471 1473-1474 1476-1477 1479-1479 1552-1562 1611-1631 1648-1648 1750-1756 " \
		"1759-1764 1767-1768 1770-1773 1809-1809 1840-1866 1958-1968 2027-2035 2045-2045 2070-2073 2075-2083 2085-2087 " \
		"2089-2093 2137-2139 2199-2207 2250-2273 2275-2307 2362-2364 2366-2383 2385-2391 2402-2403 2433-2435 2492-2492 " \
		"2494-2500 2503-2504 2507-2509 2519-2519 2530-2531 2558-2558 2561-2563 2620-2620 2622-2626 2631-2632 2635-2637 " \
		"2641-2641 2672-2673 2677-2677 2689-2691 2748-2748 2750-2757 2759-2761 2763-2765 2786-2787 2810-2815 2817-2819 " \
		"2876-2876 2878-2884 2887-2888 2891-2893 2901-2903 2914-2915 2946-2946 3006-3010 3014-3016 3018-3021 3031-3031 " \
		"3072-3076 3132-3132 3134-3140 3142-3144 3146-3149 3157-3158 3170-3171 3201-3203 3260-3260 3262-3268 3270-3272 " \
		"3274-3277 3285-3286 3298-3299 3315-3315 3328-3331 3387-3388 3390-3396 3398-3400 3402-3405 3415-3415 3426-3427 " \
		"3457-3459 3530-3530 3535-3540 3542-3542 3544-3551 3570-3571 3633-3633 3635-3642 3655-3662 3761-3761 3763-3772 " \
		"3784-3790 3864-3865 3893-3893 3895-3895 3897-3897 3902-3903 3953-3972 3974-3975 3981-3991 3993-4028 4038-4038 " \
		"4141-4151 4153-4158 4182-4185 4190-4192 4209-4212 4226-4226 4228-4230 4237-4237 4253-4253 4957-4959 5906-5909 " \
		"5938-5940 5970-5971 6002-6003 6068-6099 6109-6109 6155-6157 6159-6159 6277-6278 6313-6313 6432-6443 6448-6459 " \
		"6679-6683 6741-6750 6752-6752 6754-6754 6757-6780 6783-6783 6832-6877 6880-6891 6912-6916 6964-6980 7019-7027 " \
		"7040-7042 7073-7085 7142-7155 7204-7223 7376-7378 7380-7400 7405-7405 7412-7412 7415-7417 7616-7679 8204-8205 " \
		"8400-8432 11503-11505 11647-11647 11744-11775 12330-12335 12441-12442 42607-42610 42612-42621 42654-42655 " \
		"42736-42737 43010-43010 43014-43014 43019-43019 43043-43047 43052-43052 43136-43137 43188-43205 43232-43249 " \
		"43263-43263 43302-43309 43335-43347 43392-43395 43443-43456 43493-43493 43561-43574 43587-43587 43596-43597 " \
		"43644-43644 43696-43696 43698-43700 43703-43704 43710-43711 43713-43713 43755-43759 43765-43766 44003-44010 " \
		"44012-44013 64286-64286 65024-65039 65056-65071 65438-65439 66045-66045 66272-66272 66422-66426 68097-68099 " \
		"68101-68102 68108-68111 68152-68154 68159-68159 68325-68326 68900-68903 68969-68973 69291-69292 69370-69375 " \
		"69446-69456 69506-69509 69632-69634 69688-69702 69744-69744 69747-69748 69759-69762 69808-69818 69826-69826 " \
		"69888-69890 69927-69940 69957-69958 70003-70003 70016-70018 70067-70080 70089-70092 70094-70095 70188-70199 " \
		"70206-70206 70209-70209 70367-70378 70400-70403 70459-70460 70462-70468 70471-70472 70475-70477 70487-70487 " \
		"70498-70499 70502-70508 70512-70516 70584-70592 70594-70594 70597-70597 70599-70602 70604-70608 70610-70610 " \
		"70625-70626 70709-70726 70750-70750 70832-70851 71087-71093 71096-71104 71132-71133 71216-71232 71339-71351 " \
		"71453-71455 71458-71467 71724-71738 71984-71989 71991-71992 71995-71998 72000-72000 72002-72003 72145-72151 " \
		"72154-72160 72164-72164 72193-72202 72243-72249 72251-72254 72263-72263 72273-72283 72330-72345 72544-72551 " \
		"72751-72758 72760-72767 72850-72871 72873-72886 73009-73014 73018-73018 73020-73021 73023-73029 73031-73031 " \
		"73098-73102 73104-73105 73107-73111 73459-73462 73472-73473 73475-73475 73524-73530 73534-73538 73562-73562 " \
		"78912-78912 78919-78933 90398-90415 92912-92916 92976-92982 94031-94031 94033-94087 94095-94098 94180-94180 " \
		"94192-94193 113821-113822 118528-118573 118576-118598 119141-119145 119149-119154 119163-119170 119173-119179 " \
		"119210-119213 119362-119364 121344-121398 121403-121452 121461-121461 121476-121476 121499-121503 " \
		"121505-121519 122880-122886 122888-122904 122907-122913 122915-122916 122918-122922 123023-123023 " \
		"123184-123190 123566-123566 123628-123631 124140-124143 124398-124399 124643-124643 124646-124646 " \
		"124654-124655 124661-124661 125136-125142 125252-125258 127995-127999 917536-917631 917760-917999 "
	TP = 	"1536-1541 1757-1757 1807-1807 2192-2193 2274-2274 3406-3406 69821-69821 69837-69837 70082-70083 70609-70609 " \
		"71999-71999 72001-72001 72324-72329 73030-73030 73474-73474 "
	TL = 	"2381-2381 2509-2509 2765-2765 2893-2893 3149-3149 3405-3405 4153-4153 6098-6098 6752-6752 6980-6980 7083-7083 " \
		"43456-43456 43766-43766 68159-68159 69939-69939 70608-70608 71998-71998 72263-72263 72345-72345 73538-73538 "
	TG = 	"0-31 127-159 173-173 1564-1564 6158-6158 8203-8203 8206-8207 8232-8238 8288-8303 65279-65279 65520-65531 " \
		"78896-78911 113824-113827 119155-119162 917504-917535 917632-917759 918000-921599 "
	TPL = 	"4352-4447 43360-43388 "
	TPV = 	"4448-4519 55216-55238 93539-93539 93543-93546 "
	TPT = 	"4520-4607 55243-55291 "
	TCX = 	"4352-4447 43360-43388 "
	TCV = 	"4448-4519 55216-55238 93539-93539 93543-93546 "
	TCT = 	"4520-4607 55243-55291 "
	nW = tabw(TW, WLO, WHI, WCL)
	nJB = tab(TJ, JBLO, JBHI)
	nPP = tab(TP, PLO, PHI)
	nLK = tab(TL, LKLO, LKHI)
	nGB = tab(TG, GBLO, GBHI)
	nPL = tab(TPL, PLLO, PLHI)
	nPV = tab(TPV, PVLO, PVHI)
	nPT = tab(TPT, PTLO, PTHI)
	nCX = tab(TCX, CXLO, CXHI)
	nCV = tab(TCV, CVLO, CVHI)
	nCT = tab(TCT, CTLO, CTHI)
	# class -> (rww, xaw)
	split("0 0 1 1 2 2", RWV, " ")
	split("0 1 0 2 0 2", XAV, " ")
}
function tab(str, lo, hi,   n, i, j, a, b) {
	n = split(str, a, " ")
	j = 0
	for (i = 1; i <= n; i++) {
		if (a[i] == "") continue
		split(a[i], b, "-")
		j++
		lo[j] = b[1] + 0
		hi[j] = b[2] + 0
	}
	return j
}
function tabw(str, lo, hi, cl,   n, i, j, a, b) {
	n = split(str, a, " ")
	j = 0
	for (i = 1; i <= n; i++) {
		if (a[i] == "") continue
		split(a[i], b, "-")
		j++
		lo[j] = b[1] + 0
		hi[j] = b[2] + 0
		cl[j] = b[3] + 0
	}
	return j
}
function inr(cp, lo, hi, n,   a, b, m) {
	a = 1; b = n
	while (a <= b) {
		m = int((a + b) / 2)
		if (cp < lo[m]) b = m - 1
		else if (cp > hi[m]) a = m + 1
		else return m
	}
	return 0
}
function widths(cp,   m) {
	m = inr(cp, WLO, WHI, nW)
	RWv = (m ? RWV[WCL[m] + 1] : 1)
	XAv = (m ? XAV[WCL[m] + 1] : 1)
}
function jamocp(cp,   d) {
	# AC00..D7A3 syllables join only after L, like L/LV/LVT jamo (GB6).
	if (cp >= 44032 && cp <= 55203) return 1
	if (inr(cp, CXLO, CXHI, nCX)) return 1
	if (inr(cp, CVLO, CVHI, nCV)) return 2
	if (inr(cp, CTLO, CTHI, nCT)) return 3
	return 0
}
function jamoprev(cp,   d) {
	if (cp >= 44032 && cp <= 55203) {
		d = (cp - 44032) % 28
		return (d == 0 ? 2 : 3)                     # LV : LVT
	}
	if (inr(cp, PLLO, PLHI, nPL)) return 1          # L
	if (inr(cp, PVLO, PVHI, nPV)) return 2          # V|LV
	if (inr(cp, PTLO, PTHI, nPT)) return 3          # T|LVT
	return 0
}
function refuse(cp) {
	# measure-disagreement chars (RW vs x/ansi score these differently) and
	# InCB=Linker viramas (conjunct segmentation we cannot model).
	if (cp == 8205 || cp == 65039 || (cp >= 127995 && cp <= 127999) || (cp >= 917536 && cp <= 917631)) return 1
	return inr(cp, LKLO, LKHI, nLK)
}
{
	n = split($0, ch, "")
	nc = 0; i = 1
	while (i <= n) {
		b = ORD[ch[i]]
		if (b < 128) { len = 1; cp = b }
		else if (b >= 194 && b <= 223) {
			len = 2
			if (i + 1 > n) bad()
			c1 = ORD[ch[i + 1]]
			if (c1 < 128 || c1 > 191) bad()
			cp = (b - 192) * 64 + c1 - 128
		} else if (b >= 224 && b <= 239) {
			len = 3
			if (i + 2 > n) bad()
			c1 = ORD[ch[i + 1]]; c2 = ORD[ch[i + 2]]
			lo1 = (b == 224 ? 160 : (b == 237 ? 144 : 128))
			if (c1 < lo1 || c1 > 191 || c2 < 128 || c2 > 191) bad()
			cp = (b - 224) * 4096 + (c1 - 128) * 64 + c2 - 128
		} else if (b >= 240 && b <= 244) {
			len = 4
			if (i + 3 > n) bad()
			c1 = ORD[ch[i + 1]]; c2 = ORD[ch[i + 2]]; c3 = ORD[ch[i + 3]]
			lo1 = (b == 240 ? 144 : 128)
			hi1 = (b == 244 ? 143 : 191)
			if (c1 < lo1 || c1 > hi1 || c2 < 128 || c2 > 191 || c3 < 128 || c3 > 191) bad()
			cp = (b - 240) * 262144 + (c1 - 128) * 4096 + (c2 - 128) * 64 + c3 - 128
		} else bad()
		if (refuse(cp)) die(cp)
		nc++
		CP[nc] = cp
		OFF[nc] = i
		i += len
	}
	ENDOFF = i - 1

	# Grapheme clustering + per-cluster (rww, xaw) widths.
	ncl = 0; open = 0; force = 0; rirun = 0; pj = 0; pend = 0; headnz = 0
	for (i = 1; i <= nc; i++) {
		cp = CP[i]
		widths(cp)
		if (inr(cp, GBLO, GBHI, nGB)) {
			if (open) { open = 0 }
			ncl++
			CS[ncl] = OFF[i]; CE[ncl] = OFF[i] - 1 + clen(i)
			CW[ncl] = RWv; CX[ncl] = XAv
			force = 1; rirun = 0; pj = 0; pend = 0
			continue
		}
		j = jamocp(cp)
		join = 0
		if (!force) {
			if (pend) join = 1
			else if (inr(cp, JBLO, JBHI, nJB)) join = 1
			else if (cp >= 127462 && cp <= 127487 && prevri && rirun % 2 == 1) join = 1
			else if (j == 1 && pj == 1) join = 1                        # X after L
			else if (j == 2 && (pj == 1 || pj == 2)) join = 1           # V after L|VLV
			else if (j == 3 && (pj == 2 || pj == 3)) join = 1           # T after VLV|TLVT
		}
		if (open && join) {
			if (RWv > 0 && !headnz) { CW[ncl] += RWv; headnz = 1 }
		} else {
			ncl++; open = 1; headnz = 0
			CS[ncl] = OFF[i]
			CW[ncl] = 0; CX[ncl] = XAv
			if (RWv > 0) { CW[ncl] = RWv; headnz = 1 }
		}
		CE[ncl] = OFF[i] - 1 + clen(i)
		force = 0
		rirun = (cp >= 127462 && cp <= 127487) ? rirun + 1 : 0
		prevri = (cp >= 127462 && cp <= 127487)
		pj = jamoprev(cp)
		pend = inr(cp, PLO, PHI, nPP)
	}

	# rt: runewidth truncation to avail with "…" tail.
	rwtot = 0
	for (i = 1; i <= ncl; i++) rwtot += CW[i]
	nk = ncl; ell = 0
	if (rwtot > avail) {
		sum = 0; nk = 0
		for (i = 1; i <= ncl; i++) {
			if (sum + CW[i] > avail - 1) break
			sum += CW[i]; nk = i
		}
		ell = 1
	}
	# x/ansi contract clip: longest cluster prefix of rt within avail cells.
	xw = 0; out = ""; keep = 0
	for (i = 1; i <= nk; i++) {
		if (xw + CX[i] > avail) break
		xw += CX[i]
		out = out substr($0, CS[i], CE[i] - CS[i] + 1)
		keep = i
	}
	# The clip is a prefix of rt: "…" survives only when every cluster
	# before it did.
	if (ell && keep == nk && xw + 1 <= avail) { xw++; out = out "\342\200\246" }
	# One line, "<width> <title>": the space separator survives the
	# trailing-newline strip of $( ) where a second line would not — a
	# zero-cell title (a name that draws only padding) reads as "" not "0".
	printf "%d %s\n", xw, out
}
function clen(i) { return (i == nc ? ENDOFF + 1 : OFF[i + 1]) - OFF[i] }
function bad() { print "invalid utf-8" > "/dev/stderr"; exit 1 }
function die(cp) { printf "cannot measure U+%04X\n", cp > "/dev/stderr"; exit 1 }'
}

# _af_render_title <name> — only the drawn title bytes (for messages and
# collision checks). Returns 1 exactly when _af_draw_title does.
_af_render_title() {
    local out
    out="$(_af_draw_title "$1")" || return 1
    printf '%s\n' "${out#* }"
}

# _af_title_cell <name> — the complete title cell as bytes: the drawn title
# plus its padding out to avail. 'foo ' and 'foo' print the same cell — the
# trailing space is padding — and cell equality is what the eye sees.
_af_title_cell() {
    local out xw t
    out="$(_af_draw_title "$1")" || return 1
    xw="${out%% *}"
    t="${out#* }"
    printf '%s%*s\n' "$t" "$(( $(_af_rail_title_avail) - xw ))" ""
}

# _af_row_re <name> <arrow-ere> [icon-ere] — an ERE matching a rail row
# whose COMPLETE title cell is what the rail draws for <name>: the row's
# leading space, the ' <arrow> ' prefix and separator, the title's literal
# bytes, exactly the padding cells the rail draws out to the cell
# boundary, then the status affix — a space, the two-cell status glyph,
# its trailing space. tmux capture strips trailing whitespace, so a row
# whose tail is all padding and a blank status (or whose affix ends the
# line) loses those cells entirely: the pattern therefore also accepts
# the title's bytes ending the line, and folds the title's own trailing
# spaces into the padding count — 'foo ' and 'foo' are the same cell. A
# name that is only a whitespace-delimited PREFIX of a visible title —
# 'foo' beside 'foo bar' — still cannot match: the cells ' bar'
# occupies are neither padding, the affix, nor end-of-line. <icon-ere>
# defaults to every status glyph the rail draws (● ○ ◌ ▧ ◆ ! or a blank
# cell). Returns 1 when <name>'s rendering cannot be derived — refuse,
# don't guess.
_af_row_re() {
    local name="$1" arrow="$2" icon="${3:-●|○|◌|▧|◆|!| }" out xw t tws pad
    out="$(_af_draw_title "$name")" || return 1
    xw="${out%% *}"
    t="${out#* }"
    # Only ' ' folds into padding — it is the only char the rail pads with
    # and the only ws tmux capture strips; a locale-aware class could eat
    # a trailing NBSP the draw actually emits.
    tws="${t##*[^ ]}"
    t="${t%"$tws"}"
    pad=$(( $(_af_rail_title_avail) - xw + ${#tws} ))
    printf '[[:space:]](%s)[[:space:]]+(%s)( {%d} (%s)([[:space:]]|$)|[[:space:]]*$)' \
        "$arrow" "$(_af_regex_escape "$t")" "$pad" "$icon"
}

# _af_common_prefix_chars <a> <b> — the length in characters of the leading
# run the two strings share.
_af_common_prefix_chars() {
    printf '%s\n%s\n' "$1" "$2" | LC_ALL=C awk '
        function splitchars(s, out,   i, n, ch, k) {
            n = split(s, ch, "")
            for (i = 1; i <= n; i++) {
                if (ch[i] ~ /[\200-\277]/) { out[k] = out[k] ch[i] }
                else { k++; out[k] = ch[i] }
            }
            return k
        }
        NR == 1 { na = splitchars($0, A); next }
        { nb = splitchars($0, B)
          for (i = 1; i <= na && i <= nb; i++) { if (A[i] != B[i]) break }
          print i - 1; exit }'
}

# _af_titles_collide <a> <b> — could the rail draw the SAME complete title
# cell for <a> and <b>? Exact when both renderings derive: equal cells
# collide, anything else does not. When one side cannot be measured
# the answer stays conservative without guessing a rendering: a derived
# non-truncated cell can only come from the identical name, so it never
# collides with a different one; a derived 'P…' cell collides with any
# name carrying the literal prefix P; two underivable names collide on
# any shared leading character.
_af_titles_collide() {
    local a="$1" b="$2" ra rb ta tb ca cb
    ra="$(_af_draw_title "$a" 2>/dev/null)" || ra=""
    rb="$(_af_draw_title "$b" 2>/dev/null)" || rb=""
    ta="${ra#* }"
    tb="${rb#* }"
    if [ -n "$ra" ] && [ -n "$rb" ]; then
        ca="$(_af_title_cell "$a")"
        cb="$(_af_title_cell "$b")"
        [ "$ca" = "$cb" ]
        return
    fi
    if [ -n "$ra" ]; then
        case "$ta" in
            *…) [ "$b" != "${ta%…}" ] && [ "${b#"${ta%…}"}" != "$b" ] ;;
            *)  return 1 ;;
        esac
        return
    fi
    if [ -n "$rb" ]; then
        case "$tb" in
            *…) [ "$a" != "${tb%…}" ] && [ "${a#"${tb%…}"}" != "$a" ] ;;
            *)  return 1 ;;
        esac
        return
    fi
    [ "$(_af_common_prefix_chars "$a" "$b")" -gt 0 ]
}

# _af_session_titles — every session title in the driver repo's project, one
# per line. Non-zero when the store cannot be read (no af binary, repo, or jq)
# so the caller can fall back to what the screen shows. Authoritative for
# ambiguity when it answers: a collider scrolled off the rail is still a
# collider.
_af_session_titles() {
    local bin out
    command -v jq >/dev/null 2>&1 || return 1
    bin="$(_af_resolve_bin)"
    [ -n "$bin" ] && [ -d "$AF_DRIVER_REPO" ] || return 1
    out="$(cd "$AF_DRIVER_REPO" 2>/dev/null \
        && timeout 15 "$bin" sessions list --json 2>/dev/null)" || return 1
    printf '%s' "$out" | jq -re \
        'if type == "array" then .[] elif (.data | type) == "array" then .data[] else empty end | .title // empty'
}

# _af_unique_rendering <helper> <name> <max-rows> — the ambiguity guard: a
# helper must never resolve <name> to one of several rows drawing the same
# title cell. <max-rows> is how many rail rows may legitimately draw it —
# 0 before a create (<name>'s own row is not up yet), 1 elsewhere (the
# target's own row). The session list names the colliders when it answers;
# the rows on screen decide when it cannot. A <name> whose title cell
# cannot be derived is refused outright — the helpers could not tell its
# row from any other anyway.
_af_unique_rendering() {
    local helper="$1" name="$2" maxrows="$3" rendered row_re titles t colliders="" n
    # _af_draw_title's stderr — "cannot measure U+XXXX" — is left visible so
    # the refusal names the offending codepoint, not just the name.
    if ! rendered="$(_af_render_title "$name")"; then
        _af_fail "$helper: '$name' contains a character the driver cannot measure in display cells; refusing to match its rail row rather than guess"
        return 1
    fi
    row_re="$(_af_row_re "$name" '▾|▸')" || {
        _af_fail "$helper: cannot derive the rail's title cell for '$name'"
        return 1
    }
    if titles="$(_af_session_titles)"; then
        while IFS= read -r t; do
            [ -n "$t" ] && [ "$t" != "$name" ] || continue
            _af_titles_collide "$name" "$t" && colliders="${colliders:+$colliders, }'$t'"
        done <<< "$titles"
        if [ -n "$colliders" ]; then
            _af_fail "$helper: '$name' is ambiguous on the rail: it renders as '${rendered}' and so does session(s) $colliders; rename for a unique leading prefix"
            return 1
        fi
    fi
    n="$(af_capture | grep -cE -- "$row_re" || true)"
    n="${n//[^0-9]/}"
    if [ "${n:-0}" -gt "$maxrows" ]; then
        _af_fail "$helper: '$name' is ambiguous on the rail: $n session rows draw '${rendered}'; rename for a unique leading prefix"
        return 1
    fi
    return 0
}

# af_new_instance <name> — create an instance and wait until it is ready
# (its row shows the ● ready dot). Cheap-instance config makes `bash` the
# program, so ready arrives in seconds.
af_new_instance() {
    local name="$1" row_re
    [ -n "$name" ] || { _af_fail "af_new_instance: name required"; return 1; }
    row_re="$(_af_row_re "$name" '▾|▸' '●')" || {
        _af_fail "af_new_instance: cannot derive the rail's title cell for '$name'"
        return 1
    }
    # Refuse to add a row the rail cannot tell apart from a sibling's, and
    # re-check after the wait so a collider created meanwhile cannot slip the
    # ready match onto the wrong row.
    _af_unique_rendering af_new_instance "$name" 0 || return 1
    af_ensure_nav
    af_focus_tree || return 1
    af_send n
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" 'name prompt' || return 1
    af_wait_gone '[Ss]ession [Nn]ame:' 1 'old name prompt label' || return 1
    af_send_literal "$name"
    af_send Enter
    af_wait_for "$row_re" "$AF_DRIVER_TIMEOUT" "instance '${name}' ready" || return 1
    _af_unique_rendering af_new_instance "$name" 1 || return 1
}

# af_select <name> — put the tree cursor on one of <name>'s TAB rows (so it is
# the *real* GetSelectedInstance(), not merely display-selected). Robust to any
# starting cursor position: anchor at the first live tab stop (k is idempotent
# there), then step down until BOTH conditions hold — <name>'s parent row
# carries the ▾ selected/expanded arrow AND the menu advertises an
# instance-scoped verb (`D delete session`, or legacy `D kill`).
#
# The two-part success condition is the #1174-item-1 / #1199 fix. The sticky
# ▾ is a DISPLAY-selection: a SINGLE auto-selected instance renders ▾ while the
# tree cursor still sits on the `Instances` section header, so GetSelected-
# Instance() is nil and every cursor-driven verb (o attach, D delete session, and
# af_attach/af_open_pane which run after af_select) silently no-ops. A ▾-only
# check returns on iteration 0 in that case — a false positive that can make a
# play-test wrongly "pass" a nav action that never fired. `D delete session` appears in
# the menu ONLY when an instance is actually under the cursor (non-nil
# GetSelectedInstance()), so requiring it forces `j` past the header/title rows
# until the cursor truly lands on an actionable tab row.
af_select() {
    local name="$1" i screen menu row_re
    [ -n "$name" ] || { _af_fail "af_select: name required"; return 1; }
    row_re="$(_af_row_re "$name" '▾')" || {
        _af_fail "af_select: cannot derive the rail's title cell for '$name'"
        return 1
    }
    # A row the rail cannot tell apart from a sibling's must not be scanned
    # to: an identical ▾ on either of two sessions is silently the wrong pick.
    _af_unique_rendering af_select "$name" 1 || return 1
    af_ensure_nav
    af_focus_tree || return 1
    # Check the current actionable selection BEFORE sending navigation. Re-selecting
    # the same row otherwise queues 30 k presses, then can accept the old frame
    # while those keys are still in flight (#4056), changing selection after return.
    # Scan down from the anchored top tab stop, evaluating the ready condition
    # AFTER every `j` — including the final one. The old loop captured, checked,
    # THEN pressed `j`, so the state produced by the 40th (boundary) `j` was
    # never evaluated: a row that only became actionable on that last step was
    # missed and af_select reported a false selection failure (#1759). Check the
    # current position (i=-1), then the anchor (i=0, no `j`), then after each
    # of the 40 downward steps.
    #
    # Neither the anchor nor a `j` is judged one fixed poll after it is sent
    # (#7197). A TUI working through 30 queued k presses can still be drawing
    # the old row when the first `j` goes out, and then act on the rest of the
    # k presses and that `j` between two captures: the scan never sees the
    # first session, which is the cursor's stop after the anchor, and reports a
    # false selection failure. Or a stale frame shows the target with a `j`
    # still on its way, af_select returns, and that `j` moves the cursor off
    # the target. So wait for the anchor to land (_af_anchor_at_first_session),
    # then for each `j` to show (_af_select_state; _af_wait_key_effect explains
    # the bound).
    #
    # The scan used to cover 40 stops below an anchor 30 stops above the
    # cursor, so it reached 10 stops past where the cursor started. The anchor
    # now climbs all the way to the first session, in as many bursts of 30 as
    # that takes, so the scan grows by 30 stops a burst to keep that reach.
    local limit=40
    for ((i = -1; i <= limit; i++)); do
        if [ "$i" -eq 0 ]; then
            _af_anchor_at_first_session || return 1
            limit=$((10 + 30 * _AF_ANCHOR_BURSTS))
        elif [ "$i" -gt 0 ]; then
            local before
            before="$(printf '%s\n' "$screen" | _af_select_state)"
            af_send j
            _af_wait_key_effect _af_select_state "$before" || true
        fi
        screen="$(af_capture)"
        printf '%s\n' "$screen" | grep -qE -- "$row_re" || continue
        menu="$(printf '%s\n' "$screen" | _af_menu_row)"
        if printf '%s\n' "$menu" | grep -qE -- "$_AF_INSTANCE_VERB_RE"; then
            return 0
        fi
        # The target is display-selected (▾) but the footer is the pane menu, not
        # 'D delete session': moving the cursor onto an instance that already has an open
        # workspace pane auto-focuses that pane, which replaces the tree footer
        # AND stops the remaining scan keys from driving the tree (#1996). The
        # instance IS selected — the pane could only be focused because of that —
        # so restore tree focus and finalize instead of scanning fruitlessly to
        # the boundary and reporting a false selection failure. This is NOT the
        # #1759 sticky-header false positive: that shows the tree menu ('n new'),
        # never the pane menu.
        if printf '%s\n' "$menu" | grep -qE -- 'hide pane'; then
            af_focus_tree || return 1
            if af_capture | grep -qE -- "$row_re"; then
                return 0
            fi
        fi
    done
    _af_log "could not select '${name}' (need ▾ on its parent row AND cursor-on-tab, i.e. 'D delete session' (or legacy 'D kill') in the status-bar menu)"
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

# _af_pane_header_row — the one screen line that renders EVERY visible pane's
# identity, or "" when the workspace draws no pane box.
#
# Panes are laid out side by side in a SINGLE horizontal row (ui/layout/grid.go
# Solve), and each pane's frame puts its ` <title> · <tab> ` header on the
# first line inside the frame — so all visible pane headers share one screen
# row, immediately below the workspace box's top border. Anchoring on that
# border (the first `╭`, `┌`, or `╔` — the sidebar has no left border, so the
# workspace box owns the first one) and taking the NEXT line yields the whole
# visible-pane identity set in one string. Current workspace panes use a square
# `┌`; older captures used the rounded `╭`, so the driver accepts both.
#
# Corners are matched as literal alternatives, not a bracket expression: the sandbox runs a
# C/POSIX locale where a bracket expression would match only the first byte of
# the 3-byte glyph (cf. _af_tab_count's `(├|└)` alternation note).
_af_pane_header_row() {
    af_capture | awk '/╭|┌|╔/ { if ((getline line) > 0) print line; exit }'
}

# af_hide_pane — hide the focused pane back to the background (nothing is
# killed). Precondition: a pane is focused. Syncs on the visible-pane set
# CHANGING, captured via the pane header row.
#
# It used to wait for the tree menu (`n new`), i.e. it assumed hiding a pane
# always hands focus back to the tree. It does not: hidePane (app/handle_panes.go)
# lands focus on the pane that takes the hidden one's slot and only falls back
# to the tree when the LAST pane goes away. So in any multi-pane flow the first
# `x` hid the pane correctly, focus advanced to the remaining pane, `n new`
# never appeared, and the driver reported a false TIMEOUT failure (#1822).
# Multi-pane is not exotic: below MultiPaneMinWidth (110 cols) only one pane is
# visible at a time, so at the driver's default 100x30 a second open pane is
# merely auto-hidden — still open, and it takes focus on the next `x`.
#
# Waiting for "the tree menu OR the pane menu" would be worse than useless: the
# pane menu is ALREADY on screen before `x`, so the wait would return on the
# first poll and pass even if `x` did nothing.
#
# The header row is the right marker because hiding the focused pane ALWAYS
# mutates the visible pane set — another pane takes the slot (its header
# replaces the hidden one's) or the workspace empties (the row goes blank) —
# and it is menu-independent, which matters: `x hide pane` is in the menu's
# hintDropOrder and disappears on narrow terminals, so any marker built on it
# would false-FAIL exactly where the layout is tightest, whereas `n new` alone
# false-FAILS whenever a pane remains.
#
# It also closes a false PASS the old marker had: with focus on the TREE, `x`
# is a no-op and `n new` was already showing, so af_hide_pane returned 0
# immediately and reported success for a hide that never happened. The header
# row is unchanged in that case, so the call now fails.
af_hide_pane() {
    local before; before="$(_af_pane_header_row)"
    af_send x
    local deadline; deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while [ "$(_af_pane_header_row)" = "$before" ]; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "pane not hidden (pane header row still '$before'); is a pane focused?"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
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

# af_send_line <text> [timeout_s] — paste <text> into a FULL-SCREEN attach and
# submit it, but ONLY after confirming the whole paste has landed in the input
# line. In a full-screen attach, af_send_literal streams the bytes through the
# raw attach proxy into the nested session; a following bare `af_send Enter` can
# overtake the still-draining paste and execute a TRUNCATED command (#1995) — the
# harness analog of submit.go's load-bearing drain wait, which the attach path
# lacks. The driver can SEE the attached screen, so it asserts delivery instead
# of guessing. Use this rather than a bare `af_send_literal … ; af_send Enter`
# whenever the submitted command matters. Precondition: a full-screen attach is
# active (af_attach). Returns non-zero if the line could not be delivered.
#
# It FAILS CLOSED (#2147). The old version confirmed best-effort: on an
# unconfirmed paste it logged a note and pressed Enter anyway. That is strictly
# worse than not delivering at all, because the attach path really does drop
# bytes — the first paste after an attach frequently lands only a prefix (a
# multiple of 32 bytes, the attach input pump's read size; reproduced on 5 of 9
# consecutive live 80x24 attempts). Submitting a prefix of a QUOTED command
# leaves bash in its `>` continuation prompt, and every later scripted command
# becomes a continuation line — one unconfirmed paste silently corrupts the whole
# rest of the scenario. A testing helper must never turn an unconfirmed partial
# paste into shell input; the caller needs an error, not a wrecked session.
#
# Recovery before refusal: the drop is transient rather than terminal — the input
# path stays alive, and a re-paste lands in full (5 of 5 live retries). So each
# attempt clears the input line first (so the re-paste cannot concatenate onto
# the partial one) and re-pastes, and only after AF_DRIVER_SEND_LINE_ATTEMPTS
# failures does it clear the line one last time and fail loudly, having pressed
# no Enter at all. The [timeout_s] budget is per attempt.
af_send_line() {
    local text="$1" timeout="${2:-8}" attempt baseline
    # Nothing to confirm in an empty/whitespace-only line: submitting it is
    # harmless and there is no echo to match against.
    if [ -z "$(printf '%s' "$text" | tr -d '[:space:]')" ]; then
        af_send Enter
        return 0
    fi
    for attempt in $(seq 1 "$AF_DRIVER_SEND_LINE_ATTEMPTS"); do
        _af_clear_input_line
        # Measured AFTER the clear and BEFORE the paste, so what gets confirmed
        # is this paste's own echo and not an identical command still sitting in
        # the scrollback from an earlier af_send_line.
        baseline="$(_af_line_echo_count "$text")"
        af_send_literal "$text" || return 1
        if _af_wait_for_line_echo "$text" "$timeout" "$baseline"; then
            af_send Enter
            return 0
        fi
        _af_log "af_send_line: attempt ${attempt}/${AF_DRIVER_SEND_LINE_ATTEMPTS} landed only PART of the line; clearing and re-pasting"
    done
    _af_log "----- last screen -----"
    af_capture >&2
    _af_log "-----------------------"
    _af_clear_input_line
    _af_fail "af_send_line: only part of the line ever echoed after ${AF_DRIVER_SEND_LINE_ATTEMPTS} attempts; NOT submitting it — a partial line would corrupt the attached shell (#2147). Line: $text"
    return 1
}

# af_attach — attach the selected instance full-screen (`o`). Precondition: an
# instance is selected. Records the attach-client baseline so af_detach can
# prove the client was reaped. Syncs on the TUI chrome vanishing — AND on the
# nav status bar vanishing with it (#6750 lesson: absence of one marker is not
# a state). 'Agent Factory' is only the rail's title chip, clipped entirely at
# a narrow rail, so its absence alone cannot vouch for the attach; only the
# raw attach proxy owns the WHOLE terminal, status bar and all — a modal that
# covered the chrome would still leave the status bar behind, and nothing else
# erases it.
af_attach() {
    af_ensure_nav
    af_focus_tree || return 1
    _af_client_count > "$AF_DRIVER_STATE_DIR/attach_baseline"
    af_send o
    af_wait_gone 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'full-screen attach' || return 1
    local deadline screen menu
    deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while :; do
        screen="$(af_capture)"
        menu="$(printf '%s\n' "$screen" | _af_menu_row)"
        [ -z "$menu" ] && break
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log 'attach: chrome is gone but the nav status bar never left — not a real attach'
            printf '%s\n' "$screen" >&2
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
    # Attach intentionally discards terminal probe bytes for ~50ms; let that
    # window pass so an immediate scripted af_detach is not swallowed.
    sleep "$AF_DRIVER_POLL"
}

# af_detach [raw_sequence] — detach from a full-screen attach and, the
# anti-#1157-flake part, wait until the attach client is actually reaped (the
# count returns to the pre-attach baseline). A leaked client fails here.
#
# With no argument it presses the configured detach key ($AF_DRIVER_DETACH_KEY,
# Ctrl-W) ONCE. Sending it once is the point: the key is pressed once by a real
# user, so a retry here would paper over exactly the #1832 class of bug (a detach
# key the client never recognizes) and report it green.
#
# With an argument, those raw bytes are injected instead — how a terminal whose
# keyboard mode the pane program has upgraded actually reports the same
# keypress. See the #1832 selftest steps.
af_detach() {
    if [ $# -gt 0 ]; then
        af_send_literal "$1" || return 1
    else
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

# af_new_tab — choose the default terminal item from the new-tab picker (`t`).
# Precondition: an instance is selected. Syncs on the picker and tab-child count.
af_new_tab() {
    af_ensure_nav
    af_focus_tree || return 1
    local before; before="$(_af_tab_count)"
    af_send t
    af_wait_for 'New tab' "$AF_DRIVER_TIMEOUT" 'new-tab picker' || return 1
    af_send Enter
    local deadline; deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while [ "$(_af_tab_count)" -le "$before" ]; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "new tab did not appear (was $before)"; return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# _af_confirm_tab_delete — accept the existing dialog only once it is visible.
# The title from app/handle_tabs.go is `Delete tab %q from session %q?`.
# Match its literal prefix (escaped as ERE), since long names wrap the rest.
_af_confirm_tab_delete() {
    local title_re
    title_re="$(_af_regex_escape 'Delete tab "')"
    if ! af_wait_for "$title_re" "$AF_DRIVER_TIMEOUT" 'delete-tab confirmation'; then
        _af_fail 'delete-tab confirmation did not appear; no consent sent'
        return 1
    fi
    af_send y
}

# af_close_tab — delete the active non-agent tab: w, wait for consent, then y.
# Precondition: a deletable tab is active. This requests deletion, unlike hiding
# a pane. Sync on the tab-child count falling only after accepting the dialog.
af_close_tab() {
    af_ensure_nav
    af_focus_tree || return 1
    local before; before="$(_af_tab_count)"
    af_send w || return 1
    _af_confirm_tab_delete || return 1
    local deadline; deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while [ "$(_af_tab_count)" -ge "$before" ]; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "confirmed tab deletion did not reduce the count (still $before)"; return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# _af_tab_count — number of tab-child rows the selected instance shows. Only
# the selected instance auto-expands, so this counts its tabs. Matches the
# sidebar tab-child shape — tree connector, tab index, then a non-empty name —
# for ANY name, so CLI-created custom-named tabs count too (#1561), not just the
# built-in Preview/Terminal/Diff.
#
# The match is ANCHORED to the sidebar: the connector must sit at the sidebar's
# left column, preceded only by the row's leading indentation (whitespace). The
# sidebar is the leftmost block and has no left border, and tab rows render as
# `<indent><connector> <n> <label>`, so a real tab row always has only
# whitespace before its connector. A workspace pane, by contrast, is a
# rounded-border box to the RIGHT of the sidebar, so tree-like text INSIDE a
# pane (e.g. a shell printing `├ 1 foo`) is always preceded on its line by the
# sidebar columns and the pane's own border (`│`, or `║` when that pane is the
# interactive one — _AF_PANE_BORDER_FS) — never only whitespace — so it
# is NOT counted. Without the `^[[:space:]]*` anchor such pane output would
# re-introduce af_close_tab false timeouts from the opposite direction (#1561
# review). The `[0-9]+` index additionally keeps this off instance rows, which
# carry no `<n> <name>` prefix.
#
# The connectors are matched with an alternation `(├|└)`, not a bracket
# expression `[├└]`: the sandbox container runs a C/POSIX locale where GNU
# grep matches a bracket expression byte-by-byte, so `[├└]` would only match
# the first byte of the 3-byte glyph and the following `[[:space:]]` could
# never line up — the count silently stayed 0. Alternation matches each
# connector's literal byte sequence regardless of locale.
_af_tab_count() {
    af_capture | grep -cE '^[[:space:]]*(├|└)[[:space:]]+[0-9]+[[:space:]]+[^[:space:]]' || true
}

# _AF_TASKS_RUN_HINT remains the edit-mode assertion marker. It is deliberately
# not the open/close anchor: the default list footer hides secondary actions
# behind `?`, and an empty list has no run action at all (#3995). The editor
# opens on Name, a text field, and a text field's footer lists no verb at all
# (#6400): r would type into it. The run action shows from the first selector
# stop, the Trigger row one Tab on, as "r run now ·" or, below 42 cells,
# "r run ·".
: "${_AF_TASKS_RUN_HINT:=(^|[^[:alnum:]])r run( now)? ·}"

# _AF_TASKS_TEXT_STOP_HINT is the editor's footer while a text field has focus:
# "type to edit · tab next · esc list", which compacts to "tab next · esc list"
# below 34 cells. Waiting on it proves the editor is up on a text stop before a
# step refutes the run hint there, so the refute cannot pass on a frame that
# has not painted yet.
: "${_AF_TASKS_TEXT_STOP_HINT:=(^|[^[:alnum:]])tab next · esc list}"

# _AF_TASKS_LIST_TITLE is only the list/edit discriminator used by the self-test
# before revealing secondary list actions. It is NOT the open/close anchor: the
# edit form scrolls its title away to keep the focused field visible at the
# supported 80x10 floor.
: "${_AF_TASKS_LIST_TITLE:=│[[:space:]]+Tasks[[:space:]]+│}"

# _AF_TASKS_EDIT_TITLE is the edit form's title row ("Edit task <id>"). It is
# the one marker that does not move when a hint row is reworded, so a complete
# dialog showing it is recognized whatever its footer says. It only SUPPLEMENTS
# the footer: the form scrolls the title away at the 80x10 floor, where the
# pinned footer is all that identifies the dialog. The bare list title is not
# accepted this way — "Tasks" alone is ordinary output text (review on #3995);
# the row must also carry the task id token that only the editor prints.
: "${_AF_TASKS_EDIT_TITLE:=│[[:space:]]+Edit task[[:space:]]+[^[:space:]]+.*│}"

# _AF_TASKS_FOOTER uses combinations unique to Tasks among the built-in dialogs:
# recovery/list has n new or ? actions, edit/watch has x toggle, and create has
# tab[/shift+tab] fields. Do not accept enter edit or D delete alone: Hooks and
# the project picker share them. These mirror task_pane_recovery.go and
# task_pane_edit.go; unlike the title, the footer survives the 80x10 form scroll.
#
# The edit form's action row is contextual and width-tiered (#5538), so no verb
# fragment anchors it any more: a text stop lists no verb (#6400), only
# "type to edit · tab next · esc list", compacting to "tab next · esc list" and
# then to "esc list" alone, and no tier says "x toggle". What every tier at
# every focus stop keeps is esc's destination, `esc list`, the label the
# product pins at all nine stops
# (TestTaskPaneEditActionFooterFollowsFocus) and one no other built-in dialog
# prints (they say esc back, esc cancel or esc close), so it anchors the edit
# form. The one row without it, a selector stop's narrowest
# "r run · [✓] x toggle · D del · esc", still carries `x toggle` (the glyph is
# the task's enabled state, #7135, and leads the verb so this fragment holds).
: "${_AF_TASKS_FOOTER:=(n new ·|[?] actions ·|x toggle ·|tab(/shift[+]tab)? fields ·).*esc.*│|(^|[^[:alnum:]])esc list.*│}"

# _AF_TASKS_FRAME_TOP/BOTTOM are the rounded borders shared by every task
# dialog mode. A workspace pane uses square corners, and its OWN │ borders can
# surround output whose complete line is "Tasks". Requiring a task footer (or
# the editor's id-bearing title) between both rounded borders prevents that
# pane text from satisfying the open marker or keeping the close marker alive
# (review on #3995).
#
# Group the repeated horizontal glyph: under the sandbox's C/POSIX locale,
# `─+` repeats only the last byte of the UTF-8 sequence, while `(─)+` repeats
# the whole glyph (the same locale trap documented by _AF_PANE_BORDER_FS).
: "${_AF_TASKS_FRAME_TOP:=╭(─)+╮}"
: "${_AF_TASKS_FRAME_BOTTOM:=╰(─)+╯}"

# _af_tasks_dialog_has <content-regex> reads a captured screen on stdin and
# succeeds only when the marker is enclosed by one complete rounded dialog. The
# scanner tracks every candidate independently, including nested frames and
# edges sharing a row. Only its own matching bottom can complete a candidate;
# incomplete enclosing frames cannot hide a complete dialog inside them.
# Force byte semantics for mawk/gawk parity, then count prefix characters by
# removing UTF-8 continuation bytes. This makes a multibyte sidebar glyph count
# as one column on both edges; double-width CJK glyphs remain out of scope.
_af_tasks_dialog_has() {
    local content_re="$1"
    LC_ALL=C awk -v top_re="$_AF_TASKS_FRAME_TOP" -v bottom_re="$_AF_TASKS_FRAME_BOTTOM" \
        -v content_re="$content_re" '
        # Slice by character columns while preserving UTF-8 for content_re.
        # A continuation byte belongs to the preceding character, including
        # all three bytes of each bounding │. Columns are zero-based.
        # Columns are code points, not cells; a wide character left of the
        # overlay shifts the geometry. Session titles in the selftest fixtures
        # are ASCII by design; this is an accepted harness limitation.
        function columns(text) {
            gsub(/[\200-\277]/, "", text)
            return length(text)
        }
        function frame_row(row, left, width,    i, column, byte, result) {
            column = -1
            result = ""
            for (i = 1; i <= length(row); i++) {
                byte = substr(row, i, 1)
                if (byte !~ /[\200-\277]/) { column++ }
                if (column >= left && column < left + width) {
                    result = result byte
                }
            }
            return result
        }
        {
            rest = $0
            offset = 0
            # Register every top edge, even inside an incomplete enclosing frame.
            while (match(rest, top_re)) {
                edge_start = offset + RSTART - 1
                edge_bytes = RLENGTH
                left = columns(substr($0, 1, edge_start))
                width = columns(substr(rest, RSTART, edge_bytes))
                id = ++candidate
                active[id] = NR
                starts[id] = left
                widths[id] = width
                offset = edge_start + edge_bytes
                rest = substr($0, offset + 1)
            }
            for (id in active) {
                if (NR <= active[id]) { continue }
                if (frame_row($0, starts[id], widths[id]) ~ content_re) { footer[id] = NR }
                rest = $0
                offset = 0
                # A foreign bottom on this row must not hide this frame edge.
                while (match(rest, bottom_re)) {
                    edge_start = offset + RSTART - 1
                    edge_bytes = RLENGTH
                    left = columns(substr($0, 1, edge_start))
                    width = columns(substr(rest, RSTART, edge_bytes))
                    if (left == starts[id] && width == widths[id]) {
                        if (footer[id] > active[id] && footer[id] < NR) { found = 1 }
                        delete active[id]
                        delete starts[id]
                        delete widths[id]
                        delete footer[id]
                        break
                    }
                    offset = edge_start + edge_bytes
                    rest = substr($0, offset + 1)
                }
            }
        }
        END {
            exit found ? 0 : 1
        }
    '
}

_af_tasks_overlay_visible() {
    # Before #4006 lands, the empty recovery has no pinned task footer.
    # Accept its task-specific title only inside the same complete rounded
    # frame; the first-run workspace shares the action sentence, not this title.
    # The edit form's title is accepted the same way (see _AF_TASKS_EDIT_TITLE).
    _af_tasks_dialog_has "$_AF_TASKS_FOOTER|$_AF_TASKS_EDIT_TITLE|│[[:space:]]+No tasks[[:space:]]+│"
}

_af_tasks_list_visible() {
    _af_tasks_dialog_has "$_AF_TASKS_LIST_TITLE"
}

# _af_task_dialog_open — a task dialog of ANY mode is up: the footer-anchored
# overlay marker OR the titled list. The footer is width-tiered and CLIPPED (the
# list's "r run now · x toggle · D delete · ? back · esc" is 46 cells, so at the
# 40x10 floor its "esc" is cut off and the footer marker misses), which let a
# still-open list read as "closed" (#6017). The list title is not width-tiered,
# so closing/waiting is decided on either signal.
_af_task_dialog_open() {
    local screen; screen="$(cat)"
    _af_tasks_overlay_visible <<<"$screen" || _af_tasks_list_visible <<<"$screen"
}

# _af_dialog_frame_present — a COMPLETE rounded modal frame holds the screen:
# one ╭─…─╮ top edge, its matching ╰─…─╯ bottom edge at the same column and
# width, and text between them. Dialogs are the only rounded chrome
# (radius-dialog=1; panes and controls stay square), so this is
# content-independent "some modal is up" — the positive form of
# af_assert_no_overlay's second clause and the only close evidence a footer
# fragment can never fake (#6750).
_af_dialog_frame_present() {
    _af_tasks_dialog_has '[[:alnum:]]'
}

# af_assert_no_overlay [label] — fail, printing the screen, when ANY rounded
# modal frame holds the view at the CURRENT size (#6017). Every step that is
# about to send a global key (C, ",", m, n, …) calls this first, so an overlay
# that swallowed the previous step's close is reported as the real precondition
# failure rather than as a dropped key and a long timeout. Content-independent
# on purpose: it recognises a complete rounded frame, not one dialog's footer,
# so a clipped or width-tiered footer cannot hide an open overlay.
af_assert_no_overlay() {
    local label="${1:-before a global key}" screen
    screen="$(af_capture)"
    if _af_task_dialog_open <<<"$screen" || _af_dialog_frame_present <<<"$screen"; then
        _af_log "an overlay holds focus ${label} (${AF_DRIVER_COLS:-?}x${AF_DRIVER_ROWS:-?}):"
        printf '%s\n' "$screen" >&2
        _af_fail "an overlay holds focus ${label} — a global key would be swallowed by it"
        return 1
    fi
}

# _af_wait_for_screen_state <predicate> <0|1> <timeout_s> <label> — poll the
# capture until <predicate> (a function that reads a screen on stdin and
# answers "the surface is up") reports <expected>.
_af_wait_for_screen_state() {
    local predicate="$1" expected="$2" timeout="$3" label="$4" screen visible
    local deadline; deadline=$(( $(_af_now) + timeout ))
    while :; do
        screen="$(af_capture)"
        visible=0
        "$predicate" <<<"$screen" && visible=1
        if [ "$visible" -eq "$expected" ]; then
            return 0
        fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log "TIMEOUT ${timeout}s waiting for: $label"
            printf '%s\n' "$screen" >&2
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# _af_wait_for_tasks_overlay_state <0|1> [timeout_s] [label] — wait for the
# dialog-specific frame/footer pair to disappear or appear.
_af_wait_for_tasks_overlay_state() {
    _af_wait_for_screen_state _af_task_dialog_open "$@" || return 1
}

# _af_dismiss_overlays <predicate> <label> [budget_s] [max_sends] — Escape away
# a modal surface until <predicate> (reads a screen on stdin, answers "the
# surface is still up") reports it gone — and require the gone state on TWO
# consecutive captures so a torn mid-repaint frame cannot pass for closed.
#
# This is the #6750 close contract: "closed" is POSITIVE evidence that the
# surface's frame is off the screen, never the absence of one footer fragment
# and never a frame captured before its key took effect. The frame check is
# symmetric for the same reason: a capture can predate the last send's
# repaint, so a surface must persist on two CONSECUTIVE sightings before an
# Escape is (re)sent — otherwise one send would fire on the stale frame and
# land AFTER the close, on whatever surface replaced it. Each landing Escape
# closes exactly one level — a sub-mode's Escape abandons to the dialog's
# list, a list-level Escape closes it — so the loop sends while the surface
# stays up, retrying a send that never landed. On budget or send-cap
# exhaustion it dumps the screen and returns 1.
_af_dismiss_overlays() {
    local predicate="$1" label="$2" budget="${3:-12}" max_sends="${4:-6}"
    local screen sends=0 clean=0 up=0 deadline
    deadline=$(( $(_af_now) + budget ))
    while :; do
        screen="$(af_capture)"
        if "$predicate" <<<"$screen"; then
            clean=0
            up=$((up + 1))
            if [ "$up" -gt 1 ] && [ "$sends" -lt "$max_sends" ]; then
                af_send Escape
                sends=$((sends + 1))
                up=0
            fi
        else
            up=0
            clean=$((clean + 1))
            [ "$clean" -ge 2 ] && return 0
        fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log "TIMEOUT ${budget}s dismissing ${label} (${sends} Escape(s) sent; the surface is still up)"
            printf '%s\n' "$screen" >&2
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# af_open_tasks — open the task-manager overlay (`m`) and synchronize on its
# rounded frame plus pinned footer, which survives scrolling/collapsed actions.
af_open_tasks() {
    af_ensure_nav
    af_send m
    _af_wait_for_tasks_overlay_state 1 "$AF_DRIVER_TIMEOUT" 'tasks overlay' || return 1
}

# _af_tasks_surface_up — the task surface still holds focus: the task dialog
# itself OR any modal frame on top of it — `D` raises the shared deletion
# confirmation, whose `n/esc cancel` footer none of the task markers
# recognize. Without the frame clause an af_close_tasks during a pending
# delete returned "closed" while a dialog one `y` away from deleting a task
# still held the screen.
_af_tasks_surface_up() {
    local screen; screen="$(cat)"
    _af_task_dialog_open <<<"$screen" || _af_dialog_frame_present <<<"$screen"
}

# af_close_tasks — dismiss the tasks overlay (Escape). The overlay is
# two-level — in edit mode the first Escape drops back to the titled list, so
# a second Escape closes it; a pending deletion confirmation takes a cancel
# Escape back to the list first — and the dismiss loop syncs on the surface
# being gone, retrying an Escape that never landed.
af_close_tasks() {
    _af_dismiss_overlays _af_tasks_surface_up 'tasks overlay' 12 4 || return 1
}

# The config editor's own marker. Anchored on the hint row rather than a key
# name so it cannot match arbitrary pane text (the #1757 lesson): "esc close" is
# the editor's last footer fragment, and the leading separator keeps it from
# matching a session whose output happens to contain the words. It identifies
# the editor ONLY at its list level — a value field edit and the pane's other
# sub-modes each print their own footer — and it is meaningless outside the
# editor's rounded frame, so helpers never match it bare (#6750).
: "${_AF_CONFIG_HINT:=(^|[^[:alnum:]])esc close}"

# _af_config_list_visible — the config editor's `esc close` footer INSIDE its
# complete rounded frame: "open at the dismissable list level" as positive
# evidence rather than a bare fragment anywhere on screen (#6750 — a foreign
# dialog's footer or copied pane text could carry the same words).
_af_config_list_visible() {
    _af_tasks_dialog_has "$_AF_CONFIG_HINT"
}

# af_open_config — open the global config editor overlay (","). Syncs on the
# editor's list footer inside its frame rather than sleeping.
af_open_config() {
    af_ensure_nav
    af_send ','
    _af_wait_for_screen_state _af_config_list_visible 1 "$AF_DRIVER_TIMEOUT" 'config editor' || return 1
}

# af_close_config — dismiss the config editor (Escape). "Closed" means the
# editor's rounded FRAME is off the screen, verified on two consecutive
# captures — never merely that `esc close` is absent (#6750). The footer is
# the wrong marker for this: a value field's footer is `↵ save · esc cancel`,
# so the capture taken right after the send — before the Escape took effect —
# already lacked `esc close`, and the old helper returned "closed" while a live
# field still held the screen. Every later "global" key then typed into it —
# the issue's `> emacs,,,,`. The editor is two-level like the tasks overlay —
# a sub-mode Escape abandons to the list, a list-level Escape closes — so the
# shared dismiss loop drives it.
af_close_config() {
    _af_dismiss_overlays _af_dialog_frame_present 'config editor' 12 6 || return 1
}

# af_add_task <name> — seed a minimal valid cron task through the overlay's
# create form, so a later af_open_tasks lands on the EDIT-mode overlay, whose
# footer shows `r run` one Tab past Name (the #1757 flow). Opens the tasks overlay (list
# mode when empty), fills name + cron expression + prompt (the path field
# pre-fills with the launch repo, a valid git repo), submits, and waits for the
# new task's row to appear. Leaves the overlay open in list mode.
af_add_task() {
    local name="${1:-selftest-task}" name_re
    name_re="$(_af_regex_escape "$name")"
    af_open_tasks || return 1
    af_send n
    # Sync on the form title, not the footer: the footer's "enter create" hint
    # collapses away at a narrow overlay width, but "New task" is always shown.
    af_wait_for 'New task' "$AF_DRIVER_TIMEOUT" 'task create form' || return 1
    af_send_literal "$name"
    af_send Tab                    # name -> trigger selector (cron is default)
    af_send Tab                    # -> trigger value (cron expression)
    af_send_literal '0 3 * * *'
    af_send Tab                    # -> prompt (Enter here inserts a newline)
    af_send_literal 'selftest prompt'
    af_send Tab                    # -> target; Enter from here submits the form
    af_send Enter
    af_wait_for "${name_re}" "$AF_DRIVER_TIMEOUT" "task '${name}' created" || return 1
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
    local name="$1" line row_re
    row_re="$(_af_row_re "$name" '▾|▸')" || {
        _af_fail "af_click_instance: cannot derive the rail's title cell for '$name'"
        return 1
    }
    _af_unique_rendering af_click_instance "$name" 1 || return 1
    line="$(af_capture | grep -nE "$row_re" | head -1 | cut -d: -f1)"
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

# af_quit — quit the TUI back to a shell prompt. Dismisses interactive mode,
# status-bar forms (the naming form draws no rounded frame — the first Escape
# below cancels it) and any overlay FOR REAL first — one Escape may only
# abandon an overlay's sub-mode, leaving `q` to type into a live field instead
# of quitting (#6750) — then syncs on the on-quit log line.
af_quit() {
    af_ensure_nav
    af_send Escape
    _af_dismiss_overlays _af_dialog_frame_present 'pre-quit overlay' 4 4 || true
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
    af_wait_for "$_AF_RAIL_MARKER" "$AF_DRIVER_TIMEOUT" \
        'instance rail painted (relaunch)' || return 1
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
    local name="$1" row_re
    row_re="$(_af_row_re "$name" '▾')" || {
        _af_fail "af_expect_selected: cannot derive the rail's title cell for '$name'"
        return 1
    }
    # A ▾ match the rail cannot attribute to <name> over a same-rendered
    # sibling must not pass silently.
    _af_unique_rendering af_expect_selected "$name" 1 || return 1
    af_assert_screen "$row_re" \
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
