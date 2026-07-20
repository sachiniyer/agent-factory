#!/usr/bin/env bash
# tui-driver-selftest.sh — the acceptance proof for the #1161 TUI driver.
#
# Runs the exact scenario that failed in #1156 — and it is now a handful of
# self-synchronizing lines instead of a hand-rolled tmux harness:
#
#   boot → create TWO instances → select each (assert selection) →
#   open a pane → enter interactive → type into the pane → exit →
#   attach full-screen → detach → assert selection preserved →
#   assert no orphan attach clients.
#
# It is BOTH the acceptance proof and the bitrot guard. Run it in the
# container sandbox:
#
#   make tui-driver-selftest
#   # or directly against a running sandbox (name is unique per run, #1171):
#   docker exec "$AF_PLAYTEST_NAME" bash /src/scripts/tui-driver-selftest.sh
#
# Exit status: 0 = every step green; non-zero = the first failed step (with
# the offending screen dumped to stderr by the driver).

set -uo pipefail

# Resolve the driver next to this script (works from /src in the container).
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/tui-driver.sh
source "$SELF_DIR/tui-driver.sh"

PASS=0
FAIL=0

# step <description> <command...> — run a driver step, tally, and abort the
# whole run on the first failure (a broken premise makes later steps noise).
step() {
    local desc="$1"; shift
    printf '\n>>> %s\n' "$desc"
    if "$@"; then
        printf '    ✓ %s\n' "$desc"
        PASS=$((PASS + 1))
    else
        printf '    ✗ FAILED: %s\n' "$desc"
        FAIL=$((FAIL + 1))
        printf '\n=== SELF-TEST FAILED at: %s ===\n' "$desc"
        printf 'passed %d step(s) before the failure.\n' "$PASS"
        exit 1
    fi
}

# _expect_resize_rejected — the NEGATIVE check for af_resize (Greptile, #1201):
# a resize tmux cannot honor must FAIL LOUDLY, never masquerade as success (or a
# tiny-size gate would keep running at the wrong size). We point af_resize at a
# session that does not exist; it must return non-zero. AF_DRIVER_SESSION is
# saved/restored so the rest of the run is unaffected; af_resize leaves
# AF_DRIVER_COLS/ROWS untouched because it returns before recording them on a
# failed resize.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_resize_rejected() {
    local saved="$AF_DRIVER_SESSION" rc=0
    AF_DRIVER_SESSION="no-such-driver-session-selftest"
    af_resize 60 15 >/dev/null 2>&1 || rc=$?
    AF_DRIVER_SESSION="$saved"
    if [ "$rc" -eq 0 ]; then
        _af_log "af_resize wrongly SUCCEEDED on a missing session (should fail loudly)"
        return 1
    fi
    return 0
}

# _expect_wrapped_send_no_timeout — regression proof for #1287. At a narrow
# width the echoed command wraps inside the framed pane; af_send_to_pane must
# still confirm it without burning the old 8s false timeout.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_wrapped_send_no_timeout() {
    local saved_cols="$AF_DRIVER_COLS" saved_rows="$AF_DRIVER_ROWS"
    local started elapsed rc=0
    # shellcheck disable=SC2016
    local long_cmd='printf "%s\n" "wrap-check-abcdefghijklmnopqrstuvwxyz-0123456789-abcdefghijklmnopqrstuvwxyz-0123456789"; echo WRAP_SEND_DONE_$((1200+87))'

    af_resize 60 18 || return 1
    af_wait_for 'nav mode' 10 'interactive mode after narrow resize' || rc=$?
    if [ "$rc" -eq 0 ]; then
        started="$(_af_now)"
        af_send_to_pane "$long_cmd" || rc=$?
        elapsed=$(( $(_af_now) - started ))
        if [ "$elapsed" -ge 7 ]; then
            _af_log "wrapped command echo confirmation took ${elapsed}s; likely hit the old false timeout"
            rc=1
        fi
        af_wait_for 'WRAP_SEND_DONE_1287([^0-9]|$)' 10 'wrapped command output' || rc=$?
    fi

    af_resize "$saved_cols" "$saved_rows" || return 1
    return "$rc"
}

# _expect_wrapped_marker_recognized — regression proof for #1994. When
# af_send_to_pane's short delivery marker wraps INSIDE the framed pane, the
# wrap-tolerant matcher must still recognize it instead of burning the full 8s
# false timeout. Driven against a stubbed capture (no live TUI) that reproduces
# the exact failing shape: the marker AF32561AE1 is split across two rows AND the
# continuation row's sidebar cell carries a tree-guide │ — the extra border that
# defeated the old one-pair frame strip, compounded by that strip's byte-wise
# bracket matching in the container's C locale. A match on the first capture
# returns immediately; a regression would loop to the 3s timeout and fail here.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_wrapped_marker_recognized() {
    (
        sleep() { :; }
        af_capture() {
            printf ' \xe2\x94\x9c 1 Agent     \xe2\x94\x82 dev@host:~/sandbox/mock-repo$ : "AF32561 \xe2\x94\x82\n'
            printf ' \xe2\x94\x82 \xe2\x94\x94 2 Terminal \xe2\x94\x82 AE1"; sed -n 1,120p todo.sh        \xe2\x94\x82\n'
        }
        _af_wait_for_pane_echo 'AF32561AE1' 3 'wrapped delivery marker (#1994)'
    )
}

# _expect_interactive_wrapped_marker_recognized — regression proof for #2145,
# the INTERACTIVE-mode variant of the case above. af_send_to_pane by definition
# types into the pane that owns the keyboard, and that pane is framed with the
# DOUBLE border ║ (ui/tabbed_window.go interactiveWindowStyle), not the rounded
# │. A │-only reconstruction therefore saw no pane column at all on the one
# screen it was always run against: every successful command paid the full 8s
# false timeout (reproduced on four consecutive commands at 80x24).
#
# The stub is the shape captured live from that repro — marker AF553325DF split
# across two rows by the pane's right ║ border, the continuation row resuming
# flush against the left ║ — plus a sidebar tree-guide │ on the continuation
# row, so the row carries BOTH glyphs and pins that $(NF - 1) still lands on the
# pane cell in a mixed row rather than on a sidebar cell.
#
# This case and the #1994 one above are a PAIR, and neither alone is the lock:
# splitting on only │ fails this one, splitting on only ║ fails that one. Both
# green means the reconstruction is border-glyph agnostic, which is the actual
# invariant — the driver does not know, and must not need to know, which frame
# state the pane it is matching happens to be in.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_interactive_wrapped_marker_recognized() {
    (
        sleep() { :; }
        af_capture() {
            printf '  \xe2\x96\xbe  alpha        \xe2\x97\x8f  \xe2\x95\x91dev@host:~/sandbox/mock-repo-alpha$ : "AF553325D\xe2\x95\x91\n'
            printf ' \xe2\x94\x82   \xe2\x94\x94 2 Terminal    \xe2\x95\x91F"; echo HELLO_2145                       \xe2\x95\x91\n'
        }
        _af_wait_for_pane_echo 'AF553325DF' 3 'interactive-mode wrapped delivery marker (#2145)'
    )
}

