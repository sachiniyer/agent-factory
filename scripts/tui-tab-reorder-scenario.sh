#!/usr/bin/env bash
# Real-TUI gate for session.tab.reorder's TUI surface:
#
#   scripts/testbox.sh scenario scripts/tui-tab-reorder-scenario.sh
#
# The </> pair moves the focused tab through the daemon's ReorderTab RPC — the
# same path the web's drag reorder and `af sessions tab-reorder` take. This
# drives the real TUI end to end: two CLI-created process tabs give the sidebar
# distinguishable labels, the tree's row order must match the daemon's own
# answer (`af sessions get`) after every move, a pane must follow its tab to
# the new slot, and the agent tab stays pinned at slot 1 in both directions.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=160 AF_DRIVER_ROWS=40
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'

# _tab_names — the selected instance's sidebar tab labels, one per line, in
# order. Same anchor as _af_tab_count: connector at the sidebar's left column
# (whitespace-only prefix), then the 1-based index, then the label; the
# `(├|└)` alternation survives the sandbox's C/POSIX locale (see _af_tab_count).
# The sed chain cuts the sidebar's right border (the line continues into the
# workspace on screen), then the " · open" pane marker, then the kind glyph —
# › on shell/process rows; the agent row's label is a bare "Agent".
_tab_names() {
    af_capture |
        grep -oE '^[[:space:]]*(├|└)[[:space:]]+[0-9]+[[:space:]]+[^[:space:]].*' |
        sed -E 's/^[[:space:]]*(├|└)[[:space:]]+[0-9]+[[:space:]]+//; s/│.*//; s/[[:space:]]+$//; s/ · open$//; s/^(›|◆) //'
}

# _expect_order <csv> — poll until the sidebar's tab labels are exactly this
# order (the snapshot repaint is a beat behind the keypress).
_expect_order() {
    local want="$1" got="" deadline
    deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while true; do
        got="$(_tab_names | paste -sd, -)"
        if [ "$got" = "$want" ]; then
            _af_log "order OK: $got"
            return 0
        fi
        [ "$(_af_now)" -ge "$deadline" ] && break
        sleep "$AF_DRIVER_POLL"
    done
    _af_fail "tab order: want [$want], got [$got]"
    af_capture >&2
    return 1
}

# _expect_active <regex> — poll until the WORKSPACE header confirms the tab a
# jump landed on: process tabs paint a "tests · › <name> · preview" preview,
# the agent row shows its already-open pane as "tests · Agent". A digit key is
# queued to the tty with no round-trip; sending </> before the jump lands moves
# the WRONG tab — a real daemon reorder — which is exactly the flake this
# guards.
_expect_active() {
    af_wait_for "$1" "$AF_DRIVER_TIMEOUT" "landed on $1"
}

# _daemon_order — the roster the daemon persists, via `af sessions get` (the
# read the CLI's tab-reorder prints). The cross-surface check: the TUI's
# projection must not disagree with the single writer.
_daemon_order() {
    (cd "$AF_DRIVER_REPO" && af sessions get tests | jq -r '.tabs[].name') | paste -sd, -
}

# _expect_daemon <csv> — the daemon's roster order must equal the sidebar's.
_expect_daemon() {
    local want="$1" got
    got="$(_daemon_order)"
    if [ "$got" != "$want" ]; then
        _af_fail "daemon roster: want [$want], got [$got]"
        return 1
    fi
    _af_log "daemon order OK: $got"
}

af_boot
af_new_instance tests
af_select tests

# Two NAMED process tabs through the CLI — the TUI's own `t` picker only makes
# terminal tabs, which all render "Terminal" and leave a reorder invisible on
# screen. Names give each row a label the order assertions can read.
(cd "$AF_DRIVER_REPO" && af sessions tab-create tests --command 'sleep 3600' --name alpha >/dev/null)
(cd "$AF_DRIVER_REPO" && af sessions tab-create tests --command 'sleep 3600' --name bravo >/dev/null)
af_wait_for 'bravo' "$AF_DRIVER_TIMEOUT" 'second tab in the sidebar'
_expect_order 'Agent,alpha,bravo'
_expect_daemon 'agent,alpha,bravo'

# Once a move exists (agent pinned at slot 1 + two permutable tabs), the footer
# advertises the pair.
af_wait_for '</> move tab' "$AF_DRIVER_TIMEOUT" 'move-tab hint once a move exists'

# `>` on the tree's active tab: select alpha (slot 2) and move it right. Each
# jump waits for the workspace header to name the tab it landed on before the
# move key goes out — a queued digit that has not been applied yet would send
# the move to the wrong tab.
af_send 2
_expect_active 'alpha · preview'
af_send '>'
_expect_order 'Agent,bravo,alpha'
_expect_daemon 'agent,bravo,alpha'

# `<` moves it back; both directions land in the daemon's roster.
af_send 3          # alpha is now slot 3
_expect_active 'alpha · preview'
af_send '<'
_expect_order 'Agent,alpha,bravo'
_expect_daemon 'agent,alpha,bravo'

# The agent tab is pinned: `>` on slot 1 notices and changes nothing. Notices
# are transient (a ~3s bar), so poll rather than single-shot assert. The agent
# row has an open pane, not a preview — the header reads "tests · Agent".
af_send 1
_expect_active '· Agent'
af_send '>'
af_wait_for 'pinned to the first slot' "$AF_DRIVER_TIMEOUT" 'agent-tab pin notice'
_expect_order 'Agent,alpha,bravo'

# And nothing moves left past it either: `<` on the first movable tab.
af_send 2
_expect_active 'alpha · preview'
af_send '<'
af_wait_for 'pinned to the first slot' "$AF_DRIVER_TIMEOUT" 'left-boundary pin notice'
_expect_order 'Agent,alpha,bravo'

# `>` on the LAST tab is a notice, not a silent swallow.
af_send 3
_expect_active 'bravo · preview'
af_send '>'
af_wait_for 'already at the last position' "$AF_DRIVER_TIMEOUT" 'last-position notice'
_expect_order 'Agent,alpha,bravo'

# A pane follows its tab across the move: open bravo (slot 3) as a pane, focus
# it, and `<` moves the PANE's tab — the header keeps reading bravo while the
# sidebar shows the permutation.
af_send 3
_expect_active 'bravo · preview'
af_open_pane
af_wait_for 'tests · › bravo' "$AF_DRIVER_TIMEOUT" 'bravo pane'
af_send '<'   # pane-focused move: bravo 3 -> 2
_expect_order 'Agent,bravo,alpha'
af_wait_for 'tests · › bravo' "$AF_DRIVER_TIMEOUT" 'the pane still shows its own tab'
_expect_daemon 'agent,bravo,alpha'

_af_log 'session.tab.reorder TUI scenario PASSED'
