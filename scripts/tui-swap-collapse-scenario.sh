#!/usr/bin/env bash
# Real-TUI drive for the sidebar swap-collapse bug, via:
#
#   scripts/testbox.sh scenario scripts/tui-swap-collapse-scenario.sh
#
# The behavior under test: a #765 same-title kill+recreate swap clears the
# stale treeCollapsed override so the replacement starts auto-expanded (▾
# marker, tab rows visible) rather than folded (▸ marker, no tab rows). The
# unit test TestSidebarTreeSwapClearsCollapse pins the internal state
# transition; this scenario proves the user-visible collapse/expand behavior
# against a real TUI + daemon (the ▾/▸ markers and the tab row count that a
# user actually sees).
#
# Why a driver scenario and not only unit tests: the internal state
# transition is unit-tested directly, but a unit test cannot see the rendered
# markers or exercise the live background-sync swap path. The collapse/expand
# cycle (h/← fold, l/→ re-open) and the auto-expand on selection are
# user-visible render states that a regression in the structureSig or the
# pointer-keyed comparison would silently break while the unit tests stayed
# green.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# drive_collapse_expand_cycle: the h/← and l/→ tree verbs against a real TUI.
# Pre-fix behavior was correct on the cycle (collapse then expand always
# worked); this proves the fix did not regress the cycle and that the ▾/▸
# markers render correctly.
drive_collapse_expand_cycle() {
    local bin name name_re
    bin="$(_af_resolve_bin)"
    name='collapse-target'
    name_re="$(_af_regex_escape "$name")"

    export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    af_boot
    af_ensure_nav
    af_focus_tree

    af_new_instance "$name" || return 1
    af_select "$name" || return 1

    # Selected -> auto-expanded: tab rows visible, ▾ marker on the row.
    local count
    count="$(_af_tab_count)"
    if [ "$count" -lt 1 ]; then
        _af_fail "selected instance should auto-expand with tab rows; got count=$count"
        af_capture >&2
        return 1
    fi
    _af_log "pre-collapse: tab count=$count (auto-expanded)"

    local screen
    screen="$(af_capture)"
    if ! printf '%s\n' "$screen" | grep -qE "▾[[:space:]]+${name_re}"; then
        _af_fail "selected instance should show the ▾ expanded marker"
        af_capture >&2
        return 1
    fi

    # h/← collapses the selected instance's subtree in place.
    af_send h
    sleep "$AF_DRIVER_POLL"
    af_wait_gone '^[[:space:]]*(├|└)[[:space:]]+[0-9]+' "$AF_DRIVER_TIMEOUT" \
        'tab rows gone after h collapse' || return 1
    count="$(_af_tab_count)"
    if [ "$count" -ne 0 ]; then
        _af_fail "after h: tab rows should be folded; got count=$count"
        af_capture >&2
        return 1
    fi
    screen="$(af_capture)"
    if ! printf '%s\n' "$screen" | grep -qE "▸[[:space:]]+${name_re}"; then
        _af_fail "collapsed instance should show the ▸ marker"
        af_capture >&2
        return 1
    fi
    _af_log "post-collapse: tab count=0 (folded, ▸ marker)"

    # l/→ re-expands in place.
    af_send l
    sleep "$AF_DRIVER_POLL"
    count="$(_af_tab_count)"
    if [ "$count" -lt 1 ]; then
        _af_fail "after l: tab rows should re-expand; got count=$count"
        af_capture >&2
        return 1
    fi
    screen="$(af_capture)"
    if ! printf '%s\n' "$screen" | grep -qE "▾[[:space:]]+${name_re}"; then
        _af_fail "re-expanded instance should show the ▾ marker"
        af_capture >&2
        return 1
    fi
    _af_log "post-expand: tab count=$count (re-expanded, ▾ marker)"

    return 0
}