# _enter_interactive_and_probe_literal_send — regression proof for #1504. The
# literal sender used to pass the text as tmux command arguments, so leading
# hyphens were parsed as flags and repeated semicolons were parsed as command
# separators. Type but do not run the probe, then clear the shell line.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_enter_interactive_and_probe_literal_send() {
    local text='- item ;; literal-send-1504'
    af_enter_interactive || return 1
    af_send_literal "$text" || return 1
    _af_wait_for_pane_echo "$text" 8 "literal send preserves leading hyphen and ;;" || return 1
    af_send C-u
    sleep "$AF_DRIVER_POLL"
}

# _expect_multipane_hide — regression proof for #1822. Hiding a pane while
# ANOTHER pane is still open advances focus to the pane that takes its slot, NOT
# back to the tree; af_hide_pane waited for the tree menu (`n new`) and so
# false-timed-out on the first `x` of every multi-pane flow even though the pane
# was hidden correctly.
#
# The multi-pane state needs no wide terminal: below MultiPaneMinWidth (110
# cols) only ONE pane is visible at a time, so at the driver's default 100x30
# the second pane is merely auto-hidden — still open, and it takes focus on the
# next `x`. Two af_open_pane calls reach that state deterministically (the `S`
# split verb in the #1822 report is just one of several ways in; it additionally
# depends on a live preview transaction, so it is the wrong lever for a
# regression gate).
#
# Both flows are proven here: hide #1 is the multi-pane case (a pane remains),
# hide #2 is the single-pane case (the workspace empties and the tree takes
# focus back).
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_multipane_hide() {
    af_select alpha || return 1
    af_open_pane || return 1
    af_select beta || return 1
    af_open_pane || return 1

    # #1822 proper: a pane remains, so focus advances to it.
    af_hide_pane || return 1
    # Assert the PREMISE actually held — a pane is still focused. If the tree
    # menu is up here, only one pane was open and this step would be silently
    # re-testing the single-pane path that already worked.
    if af_capture | grep -qE 'n new'; then
        _af_log "#1822: tree menu is up after hiding one of TWO panes — premise broken"
        return 1
    fi

    # Single-pane case: the last pane leaves, focus returns to the tree.
    af_hide_pane || return 1
    af_assert_screen 'n new' 'focus back on the tree after the last pane is hidden' || return 1
}

# _expect_hide_pane_rejects_tree_focus — the NEGATIVE check for af_hide_pane
# (cf. _expect_resize_rejected). With focus on the TREE, `x` is a no-op — and
# the old tree-menu marker was ALREADY satisfied, so af_hide_pane returned 0 and
# reported success for a hide that never happened. It must now fail. A short
# AF_DRIVER_TIMEOUT keeps the negative probe from burning the full 25s.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_hide_pane_rejects_tree_focus() {
    local saved="$AF_DRIVER_TIMEOUT" rc=0
    af_ensure_nav
    af_focus_tree || return 1
    AF_DRIVER_TIMEOUT=3
    af_hide_pane >/dev/null 2>&1 || rc=$?
    AF_DRIVER_TIMEOUT="$saved"
    if [ "$rc" -eq 0 ]; then
        _af_log "af_hide_pane wrongly SUCCEEDED with focus on the tree (no pane to hide)"
        return 1
    fi
    return 0
}

# _expect_pane_text_not_counted_as_tabs — regression proof for the #1561
# review finding. _af_tab_count must count sidebar tab rows (including
# custom-named ones) but MUST NOT count tree-like text a shell prints inside a
# workspace pane — otherwise af_close_tab regains a false timeout from the
# opposite direction. Drive this off a synthetic capture (stub af_capture in a
# subshell) so it is deterministic and does not depend on a live pane happening
# to print the right thing: two real sidebar tab rows on the left (one built-in
# `Preview`, one CUSTOM `cli-check`), and a bordered pane on the right whose
# `tree` output prints `├ 1 foo` / `└ 2 bar`. Exactly the two sidebar rows must
# count.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_pane_text_not_counted_as_tabs() {
    local screen count
    # Two dangerous pane rows are represented: one where the pane's `├ 1 foo`
    # shares a line with a sidebar tab row, and one where the sidebar column is
    # BLANK so only the pane's `│` border precedes the pane's `└ 2 bar`.
    screen="$(cat <<'SCREEN'
 ▾ beta                        │ ╭─ 2 cli-check ──────────────╮
   ├ 1 Preview                 │ │ $ tree                     │
   └ 2 cli-check               │ │ ├ 1 foo                    │
 ▸ alpha                       │ │   subdir                   │
                               │ │ └ 2 bar                    │
                               │ ╰────────────────────────────╯
SCREEN
)"
    # Override af_capture only inside this command substitution's subshell.
    count="$(af_capture() { printf '%s\n' "$screen"; }; _af_tab_count)"
    if [ "$count" != "2" ]; then
        _af_log "pane text inflated the tab count: got '$count', want 2 (the two sidebar rows)"
        _af_log "----- synthetic screen -----"
        printf '%s\n' "$screen" >&2
        _af_log "----------------------------"
        return 1
    fi
    return 0
}

