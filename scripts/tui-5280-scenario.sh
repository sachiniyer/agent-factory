#!/usr/bin/env bash
# Real-TUI regression for #5280 — `R` on a tab kind that can never display a
# custom name (the Agent tab, and Terminal/shell tabs whose label always reads
# "Terminal") must answer the key with a visible refusal notice through the
# existing status-bar mechanism instead of looking dead.
#
#   scripts/testbox.sh scenario scripts/tui-5280-scenario.sh
#
# The same script doubles as the master control: run it from a detached
# origin/master worktree (copy this file under its scripts/ first) and the
# harness builds that checkout's af, so the before/after frames are comparable.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=200 AF_DRIVER_ROWS=50
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox
af_set_config 'default_program = "claude"

[program_overrides]
claude = "bash"'

af_boot
af_new_instance rename
af_select rename
af_ensure_nav           # Ctrl-]: the host owns the keyboard (no interactive pane)

# --- 1. R on the Agent tab row — the issue's exact repro -------------------
# af_select leaves the cursor on the instance row with the agent tab active;
# one `j` parks the cursor on the Agent tab row itself (pushSelection keeps
# ActiveTab=0), so the drive matches "cursor on the Agent tab row" verbatim.
af_send j
af_send R
af_wait_for "Agent and Terminal tabs can't be renamed" "$AF_DRIVER_TIMEOUT" \
    'R on the Agent tab answers with a refusal notice on the status bar'
af_refute_screen 'Rename tab' 'no rename prompt opened over the agent tab'
_af_log '--- frame after R on Agent ---'; af_capture >&2

# --- 2. The refusal notice behaves like every transient notice -------------
# It self-expires on its ~3s clock and the retained copy stays reachable
# through E details — the #2618/#4952 lifecycle guarantee.
af_wait_gone "can't be renamed" 8 'refusal notice self-expires in ~3s'
af_send E
af_wait_for 'Last notice' "$AF_DRIVER_TIMEOUT" 'retained refusal reachable via E details'
af_send Escape

# --- 3. R on a Terminal tab — same fixed-label class, same answer ----------
af_new_tab              # t -> Terminal; selects the new tab row and opens its pane
af_focus_tree || return 1   # back to the tree; the cursor stays on the Terminal row
af_send R
af_wait_for "Agent and Terminal tabs can't be renamed" "$AF_DRIVER_TIMEOUT" \
    'R on the Terminal tab answers with a refusal notice on the status bar'
af_refute_screen 'Rename tab' 'no rename prompt opened over the terminal tab'
_af_log '--- frame after R on Terminal ---'; af_capture >&2

# --- 4. The fixed labels themselves are untouched --------------------------
af_assert_screen 'Agent' 'agent tab still renders its fixed label'
af_assert_screen 'Terminal' 'shell tab still renders its fixed label'

_af_log 'PASS: R on non-renameable tabs answers with a visible refusal notice'
