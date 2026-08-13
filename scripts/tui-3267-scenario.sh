#!/usr/bin/env bash
# Real-TUI regression for #3267, via:
#
#   scripts/testbox.sh scenario scripts/tui-3267-scenario.sh
#
# A pane-focused numeric jump to a tab ALREADY OPEN in another pane must focus
# that pane (the #1493 open-or-focus contract), never rebind the focused pane
# onto it — rebinding rendered one terminal in two panes, with the sidebar
# still marking the original tab active, and persisted the duplicate. The
# relaunch phase seeds the exact duplicated view-state a buggy build wrote and
# asserts restore heals it to one pane.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# 200x50 is load-bearing: below MultiPaneMinWidth only one pane renders at a
# time and the duplicate is hidden by auto-layout (the issue's 80x24 note).
export AF_DRIVER_COLS=200 AF_DRIVER_ROWS=50
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'

# _pane_boxes — how many pane frames the workspace draws, counted off the top
# border row (each pane contributes one ╭; the sidebar has no border).
_pane_boxes() { af_capture | grep -m1 '╭' | grep -o '╭' | wc -l; }

# _terminal_headers — committed 'tests · › Terminal' headers on the pane header
# row. The selection hint ('— selected: …') never contains the full
# 'tests · › Terminal' prefix while the selection is the agent tab, so this
# counts panes BOUND to the Terminal tab: 1 healthy, 2 when the bug duplicates.
_terminal_headers() { _af_pane_header_row | grep -o 'tests · › Terminal' | wc -l; }

af_boot
af_new_instance tests
af_select tests
af_open_pane            # s: agent tab as a pane
af_new_tab              # t + Enter: Terminal tab, auto-opened as a second pane
af_wait_for 'tests · › Terminal' "$AF_DRIVER_TIMEOUT" 'terminal pane appears'
[ "$(_pane_boxes)" = 2 ] || { _af_fail "#3267 setup: expected 2 panes, got $(_pane_boxes)"; exit 1; }

# Land focus on the AGENT pane through the tree: putting the cursor on the
# agent TAB ROW makes the selection tab-specific, so the #1493 already-open
# gate focuses the agent pane (Tab-cycling instead would stamp the agent pane
# last-focused and the same-binding early return would eat the gate).
af_send k               # pane focus -> tree focus, cursor to the instance row
af_wait_for 'n new' "$AF_DRIVER_TIMEOUT" 'tree focus after nav key'
af_send j               # cursor onto the agent tab row
af_wait_for 'hide pane' "$AF_DRIVER_TIMEOUT" 'already-open gate focuses the agent pane'
af_assert_screen '◆ Agent \*' 'sidebar marks the agent tab active'

# The issue's steps 4-5: pane-focused 1 (own tab, a no-op that pins intent),
# then pane-focused 2 — the jump to the tab the OTHER pane already shows.
af_send 1
sleep 1
af_send 2
sleep 1.5

# The fix: the agent pane keeps its binding — exactly one pane shows Terminal.
# Pre-fix both panes rendered 'tests · › Terminal' (count 2).
if [ "$(_terminal_headers)" != 1 ]; then
    _af_fail "#3267: expected exactly 1 Terminal-bound pane after the jump, got $(_terminal_headers): [$(_af_pane_header_row)]"
    exit 1
fi
[ "$(_pane_boxes)" = 2 ] || { _af_fail "#3267: both panes must survive the jump, got $(_pane_boxes)"; exit 1; }
af_assert_screen '◆ Agent \*' 'the sidebar still marks the agent tab active (pane jumps do not move the tree)'

# Snapshot the live view-state while both panes are open — the relaunch phase
# below rewrites it into the duplicated shape a buggy build persisted.
state_file="$AGENT_FACTORY_HOME/tui-state.json"
snap="$AF_DRIVER_STATE_DIR/tui-3267-state.json"
deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
while [ "$(jq '[.repos[].open_panes[]] | length' "$state_file" 2>/dev/null || echo 0)" != 2 ]; do
    if [ "$(_af_now)" -ge "$deadline" ]; then
        _af_fail "#3267: view state never persisted both panes"; exit 1
    fi
    sleep "$AF_DRIVER_POLL"
done
cp "$state_file" "$snap"

# Focus probe: the jump must have FOCUSED the Terminal pane, not been
# swallowed. `x` hides the focused pane, so the surviving header tells us
# where focus was: the agent pane must remain.
af_hide_pane
if [ "$(_terminal_headers)" != 0 ]; then
    _af_fail "#3267: x must hide the just-focused Terminal pane; still see $(_terminal_headers) Terminal pane(s): [$(_af_pane_header_row)]"
    exit 1
fi
af_assert_screen 'tests · ◆ Agent' 'the agent pane survives the hide — the jump had focused the Terminal pane'

af_quit

# Relaunch heal: seed the exact duplicate a buggy build persisted — two
# open-pane entries bound to the same (instance, tab) — and point the
# selection at that tab so the restored screen is hint-free and unambiguous.
shell_entry="$(jq -c '[.repos[].open_panes[] | select(.tab_name != "agent")][0]' "$snap")"
if [ -z "$shell_entry" ] || [ "$shell_entry" = null ]; then
    _af_fail "#3267: no shell pane entry in snapshot"; exit 1
fi
target="$(jq -c '{instance_id, title, tab_id, tab_name}' <<<"$shell_entry")"
jq --argjson e "$shell_entry" --argjson t "$target" \
    '.repos |= with_entries(.value += {open_panes: [$e, $e], selected: $t, active_tab: $t, focus: {region: "tree"}})' \
    "$snap" > "$state_file"

bin="$(_af_resolve_bin)"
af_send_literal "cd $AF_DRIVER_REPO && $bin"
af_send Enter
af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'af first frame (relaunch)'
af_wait_for 'tests · › Terminal' "$AF_DRIVER_TIMEOUT" 'restored Terminal pane paints'
if [ "$(_pane_boxes)" != 1 ] || [ "$(_terminal_headers)" != 1 ]; then
    _af_fail "#3267: relaunch must heal the persisted duplicate to ONE Terminal pane; boxes=$(_pane_boxes) terminals=$(_terminal_headers): [$(_af_pane_header_row)]"
    exit 1
fi

af_assert_no_orphan_clients
af_quit
echo 'PASS: #3267 pane-focused jump focuses the already-open pane, and relaunch heals a persisted duplicate'