# _expect_af_select_boundary — regression proof for #1759. af_select must
# evaluate its ready condition AFTER the final downward `j`, not only before it.
# Drive af_select against stubbed send/capture (no live TUI, no real sleeps) in
# a subshell so nothing leaks: a counter tracks `j` presses, and the row is
# rendered actionable (▾ + `D kill`) ONLY once the count reaches 40 — exactly
# the loop boundary. The old loop pressed the 40th `j` and exited without a
# trailing capture, so it never saw the actionable frame and returned non-zero
# here; the fixed loop checks the post-`j` state and returns 0.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_af_select_boundary() {
    (
        _AF_BOUNDARY_JCOUNT=0
        # Stubs local to this subshell; the sourced af_select resolves them
        # dynamically when it calls them.
        af_ensure_nav() { :; }
        af_focus_tree() { return 0; }
        sleep() { :; }
        af_send() { [ "${1:-}" = j ] && _AF_BOUNDARY_JCOUNT=$((_AF_BOUNDARY_JCOUNT + 1)); return 0; }
        af_capture() {
            if [ "$_AF_BOUNDARY_JCOUNT" -ge 40 ]; then
                printf '%s\n' ' ▾ target                       │ menu: D kill'
            else
                printf '%s\n' ' ▸ target                       │ menu: n new'
            fi
        }
        af_select target
    )
}

# _expect_af_select_open_pane — regression proof for #1996. Scanning onto an
# instance that already has an open workspace pane auto-focuses that pane, which
# replaces the tree footer (removing 'D kill') and stops the scan keys from
# driving the tree — so af_select used to exhaust its scan and report a false
# selection failure even though the instance WAS selected. Stubbed (no live TUI):
# a `j` counter flips the target to display-selected-with-pane-menu at the loop
# midpoint, and af_focus_tree restores the tree footer. The old af_select never
# saw 'D kill' and failed; the fixed one detects the pane menu, re-focuses the
# tree, and finalizes.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_af_select_open_pane() {
    (
        _AF_OPENPANE_JCOUNT=0
        _AF_OPENPANE_FOCUS=tree
        af_ensure_nav() { :; }
        # Restoring tree focus clears the pane menu (as the real af_focus_tree does).
        af_focus_tree() { _AF_OPENPANE_FOCUS=tree; return 0; }
        sleep() { :; }
        af_send() {
            if [ "${1:-}" = j ]; then
                _AF_OPENPANE_JCOUNT=$((_AF_OPENPANE_JCOUNT + 1))
                [ "$_AF_OPENPANE_JCOUNT" -ge 5 ] && _AF_OPENPANE_FOCUS=pane
            fi
            return 0
        }
        af_capture() {
            if [ "$_AF_OPENPANE_JCOUNT" -lt 5 ]; then
                printf '%s\n' ' ▸ target                       │ menu: n new'
            elif [ "$_AF_OPENPANE_FOCUS" = pane ]; then
                printf '%s\n' ' ▾ target                       │ ← prev pane • → next pane │ s open pane • x hide pane'
            else
                printf '%s\n' ' ▾ target                       │ menu: n new • D kill'
            fi
        }
        af_select target
    )
}

# _expect_attach_send_no_truncation — regression proof for #1995. In a
# full-screen attach, a bare `af_send_literal … ; af_send Enter` can let the
# Enter overtake a still-draining paste and execute a TRUNCATED command;
# af_send_line must wait for the whole line to echo before submitting. Stubbed
# (no live TUI): a file-backed counter reveals the pasted line ~8 chars per
# capture (drain latency), and Enter "executes" whatever is visible at that
# instant. First it proves the naive path truncates (so this stays a real
# regression proof), then proves af_send_line delivers the whole command.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_attach_send_no_truncation() {
    (
        local state target submitted
        state="$(mktemp "${TMPDIR:-/tmp}/af-1995.XXXXXX")" || return 1
        target='git status --short && echo ATTACH_OK'
        sleep() { :; }
        _vis_for() {
            local n="${1:-0}" reveal
            reveal=$(( n * 8 ))
            if [ "$reveal" -ge "${#target}" ]; then printf '%s' "$target"
            else printf '%s' "${target:0:$reveal}"; fi
        }
        af_capture() {
            local n; n="$(cat "$state" 2>/dev/null || echo 0)"; n=$(( n + 1 ))
            printf '%s' "$n" > "$state"
            printf 'dev@host:~/sandbox/mock-repo$ %s\n' "$(_vis_for "$n")"
        }
        af_send_literal() { printf '0' > "$state"; return 0; }
        # ctrl-u wipes the input line, exactly as readline does — af_send_line
        # clears before each paste (#2147), and a stub that ignored the clear
        # would leave the previous attempt's text on screen and confirm it.
        af_send() {
            case "${1:-}" in
                Enter) submitted="$(_vis_for "$(cat "$state" 2>/dev/null || echo 0)")" ;;
                C-u)   printf '0' > "$state" ;;
            esac
            return 0
        }

        # 1. The naive sequence must truncate here, or the stub isn't reproducing
        #    the race and the pass below would be meaningless.
        printf '0' > "$state"; submitted='<none>'
        af_send_literal "$target"; af_send Enter
        if [ "$submitted" = "$target" ]; then
            rm -f "$state"
            _af_fail "#1995 stub did not reproduce truncation (naive path submitted the full line)"
            return 1
        fi

        # 2. af_send_line waits for the whole line before Enter, so it submits it intact.
        printf '0' > "$state"; submitted='<none>'
        af_send_line "$target" 3
        rm -f "$state"
        if [ "$submitted" = "$target" ]; then
            return 0
        fi
        _af_fail "#1995: af_send_line submitted a TRUNCATED command: [$submitted]"
        return 1
    )
}

# _expect_short_command_delivery_waits — regression proof for the SHORT-command
# hole in the #1995 fix, now expressed against the whole-line matcher (#2147).
#
# The original hole was in _af_delivery_tail: a bare `${ws: -24}` evaluates to
# the EMPTY string whenever the command is shorter than 24 characters (bash does
# not clamp to the whole string), and an empty tail satisfied the matcher's
# `[ -z "$tail" ]` short-circuit — so af_send_line returned WITHOUT waiting for
# most real commands ('git status', 'ls -la', 'make'). Matching the WHOLE text
# removes the tail, and with it the short-command special case: there is no
# length below which the matcher stops looking at anything. This pins the
# BEHAVIOUR that hole broke, so it stays locked however the matcher is written.
#
# Both polarities, because either alone is satisfiable by a broken matcher: one
# that always fails passes the negative, one that always succeeds passes the
# positive.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_short_command_delivery_waits() {
    (
        local short='git status' rc=0

        # 1. NEGATIVE — a screen that never echoes the command must never
        #    confirm. Reporting success here is the bug: delivery "confirmed"
        #    for a paste that never arrived.
        af_capture() { printf '%s\n' 'dev@host:~/sandbox/mock-repo$ '; }
        _af_wait_for_line_echo "$short" 1 >/dev/null 2>&1 || rc=$?
        if [ "$rc" -eq 0 ]; then
            _af_fail "#1995: _af_wait_for_line_echo reported success for a short command that never echoed"
            return 1
        fi

        # 2. POSITIVE — once the short command IS echoed it must confirm, or the
        #    negative above would also pass on a matcher that never matches.
        af_capture() { printf '%s\n' 'dev@host:~/sandbox/mock-repo$ git status'; }
        if ! _af_wait_for_line_echo "$short" 1 >/dev/null 2>&1; then
            _af_fail "#1995: _af_wait_for_line_echo failed to confirm a short command that IS fully echoed"
            return 1
        fi
        return 0
    )
}

