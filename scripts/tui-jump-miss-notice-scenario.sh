#!/usr/bin/env bash
# Real-TUI regression for the jump-to-tab miss-notice lifecycle (the #3021 /
# #3067 jump-to-tab "no tab matches" / "more than one tab matches" notice).
#
#   scripts/testbox.sh scenario scripts/tui-jump-miss-notice-scenario.sh
#
# The miss notice must route through handleNotice so it (1) self-expires on its
# own ~3s clock — it does not LINGER on the bar until the next notice
# overwrites it — and (2) takes a fresh notice generation, so an OLDER notice's
# still-pending 3s hideErrMsg timer cannot erase it early. The retained copy
# survives Expire, so E details still opens it, titled "Last notice" (not
# "Last error").
#
# The unit test TestJumpTabMissNoticeLifecycle covers the same invariants by
# driving hideErrMsg through Update directly; this scenario exercises the
# real 3-second tea.Cmd timers end-to-end against a real render.
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
af_new_instance jump
af_select jump
af_open_pane            # s: agent tab as a pane
af_ensure_nav           # Ctrl-]: the host owns the keyboard (no interactive pane)

# --- 1. the miss notice reaches the bar ---
af_send g
sleep 1
af_send_literal 'zzz-nomatch'
sleep 1
af_send Enter
af_wait_for 'no tab matches' "$AF_DRIVER_TIMEOUT" 'miss notice appears on the bar'
af_assert_screen 'available tabs' 'miss notice lists the available tabs'

# --- 2. the miss self-expires on its own ~3s clock (the "lingers" symptom) ---
# Pre-fix the path bypassed handleNotice, so no hideErrMsg was ever armed and the
# text stayed on the bar until the next notice overwrote it. 8s is well past the
# 3s expiry but short enough to fail fast if the notice lingers.
af_wait_gone 'no tab matches' 8 'miss notice self-expires in ~3s (no linger)'

# --- 3. the retained copy survives Expire: E details opens it ---
# Expire() clears e.err but not e.retained, so the miss stays reachable through
# E details. handleNotice (not handleError) raised it, so the overlay is titled
# "Last notice", not "Last error" — the #2618 categorization guarantee.
af_send E
af_wait_for 'Last notice' "$AF_DRIVER_TIMEOUT" 'E details overlay titled Last notice'
af_refute_screen 'Last error' 'a deliberate decline is a notice, not a failure'
af_assert_screen 'no tab matches' 'retained miss text is reachable via E details'
af_send Escape
af_wait_gone 'Last notice' "$AF_DRIVER_TIMEOUT" 'details overlay closes'

# --- 4. an older notice's timer must not erase the miss (the "early erase" symptom) ---
# Raise miss A (generation N, arms timer A at ~3s), then ~2s later raise miss B
# (generation N+1, arms timer B at ~5s). At t≈4s — AFTER A's 3s timer fired but
# BEFORE B's ~5s timer fires — A's stale hideErrMsg{N} must be IGNORED (the gen
# token advanced to N+1), so B stays on the bar. B then retires on its own
# clock. Pre-fix the gen token never advanced, so A's timer matched and
# Expire()'d B off the bar early.
af_ensure_nav
af_send g; sleep 1; af_send_literal 'zzz-A'; sleep 1; af_send Enter
af_wait_for 'no tab matches.*zzz-A' "$AF_DRIVER_TIMEOUT" 'miss A on the bar'
sleep 2   # let A's timer approach firing; B is raised ~2s after A
af_send g; sleep 1; af_send_literal 'zzz-B'; sleep 1; af_send Enter
af_wait_for 'no tab matches.*zzz-B' "$AF_DRIVER_TIMEOUT" 'miss B on the bar (new generation)'
sleep 2   # now ~4s after A (A's 3s timer has fired), ~2s after B (B's timer pending)
af_assert_screen 'no tab matches.*zzz-B' 'older notice timer did NOT erase miss B'
af_refute_screen 'no tab matches.*zzz-A' 'miss A is gone (overwritten by B, not by A erasing B)'
af_wait_gone 'no tab matches.*zzz-B' 6 'miss B self-expires on its own clock (no early erase)'

af_assert_no_orphan_clients
af_quit
echo 'PASS: jump-to-tab miss notice self-expires, survives older timers, and stays reachable via E details'