# drive_kill_recreate_swap: the #765 same-title kill+recreate swap with an
# active explicit collapse. After the swap the replacement should render
# auto-expanded (▾, tab rows visible), NOT folded.
#
# The swap: kill the session via the CLI (af sessions kill), then create a
# new one with the SAME title (af sessions create). The daemon's background
# snapshot sync calls swapInstanceFromSnapshot -> ReplaceInstanceByTitle,
# re-pointing the projection's selected instance to the replacement while the
# sidebar's treeCollapsed override (now keyed by the OLD pointer) is detected
# as stale and cleared. The unit test pins the internal transition; here the
# real daemon + sync drive the same path end-to-end.
drive_kill_recreate_swap() {
    local bin name name_re
    bin="$(_af_resolve_bin)"
    name='swap-target'
    name_re="$(_af_regex_escape "$name")"

    export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    af_boot
    af_ensure_nav
    af_focus_tree

    af_new_instance "$name" || return 1
    af_select "$name" || return 1

    # Collapse the subtree so the swap has a stale override to clear.
    af_send h
    af_wait_gone '^[[:space:]]*(├|└)[[:space:]]+[0-9]+' "$AF_DRIVER_TIMEOUT" \
        'tab rows gone after h collapse' || return 1
    local count
    count="$(_af_tab_count)"
    if [ "$count" -ne 0 ]; then
        _af_fail "pre-swap: subtree should be collapsed; got count=$count"
        af_capture >&2
        return 1
    fi
    _af_log "pre-swap: collapsed (count=0)"

    # Kill the session via the CLI — talks to the same daemon (AGENT_FACTORY_HOME
    # is the sandbox). Sessions are project-scoped, so --repo must point at the
    # mock repo the daemon registered. The session is removed; the sidebar
    # reconciles.
    if ! "$bin" sessions kill "$name" --repo "$AF_DRIVER_REPO" >/dev/null 2>&1; then
        _af_fail "af sessions kill '$name' failed"
        return 1
    fi
    # Wait for the instance row to drop (the daemon removes it from the snapshot).
    af_wait_gone "${name_re}" "$AF_DRIVER_TIMEOUT" 'killed instance row gone' || return 1
    _af_log "killed '$name'; row gone"

    # Re-create with the SAME title — the #765 same-title swap. The daemon's
    # snapshot sync fires swapInstanceFromSnapshot when the new session appears
    # while a stale same-title row still lingers, or simply re-adds it; either
    # way the sidebar renders the replacement as auto-expanded.
    if ! "$bin" sessions create "$name" --repo "$AF_DRIVER_REPO" >/dev/null 2>&1; then
        _af_fail "af sessions create '$name' (recreate) failed"
        return 1
    fi
    # Wait for the replacement row to appear and reach ready (● dot).
    af_wait_for "${name_re}.*●" "$AF_DRIVER_TIMEOUT" "replacement '${name}' ready" || return 1
    _af_log "recreated '$name'; row ready"

    # Re-select so the cursor is on the replacement (the sidebar may have clamped
    # onto a header after the kill). af_select lands on a tab row, which also
    # exercises the selectTabStop clear path.
    af_select "$name" || return 1

    # The replacement must be auto-expanded: ▾ marker and tab rows visible.
    # If the stale collapse had survived the swap, this would show ▸ and 0 rows.
    sleep "$AF_DRIVER_POLL"
    count="$(_af_tab_count)"
    if [ "$count" -lt 1 ]; then
        _af_fail "post-swap: replacement should auto-expand with tab rows; got count=$count (stale collapse survived?)"
        af_capture >&2
        return 1
    fi
    local screen
    screen="$(af_capture)"
    if ! printf '%s\n' "$screen" | grep -qE "▾[[:space:]]+${name_re}"; then
        _af_fail "post-swap: replacement should show the ▾ expanded marker (stale collapse survived?)"
        af_capture >&2
        return 1
    fi
    _af_log "post-swap: tab count=$count, ▾ marker (auto-expanded — fix holds)"
    return 0
}

PASS=0
FAIL=0

step() {
    local desc="$1"; shift
    printf '\n>>> %s\n' "$desc"
    if "$@"; then
        printf '    ok: %s\n' "$desc"
        PASS=$((PASS + 1))
    else
        printf '    FAILED: %s\n' "$desc"
        FAIL=$((FAIL + 1))
    fi
}

step "collapse/expand cycle renders ▸/▾ in the real TUI" drive_collapse_expand_cycle
step "#765 same-title kill+recreate swap clears the stale collapse" drive_kill_recreate_swap

printf '\n=== SCENARIO RESULT: %d passed, %d failed ===\n' "$PASS" "$FAIL"
af_quit >/dev/null 2>&1 || true
[ "$FAIL" -eq 0 ]