# _expect_partial_middle_not_confirmed — the assumption #2147 broke. The old
# matcher checked a 24-character TAIL of the line, on the theory that the only
# way a paste can be incomplete is a racing Enter chopping its END. The
# full-screen attach path falsifies that: whole INTERIOR chunks go missing while
# the tail lands (reproduced live — a 128-char line arrived as chunks 1, 2 and 4,
# with chunk 3 dropped), and a tail-only check happily confirms that mangled
# line, so af_send_line submits a command the user never wrote.
#
# The stub is that live shape: the screen holds the line's first half and its
# last quarter, with a 32-byte middle chunk missing. The tail is fully present,
# so a tail matcher returns 0 here; the whole-line matcher must not.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_partial_middle_not_confirmed() {
    (
        local target mangled tail_ws mangled_ws rc=0
        target='sed -i "/first item/a ./todo.sh done 1" test.sh && echo tests-edited-2147'
        # Drop 20 characters out of the MIDDLE, keep the rest — including the
        # whole tail window — verbatim.
        mangled="${target:0:20}${target:40}"
        af_capture() { printf 'dev@host:~/sandbox/mock-repo$ %s\n' "$mangled"; }

        # Premise: the old 24-character tail check would still PASS on this
        # screen. Without that, the case proves nothing about tail matching —
        # it would just be another "incomplete line" the old code also caught.
        tail_ws="$(printf '%s' "$target" | tr -d '[:space:]')"; tail_ws="${tail_ws: -24}"
        mangled_ws="$(printf '%s' "$mangled" | tr -d '[:space:]')"
        if ! printf '%s' "$mangled_ws" | grep -Fq -- "$tail_ws"; then
            _af_fail "#2147 stub is not reproducing the shape: the mangled screen lost the TAIL too, so a tail matcher would have caught it"
            return 1
        fi

        _af_wait_for_line_echo "$target" 1 >/dev/null 2>&1 || rc=$?
        if [ "$rc" -eq 0 ]; then
            _af_fail "#2147: delivery was CONFIRMED for a line with a dropped middle chunk — af_send_line would submit a mangled command"
            return 1
        fi
        return 0
    )
}

# _expect_send_line_refuses_partial — the #2147 headline: af_send_line must not
# press Enter on a line it could not confirm.
#
# The old helper logged "submitting best-effort" and submitted anyway. That is
# strictly worse than failing: the live repro pasted a ~120-character QUOTED
# command, only its first 32 bytes landed, and submitting that prefix left bash
# in its `>` continuation prompt — after which every later scripted command
# became a continuation line and the whole scenario was driving a corrupted
# shell while reporting success.
#
# The stub makes the live drop permanent: the screen only ever shows the first 32
# bytes, on every attempt. Three things are asserted, and the first is the one
# that matters — no Enter is EVER sent. A helper that returns non-zero but has
# already submitted has still wrecked the session.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_send_line_refuses_partial() {
    (
        local target enters=0 cleared=0 rc=0
        target='sed -i '"'"'/\.\/todo.sh | grep -q "first item"/a ./todo.sh done 1'"'"' test.sh && echo tests-edited'
        # The real clock and the real poll sleep are used deliberately. _af_now is
        # called as `$(_af_now)`, i.e. in a SUBSHELL, so a counter-in-a-variable
        # clock stub can never advance and the wait loop spins forever; and
        # stubbing sleep against a real clock busy-forks for the whole timeout.
        # A 1s budget over 2 attempts keeps this at ~2s.
        af_capture() { printf 'dev@host:~/sandbox/mock-repo$ %s\n' "${target:0:32}"; }
        af_send_literal() { return 0; }
        af_send() {
            case "${1:-}" in
                Enter) enters=$(( enters + 1 )) ;;
                C-u)   cleared=$(( cleared + 1 )) ;;
            esac
            return 0
        }

        AF_DRIVER_SEND_LINE_ATTEMPTS=2
        af_send_line "$target" 1 >/dev/null 2>&1 || rc=$?

        if [ "$enters" -ne 0 ]; then
            _af_fail "#2147: af_send_line SUBMITTED an unconfirmed partial line (${enters} Enter(s)) — that is what corrupts the attached shell"
            return 1
        fi
        if [ "$rc" -eq 0 ]; then
            _af_fail "#2147: af_send_line reported SUCCESS for a line that only ever landed as a 32-byte prefix"
            return 1
        fi
        if [ "$cleared" -eq 0 ]; then
            _af_fail "#2147: af_send_line gave up without clearing the partial line, leaving an unbalanced fragment on the prompt"
            return 1
        fi
        return 0
    )
}

# --- #1884/#1885 multi-tab pane cycling ---
# The tab-cycling play-test that found #1884/#1885/#1886: on one instance with
# several tabs, a pane-focused number key (1-9) jumps THAT pane, and the pane
# layer keys on the FOCUSED PANE + the STABLE TAB ID rather than the tree's
# active tab. These helpers drive that flow end to end with per-tab content
# markers so the rendered tab is unambiguous (shell tabs all render "Terminal").

# _cycle_plant <tabnum> <marker> — pane-focused jump to tab <tabnum> (#1885),
# then type a unique marker into that tab's shell so later jumps can tell the
# tabs apart by content. Precondition: a pane is focused.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_cycle_plant() {
    local tabnum="$1" marker="$2"
    af_ensure_nav
    af_send "$tabnum"                     # pane-focused jump to tab N
    af_enter_interactive || return 1
    af_send_to_pane "echo $marker"
    af_wait_for "$marker" 10 "tab $tabnum planted $marker" || return 1
    af_exit_interactive || return 1
}

