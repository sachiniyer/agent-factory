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
: "${_AF_RAIL_MARKER:=Instances \(|^[[:space:]]*▲ [0-9]+ more}"

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

    af_send_literal "cd $AF_DRIVER_REPO && $bin"
    af_send Enter
    af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'af first frame' || return 1
    af_wait_for "$_AF_RAIL_MARKER" "$AF_DRIVER_TIMEOUT" \
        'instance rail painted (header or scrolled ▲ indicator)' || return 1
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
    af_wait_gone '[Ss]ession [Nn]ame:' 1 'old name prompt label' || return 1
    af_send_literal "$name"
    af_send Enter
    af_wait_for "${name}.*●" "$AF_DRIVER_TIMEOUT" "instance '${name}' ready" || return 1
}

# af_select <name> — put the tree cursor on one of <name>'s TAB rows (so it is
# the *real* GetSelectedInstance(), not merely display-selected). Robust to any
# starting cursor position: anchor at the first live tab stop (k is idempotent
# there), then step down until BOTH conditions hold — <name>'s parent row
# carries the ▾ selected/expanded arrow AND the menu advertises an
# instance-scoped verb (`D kill`).
#
# The two-part success condition is the #1174-item-1 / #1199 fix. The sticky
# ▾ is a DISPLAY-selection: a SINGLE auto-selected instance renders ▾ while the
# tree cursor still sits on the `Instances` section header, so GetSelected-
# Instance() is nil and every cursor-driven verb (o attach, D kill, and
# af_attach/af_open_pane which run after af_select) silently no-ops. A ▾-only
# check returns on iteration 0 in that case — a false positive that can make a
# play-test wrongly "pass" a nav action that never fired. `D kill` appears in
# the menu ONLY when an instance is actually under the cursor (non-nil
# GetSelectedInstance()), so requiring it forces `j` past the header/title rows
# until the cursor truly lands on an actionable tab row.
af_select() {
    local name="$1" i screen name_re
    [ -n "$name" ] || { _af_fail "af_select: name required"; return 1; }
    name_re="$(_af_regex_escape "$name")"
    af_ensure_nav
    af_focus_tree || return 1
    for i in $(seq 1 30); do af_send k; done
    sleep "$AF_DRIVER_POLL"
    # Scan down from the anchored top tab stop, evaluating the ready condition
    # AFTER every `j` — including the final one. The old loop captured, checked,
    # THEN pressed `j`, so the state produced by the 40th (boundary) `j` was
    # never evaluated: a row that only became actionable on that last step was
    # missed and af_select reported a false selection failure (#1759). `seq 0 40`
    # checks the anchored position first (i=0, no `j`), then re-checks after each
    # of the 40 downward steps, with a settle poll between the `j` and the
    # capture so the post-`j` frame is the one we inspect.
    for i in $(seq 0 40); do
        if [ "$i" -gt 0 ]; then
            af_send j
            sleep "$AF_DRIVER_POLL"
        fi
        screen="$(af_capture)"
        printf '%s\n' "$screen" | grep -qE -- "▾[[:space:]]+${name_re}([[:space:]]|\$)" || continue
        if printf '%s\n' "$screen" | grep -qE -- 'D kill'; then
            return 0
        fi
        # The target is display-selected (▾) but the footer is the pane menu, not
        # 'D kill': moving the cursor onto an instance that already has an open
        # workspace pane auto-focuses that pane, which replaces the tree footer
        # AND stops the remaining scan keys from driving the tree (#1996). The
        # instance IS selected — the pane could only be focused because of that —
        # so restore tree focus and finalize instead of scanning fruitlessly to
        # the boundary and reporting a false selection failure. This is NOT the
        # #1759 sticky-header false positive: that shows the tree menu ('n new'),
        # never the pane menu.
        if printf '%s\n' "$screen" | grep -qE -- 'hide pane'; then
            af_focus_tree || return 1
            if af_capture | grep -qE -- "▾[[:space:]]+${name_re}([[:space:]]|\$)"; then
                return 0
            fi
        fi
    done
    _af_log "could not select '${name}' (need ▾ on its parent row AND cursor-on-tab, i.e. 'D kill' in the menu)"
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
# border (the first `╭` on screen — the sidebar has no left border, so the
# workspace box owns the first one) and taking the NEXT line yields the whole
# visible-pane identity set in one string.
#
# `╭` is matched as a literal, not a bracket expression: the sandbox runs a
# C/POSIX locale where a bracket expression would match only the first byte of
# the 3-byte glyph (cf. _af_tab_count's `(├|└)` alternation note).
_af_pane_header_row() {
    af_capture | awk '/╭/ { if ((getline line) > 0) print line; exit }'
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

# _AF_TASKS_RUN_HINT — the run-action affordance that marks the task overlay as
# open. `m` drops STRAIGHT into the selected task's edit form when a task exists
# (#1249), whose narrow-width footer collapses `r run now` to `r run` — so the
# old `run now` marker matched the empty-list view but MISSED the edit view at
# 80x24 and af_open_tasks reported a false timeout (#1757). `r run[ now]` is the
# one run affordance common to every list/edit × wide/narrow variant.
#
# ANCHOR it on BOTH sides of the `r` key hint so it can only match the overlay's
# own menu line, never arbitrary visible pane text that happens to contain the
# substring `r run` (Greptile, PR #1769):
#   * LEFT — `(^|[^[:alnum:]])`: the `r` must start a token (line start, or a
#     non-alphanumeric like the frame padding / a `• ` separator before it), so
#     the trailing `r` of a word does NOT count ("serve`r run` •", "you`r run`").
#   * RIGHT — ` •` (U+2022): the run action is ALWAYS followed by the TASK
#     OVERLAY's space-and-bullet separator (`r run now • …` / `r run • …`), which
#     no shell output line ("no longe`r run`ning.", "you`r run` finished")
#     carries.
# The bullet is matched as a literal byte sequence, not a bracket expression, so
# it works under the sandbox's C/POSIX locale (cf. _af_tab_count).
#
# This bullet is the TASK OVERLAY's (ui/task_pane.go), NOT the root status
# menu's. Those are different rows built by different renderers, and the status
# menu moved to the repo-standard ` · ` in #2399 while the overlay hints did not
# — so do not "fix" this to a middle dot to match the status bar. If the overlay
# hints are converted later, this anchor moves with them, and the tests in
# ui/menu_test.go are what pin the status menu's own separator.
: "${_AF_TASKS_RUN_HINT:=(^|[^[:alnum:]])r run( now)? •}"

# af_open_tasks — open the task-manager overlay (`m`). Syncs on the overlay's
# `r run` run-action hint, present whether it opens in list or edit mode.
af_open_tasks() {
    af_ensure_nav
    af_send m
    af_wait_for "$_AF_TASKS_RUN_HINT" "$AF_DRIVER_TIMEOUT" 'tasks overlay' || return 1
}

# af_close_tasks — dismiss the tasks overlay (Escape). When the overlay opened
# in edit mode the first Escape drops back to the list (still showing the run
# hint), so a second Escape closes it; both cases sync on the run hint going
# away.
af_close_tasks() {
    local deadline screen
    af_send Escape
    deadline=$(( $(_af_now) + 4 ))
    while :; do
        screen="$(af_capture)"
        if ! printf '%s\n' "$screen" | grep -qE -- "$_AF_TASKS_RUN_HINT"; then
            return 0
        fi
        [ "$(_af_now)" -ge "$deadline" ] && break
        sleep "$AF_DRIVER_POLL"
    done
    af_send Escape
    af_wait_gone "$_AF_TASKS_RUN_HINT" 8 'tasks overlay closed' || return 1
}

# The config editor's own marker. Anchored on the hint row rather than a key
# name so it cannot match arbitrary pane text (the #1757 lesson): "esc close" is
# the editor's last footer fragment, and the leading separator keeps it from
# matching a session whose output happens to contain the words.
: "${_AF_CONFIG_HINT:=(^|[^[:alnum:]])esc close}"

# af_open_config — open the global config editor overlay (","). Syncs on the
# editor's footer hint rather than sleeping.
af_open_config() {
    af_ensure_nav
    af_send ','
    af_wait_for "$_AF_CONFIG_HINT" "$AF_DRIVER_TIMEOUT" 'config editor' || return 1
}

# af_close_config — dismiss the config editor (Escape). When a value field is
# open the first Escape only abandons the edit (the editor stays up, still
# showing its hints), so a second Escape closes it — the same two-level escape
# af_close_tasks handles.
af_close_config() {
    local deadline screen
    af_send Escape
    deadline=$(( $(_af_now) + 4 ))
    while :; do
        screen="$(af_capture)"
        if ! printf '%s\n' "$screen" | grep -qE -- "$_AF_CONFIG_HINT"; then
            return 0
        fi
        [ "$(_af_now)" -ge "$deadline" ] && break
        sleep "$AF_DRIVER_POLL"
    done
    af_send Escape
    af_wait_gone "$_AF_CONFIG_HINT" 8 'config editor closed' || return 1
}

# af_add_task <name> — seed a minimal valid cron task through the overlay's
# create form, so a later af_open_tasks lands on the EDIT-mode overlay whose
# narrow footer shows `r run` (the #1757 flow). Opens the tasks overlay (list
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