# _expect_cycle_jumps_land — plant markers in tabs 2 and 3, then prove a
# pane-focused number jump LANDS on the addressed tab and STAYS there: the pane
# renders that tab's own marker, never the other tab's (the #1885 repaint
# symptom — "press 4, see tab 2"). Precondition: cycle selected, a pane focused,
# tabs = agent + shell + shell-2.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_cycle_jumps_land() {
    _cycle_plant 2 AF_CYCLE_T2 || return 1
    _cycle_plant 3 AF_CYCLE_T3 || return 1

    af_ensure_nav
    af_send 2
    af_wait_for 'AF_CYCLE_T2' 10 'pane jump to tab 2 shows the shell marker' || return 1
    af_refute_screen 'AF_CYCLE_T3' 'tab-2 jump must NOT render shell-2 (no repaint #1885)' || return 1

    af_send 3
    af_wait_for 'AF_CYCLE_T3' 10 'pane jump to tab 3 shows the shell-2 marker' || return 1
    af_refute_screen 'AF_CYCLE_T2' 'tab-3 jump must NOT render shell (no repaint #1885)' || return 1
}

# _expect_cycle_w_closes_viewed — the #1884 destructive divergence: the tree's
# active tab is shell-2 (last created), but the FOCUSED pane is jumped to shell.
# w must close the tab the user is VIEWING (shell), leaving shell-2 — not the
# tree's active tab. Verified by content: after w, tab 2 is now shell-2 (its
# marker), and shell's marker is gone. The old bug closed shell-2 and left shell
# here, which the refute would catch.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_cycle_w_closes_viewed() {
    # Pane-focused jump to shell (tab 2); the tree's active tab stays shell-2.
    af_ensure_nav
    af_send 2
    af_wait_for 'AF_CYCLE_T2' 10 'pane viewing shell (tab 2) before close' || return 1

    local before; before="$(_af_tab_count)"
    af_send w                             # closes the FOCUSED pane's tab (#1884)
    local deadline; deadline=$(( $(_af_now) + AF_DRIVER_TIMEOUT ))
    while [ "$(_af_tab_count)" -ge "$before" ]; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "w did not close a tab (still $before)"; return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done

    # Reopen a pane and land on tab 2: it must now be shell-2 (its marker), proof
    # that w closed the VIEWED shell tab and left shell-2 alive.
    af_ensure_nav
    af_focus_tree || return 1
    af_open_pane || return 1
    af_send 2
    af_wait_for 'AF_CYCLE_T3' 10 'surviving tab 2 is shell-2 — w closed the VIEWED tab (#1884)' || return 1
    af_refute_screen 'AF_CYCLE_T2' 'the viewed shell tab was closed, so its marker is gone' || return 1
}

# _expect_live_attach_send_line — the LIVE half of #2147, and the reason the
# stub above is not enough on its own. The #1995 case is a deterministic stub
# proof, and it stayed green while the real 80x24 attach path mis-drove every
# day: only a driver talking to a real attach can show that a ~120-character
# quoted command actually arrives whole and RUNS.
#
# Shaped to reproduce the drop rather than dodge it. The paste goes in
# immediately after af_attach, which is where the loss concentrates (the first
# paste after an attach truncated on 3 of 3 live attempts with no added settle,
# versus intermittently later), so af_send_line's clear-and-re-paste recovery is
# exercised on nearly every run rather than only when unlucky.
#
# The asserted marker is COMPUTED by the command (arithmetic expansion), so
# AF_2147_9182 can appear ONLY in the command's OUTPUT — the echoed input line
# shows the literal $((9000+182)). A shell that echoed the line but never ran it
# therefore fails here. Single quotes are REQUIRED so the arithmetic reaches the
# attached shell unexpanded.
#
# The refute is the corruption check proper: an unbalanced quote from a
# submitted prefix leaves bash in its `>` continuation prompt, which is the
# state that swallowed every subsequent command in the #2147 report.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
# shellcheck disable=SC2016
_expect_live_attach_send_line() {
    local saved_cols="$AF_DRIVER_COLS" saved_rows="$AF_DRIVER_ROWS" rc=0
    # shellcheck disable=SC2016
    local cmd='printf "%s\n" "af-2147-alpha-bravo-charlie-delta-echo" | grep -q "charlie" && echo AF_2147_$((9000+182))'

    af_resize 80 24 || return 1
    if af_select beta && af_attach; then
        af_send_line "$cmd" || rc=$?
        if [ "$rc" -eq 0 ]; then
            af_wait_for 'AF_2147_9182([^0-9]|$)' 15 'the WHOLE quoted line ran in the attached shell' || rc=$?
            af_refute_screen '^>' 'attached shell is NOT stuck in a quote-continuation prompt (#2147)' || rc=$?
        fi
        # shellcheck disable=SC2119  # af_detach's optional arg is for encoded keys
        af_detach || rc=1
    else
        rc=1
    fi

    af_resize "$saved_cols" "$saved_rows" || return 1
    return "$rc"
}

# _expect_scrolled_rail_reads_as_booted — regression proof for #2148, driven off
# stubbed captures (no live TUI) so all three polarities are deterministic.
#
# 1. POSITIVE: the exact first frame from the report — three sessions at 80x24
#    with a lower one restored, so the rail has scrolled `Instances (N)` out of
#    the window and drew `▲ 2 more` in its place. af_boot waited 35s for the
#    header, never saw it, and reported a boot failure for a TUI that had booted
#    fine with every session alive.
# 2. NEGATIVE (not booted): a bare shell prompt must still time out. Without
#    this, "fix" the marker by matching anything and the boot gate becomes
#    decorative — it would report success for an af that never started.
# 3. NEGATIVE (pane text): a workspace pane printing "▲ 3 more" must NOT satisfy
#    the gate. The indicator is anchored to the sidebar's left edge, so only
#    leading whitespace may precede it; pane text always carries the sidebar
#    columns and a pane border ahead of it.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_scrolled_rail_reads_as_booted() {
    (
        # Real clock/sleep on purpose — see _expect_send_line_refuses_partial:
        # `$(_af_now)` is a subshell, so a variable-counter clock stub never
        # advances and the negative probes would spin forever. The positive
        # matches on its first capture; the two negatives cost 1s each.
        af_capture() {
            cat <<'SCREEN'
                      ╭────────────────────────────────────────╮
 Agent Factory        │ PREVIEW gamma · ◆ Agent                │
 ▲ 2 more             │dev@host:~/sandbox/mock-repo-gamma$     │
                      │                                        │
  ▸  beta          ●  │                                        │
     ⎇-dev/beta       │                                        │
                      │                                        │
  ▾  gamma         ●  │                                        │
     ⎇-dev/gamma      │                                        │
                      │                                        │
    └ 1 ◆ Agent *     │                                        │
                      ╰────────────────────────────────────────╯
     n new • D kill │ t new tab • w close tab │ ? help • q quit
SCREEN
        }
        # Premise: the frame really is missing the header, or this is just
        # re-testing the case that always worked.
        if af_capture | grep -qE -- 'Instances \('; then
            _af_fail "#2148 stub is not reproducing the shape: the scrolled frame still shows the Instances header"
            return 1
        fi
        if ! af_wait_for "$_AF_RAIL_MARKER" 3 'scrolled rail reads as booted (#2148)' >/dev/null 2>&1; then
            _af_fail "#2148: a scrolled rail (▲ N more, header off-window) was not accepted as booted"
            return 1
        fi

        local rc=0
        af_capture() { printf '%s\n' 'dev@host:~/sandbox/mock-repo$ '; }
        af_wait_for "$_AF_RAIL_MARKER" 1 'not booted' >/dev/null 2>&1 || rc=$?
        if [ "$rc" -eq 0 ]; then
            _af_fail "#2148: the boot marker matched a bare shell prompt — the gate no longer proves af booted"
            return 1
        fi

        rc=0
        af_capture() {
            printf '%s\n' '  ▾  alpha        ●  │ $ cat rail.txt                     │'
            printf '%s\n' '     ⎇-dev/alpha     │ ▲ 3 more                           │'
        }
        af_wait_for "$_AF_RAIL_MARKER" 1 'pane text' >/dev/null 2>&1 || rc=$?
        if [ "$rc" -eq 0 ]; then
            _af_fail "#2148: pane output containing '▲ 3 more' satisfied the boot gate — the indicator is not anchored to the sidebar"
            return 1
        fi
        return 0
    )
}

# _expect_scrolled_rail_relaunch — the LIVE half of #2148. Three sessions at
# 80x24 with the lowest selected is exactly the reported state: the rail scrolls,
# the header is gone, and the boot gate has to read the frame as booted anyway.
# af_relaunch shares af_boot's gate, so this drives the real thing end to end.
#
# The premise is asserted AFTER the relaunch rather than assumed: if the rail
# happened NOT to scroll, the relaunch would pass on unfixed code too and this
# step would be quietly vacuous.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_scrolled_rail_relaunch() {
    local saved_cols="$AF_DRIVER_COLS" saved_rows="$AF_DRIVER_ROWS" rc=0

    af_resize 80 24 || return 1
    if af_select cycle; then
        af_relaunch || rc=$?
        if [ "$rc" -eq 0 ]; then
            if ! af_capture | grep -qE -- '^[[:space:]]*▲ [0-9]+ more'; then
                _af_log "#2148 premise not met: the 80x24 rail did NOT scroll, so this step proves nothing"
                rc=1
            elif af_capture | grep -qE -- 'Instances \('; then
                _af_log "#2148 premise not met: the header is still visible, so the old gate would have passed too"
                rc=1
            fi
        fi
    else
        rc=1
    fi

    af_resize "$saved_cols" "$saved_rows" || return 1
    return "$rc"
}

printf '=== tui-driver self-test (#1161) ===\n'
printf 'session=%s size=%sx%s home=%s\n' \
    "$AF_DRIVER_SESSION" "$AF_DRIVER_COLS" "$AF_DRIVER_ROWS" "$AGENT_FACTORY_HOME"

# Start from a clean slate so the run is deterministic even in a reused
# container (scoped to the sandbox; fails closed on a non-sandbox home).
step "reset sandbox to a clean state"                       af_reset_sandbox
# af_boot routes launch geometry through af_resize, which verifies the window
# actually took the requested size — so a green boot is also positive proof
# af_resize works (#1174 item 2 / #1201).
step "boot af at ${AF_DRIVER_COLS}x${AF_DRIVER_ROWS}"        af_boot
step "af_resize fails loudly on an impossible resize"       _expect_resize_rejected
# --- #2148 regression: the boot gate must survive a scrolled rail ---
# The `Instances (N)` header is the rail's first windowed ROW, not chrome, so a
# scrolled rail hides it behind `▲ N more` and a header-only gate false-times-out
# on a TUI that booted fine. Deterministic stub proof — both polarities.
step "a scrolled rail still reads as booted (#2148)"        _expect_scrolled_rail_reads_as_booted
step "create instance 'alpha'"                              af_new_instance alpha

# --- #1174 item 1 / #1199 regression: the SINGLE-instance false positive ---
# With exactly one instance the row auto-display-selects (sticky ▾) while the
# tree cursor still sits on the section header — so GetSelectedInstance() is
# nil and cursor-driven verbs (o/D) silently NO-OP even though the row LOOKS
# selected. The old af_select returned on iteration 0 (▾ present) and a
# play-test could wrongly "pass". af_select now lands the cursor on an
# actionable tab row, so `o attach` MUST actually fire here. If the bug returns,
# either af_select fails (no 'D kill') or af_attach times out waiting for the
# chrome to vanish.
step "select the SOLE instance alpha"                       af_select alpha
step "assert alpha display-selected (sole instance)"        af_expect_selected alpha
step "attach the SOLE instance (proves 'o' is NOT a no-op)" af_attach
step "detach from the single-instance attach"              af_detach
step "assert alpha survived the single-instance round trip"  af_expect_selected alpha

step "create instance 'beta'"                               af_new_instance beta

# The #1156 failure, now two lines each.
step "select alpha"                                         af_select alpha
step "assert alpha is selected"                             af_expect_selected alpha
step "select beta"                                          af_select beta
step "assert beta is selected"                              af_expect_selected beta

# --- #1759 regression: af_select must check AFTER the final scan step ---
# A row that only becomes actionable on the loop's boundary `j` used to be
# missed (false selection failure). Deterministic stub proof — no live TUI.
step "af_select evaluates the boundary step (#1759)"        _expect_af_select_boundary
step "af_select handles a target with an open pane (#1996)"  _expect_af_select_open_pane

# --- #1757 regression: the task-overlay run action ---
# `m` drops straight into the selected task's EDIT form when a task exists, and
# at 80x24 its footer collapses `r run now` to `r run`. af_open_tasks used to
# wait for the stale `run now` and time out here; it now syncs on `r run`.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
# _expect_config_editor_writes — the config editor's end-to-end flow against the
# sandbox's throwaway AF home: open it, assert it rendered a tier-1 key FROM THE
# MANIFEST, edit a value through the real write path, and assert the editor
# echoed the write AND told the user the change is not live until a restart.
#
# The restart notice is the assertion that matters most here. config.toml is read
# at startup, so an editor that changed a value the running daemon then ignored —
# without saying so — would be lying by omission. This pins that it says so at the
# moment of the edit, and names the command.
_expect_config_editor_writes() {
    af_open_config || return 1
    # The manifest's tier-1 keys lead; the editor holds no key list of its own.
    af_assert_screen 'default_program' 'config editor renders a manifest key' || return 1

    # Edit the selected (first) key: Enter opens the value field pre-filled with
    # the live value, so clear it before typing.
    af_send Enter
    local i
    for i in $(seq 1 32); do af_send BSpace; done
    af_send_literal 'codex' || return 1
    af_send Enter

    af_wait_for 'set default_program = codex' "$AF_DRIVER_TIMEOUT" 'config write echo' || return 1
    # Match a fragment that survives the overlay's line wrap. The notice reads
    # "… run `af daemon restart` and restart af to apply", and at the driver's
    # fixed 100x30 the wrap falls between "af" and "daemon restart" — so the full
    # phrase is NOT greppable on one line. Verified against a real capture rather
    # than guessed; the fixed terminal size is what makes it deterministic.
    af_wait_for 'daemon restart' "$AF_DRIVER_TIMEOUT" 'config restart notice' || return 1
    af_close_config || return 1

    # It reached the real file, through the real writer.
    grep -q "default_program = 'codex'" "$AGENT_FACTORY_HOME/config.toml" || {
        printf 'config.toml does not hold the edited value:\n'
        cat "$AGENT_FACTORY_HOME/config.toml"
        return 1
    }
}

# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
# _expect_config_editor_rejects — an invalid value is refused by the SAME
# validation the CLI uses, in the UI, with the validator's own message, and the
# field stays open to correct. Nothing reaches the file.
_expect_config_editor_rejects() {
    af_open_config || return 1
    af_send Enter
    local i
    for i in $(seq 1 32); do af_send BSpace; done
    af_send_literal 'emacs' || return 1
    af_send Enter

    # The validator's own message, not one the pane invented: "default_program
    # must be one of [claude, codex, aider, gemini, amp], got "emacs"…".
    # Waiting on 'default_program' here would be VACUOUS — the key is on screen
    # whether or not the value was refused.
    af_wait_for 'must be one of' "$AF_DRIVER_TIMEOUT" 'validator error' || return 1
    af_refute_screen 'set default_program = emacs' 'a rejected value must not echo as written' || return 1
    grep -q "default_program = 'emacs'" "$AGENT_FACTORY_HOME/config.toml" && {
        printf 'a REJECTED value reached config.toml\n'
        return 1
    }
    af_close_config || return 1
    af_close_config 2>/dev/null || true
    return 0
}

# _expect_config_agent_attaches_in_tmux — regression proof for #2019. This is the
# END-TO-END gate for the config-agent takeover, and it is only meaningful HERE:
# the driver runs af inside a real tmux pane, so pressing C hits the reporter's
# exact condition — af has $TMUX set. On unfixed code the config agent's
# `tmux attach-session` refuses to nest ("sessions should be nested with care,
# unset $TMUX to force") and the takeover collapses back to the TUI as
# "config agent: exit status 1". The fix (1) scrubs $TMUX so tmux stops refusing,
# (2) pins the server socket with -S, and (3) attaches to the RESOLVABLE session
# name (the daemon returned the bare seq af-config-<n>, which does not resolve the
# real session af_af-config-<n> — a latent "can't find session" that the nesting
# refusal masked). So the takeover LANDS: the TUI chrome is replaced by the config
# agent's own session.
#
# The sandbox default_program resolves to bash (program_overrides), so the config
# agent needs no real agent binary; the briefing arrives as a bracketed paste, so
# bash keeps running and the takeover stays up until we detach it. The config
# session lives on this same container tmux server, so detach-client by name
# returns control to the TUI without guessing a nested prefix key.
# shellcheck disable=SC2317  # dispatched indirectly via step(); not dead code.
_expect_config_agent_attaches_in_tmux() {
    af_ensure_nav
    af_focus_tree || return 1

    af_send C

    # Resolve one of two mutually exclusive outcomes:
    #   * the attach error surfaces — #2019 reproduced (hard fail), OR
    #   * the TUI chrome vanishes — the takeover landed (pass).
    # The failure strings are deterministic on unfixed code (af always runs inside
    # tmux here), so seeing any of them is the failure. The 75s budget covers the
    # spawn's readiness wait plus the briefing paste.
    local deadline screen; deadline=$(( $(_af_now) + 75 ))
    while :; do
        screen="$(af_capture)"
        if printf '%s\n' "$screen" | grep -qiE 'exit status 1|nested with care|can.t find session'; then
            _af_log "#2019: config agent failed to attach (takeover collapsed to an error):"
            printf '%s\n' "$screen" >&2
            return 1
        fi
        if ! printf '%s\n' "$screen" | grep -qE 'Agent Factory'; then
            break  # chrome gone → the takeover landed
        fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_log "#2019: config agent neither attached nor errored within 75s"
            printf '%s\n' "$screen" >&2
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done

    # We are inside the config agent's tmux session now. Detach its client so
    # control returns to the TUI (the app then reaps the session and reloads
    # config). The session is af_af-config-<n> on this same server.
    local cfg
    cfg="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep '^af_af-config-' | head -1)"
    if [ -z "$cfg" ]; then
        _af_log "#2019: takeover landed but no af_af-config-* session was found to detach"
        return 1
    fi
    tmux detach-client -s "$cfg" 2>/dev/null || true

    # Back in the TUI, and NOT via the error path.
    af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'returned to the TUI after the config-agent takeover' || return 1
    af_refute_screen 'exit status 1' 'the config-agent takeover must not have errored (#2019)' || return 1
    return 0
}

step "seed a task via the create form"                      af_add_task selftest-task
step "close the tasks overlay after create"                 af_close_tasks
step "reopen tasks — edit-mode overlay recognized (#1757)"  af_open_tasks
step "assert the task editor shows the run action"          af_assert_screen "$_AF_TASKS_RUN_HINT" 'task-overlay run action'
step "close the tasks overlay"                              af_close_tasks

# --- #2019 regression: the config agent (C) must attach even though af is nested
# inside tmux. On unfixed code the takeover collapses to "config agent: exit
# status 1" (tmux refusing to nest); the fix scrubs $TMUX, pins the socket, and
# attaches to the resolvable session name.
#
# This runs BEFORE the config-editor steps deliberately: those rewrite
# default_program to codex (not installed in the test image), which would make the
# config agent fail preflight rather than exercise the attach. Here the sandbox's
# default_program still resolves to bash (config.json program_overrides), so the
# config agent runs bash and needs no real agent binary.
step "config agent (C) attaches while af runs inside tmux (#2019)"  _expect_config_agent_attaches_in_tmux

step "open the config editor (,) and write through the real path"  _expect_config_editor_writes
step "config editor refuses an invalid value with the CLI error"   _expect_config_editor_rejects

step "open beta's tab as a pane"                            af_open_pane
step "enter interactive mode and probe literal send"         _enter_interactive_and_probe_literal_send
step "send a wrapped command without echo false-timeout"     _expect_wrapped_send_no_timeout
step "recognize a delivery marker split by a pane wrap (#1994)" _expect_wrapped_marker_recognized
step "recognize a marker split by the interactive ║ border (#2145)" _expect_interactive_wrapped_marker_recognized
# The command COMPUTES its marker (arithmetic expansion), so the sentinel we
# assert on — SELFTEST_42 — appears ONLY in the command's output, never in the
# echoed input line (which shows the literal $((6*7))). A shell that echoed
# input but failed to RUN it would therefore fail this step (Greptile P2).
# Single quotes are REQUIRED: the arithmetic must reach the pane's shell
# unexpanded, not be computed by this script.
# shellcheck disable=SC2016
step "type a command into the pane"                         af_send_to_pane 'echo SELFTEST_$((6*7))'
step "assert the pane RAN it (computed marker, not input echo)" \
    af_wait_for 'SELFTEST_42([^0-9]|$)' 10 'computed pane output'
step "exit interactive mode"                                af_exit_interactive

step "attach full-screen"                                   af_attach
step "detach (and prove the attach client was reaped)"      af_detach

# --- #1995 regression: a following Enter must not overtake an attach paste ---
# af_send_line asserts the pasted line fully echoed before submitting, so it can
# never execute a truncated command the way a bare af_send_literal + af_send
# Enter can. Deterministic stub proof — no live TUI.
step "attach send-line does not submit a truncated command (#1995)" _expect_attach_send_no_truncation
step "short commands still wait for their echo (#1995)"      _expect_short_command_delivery_waits

# --- #2147 regression: an unconfirmed paste must never become shell input ---
# The attach path really does drop bytes, so "submit best-effort" turns a
# ~120-char quoted command into a 32-byte prefix, bash into its `>` continuation
# prompt, and every later scripted command into a continuation line. Two stub
# proofs (no live TUI) then the live 80x24 attach the daily play-test found it in.
step "a dropped middle chunk must not confirm delivery (#2147)" _expect_partial_middle_not_confirmed
step "af_send_line REFUSES to submit a partial line (#2147)"   _expect_send_line_refuses_partial
step "live attach: a long quoted line lands whole and RUNS (#2147)" _expect_live_attach_send_line

step "assert selection survived attach/detach"              af_expect_selected beta
step "assert no orphan tmux attach clients"                 af_assert_no_orphan_clients
step "pane text must not inflate the tab count (#1561 review)" _expect_pane_text_not_counted_as_tabs

# --- #1832 regression: detach when the pane program has upgraded the keyboard ---
# Full-screen attach is a raw byte proxy, so an agent CLI that negotiates a
# richer keyboard encoding with the REAL terminal (claude emits `CSI > 1 u` and
# `CSI > 4 ; 2 m` at startup) makes it report ctrl+w as an escape sequence rather
# than 0x17 — and the user was left with no way out of the attach.
#
# The sandbox terminal is not kitty, so these steps inject the exact bytes such a
# terminal sends for ctrl+w. That reproduces #1832 here without needing kitty:
# before the fix both steps hang in the attach until they time out.
step "attach full-screen (#1832 kitty encoding)"            af_attach
step "detach via the kitty CSI u ctrl+w encoding (#1832)"   af_detach $'\e[119;5u'
step "attach full-screen (#1832 modifyOtherKeys encoding)"  af_attach
step "detach via the modifyOtherKeys ctrl+w encoding (#1832)" af_detach $'\e[27;5;119~'
step "assert selection survived the encoded detaches (#1832)" af_expect_selected beta

# A pane program that also turns on kitty's event-type reporting makes ONE tap of
# ctrl+w report twice — a press then a release — which a single read batches. Both
# halves are the same keypress, so the tap must detach and must not leak its press
# half into the pane on the way out.
step "attach full-screen (batched kitty press+release tap)" af_attach
step "detach via a batched press+release ctrl+w tap"        af_detach $'\e[119;5:1u\e[119;5:3u'
step "assert selection survived the batched tap"            af_expect_selected beta

# --- #1822 regression: af_hide_pane across the multi-pane and single-pane flows ---
step "hide a pane while another remains, then the last (#1822)" _expect_multipane_hide
step "af_hide_pane fails loudly with no pane focused (#1822)"  _expect_hide_pane_rejects_tree_focus

# --- #1884/#1885 multi-tab pane cycling: number jumps land + w closes the viewed tab ---
# Runs last: it creates its own 'cycle' instance and ends with a pane open.
step "cycle: create a multi-tab instance"                   af_new_instance cycle
step "cycle: select it"                                     af_select cycle
step "cycle: add a shell tab (t)"                           af_new_tab
step "cycle: add a shell-2 tab (t)"                         af_new_tab
step "cycle: open the active tab as a pane"                 af_open_pane
step "cycle: pane number-jumps land and STAY (#1885)"       _expect_cycle_jumps_land
step "cycle: w closes the VIEWED tab, not the tree's (#1884)" _expect_cycle_w_closes_viewed

# --- #2148 live: relaunch at 80x24 with three sessions and the lowest selected.
# Runs last because it needs a third instance ('cycle') to make the rail scroll,
# and because it relaunches the TUI.
step "relaunch boots with the rail scrolled past the header (#2148)" _expect_scrolled_rail_relaunch

printf '\n=== SELF-TEST PASSED — %d/%d steps green ===\n' "$PASS" "$PASS"
printf 'the #1156 mis-drive is now a deterministic scenario.\n'

# Leave the sandbox as we found it (best-effort; container teardown is the
# real cleanup).
af_quit >/dev/null 2>&1 || true
exit 0
