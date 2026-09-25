#!/usr/bin/env bash
# Real-TUI + CLI drive for #4040's shift+<rune> dead-binding fix.
#
#   scripts/testbox.sh scenario scripts/tui-4040-scenario.sh
#
# The bug: `normalizeKeySpec` accepted `quit = ["shift+a"]` (and the
# ctrl/alt+shift variants) and installed the binding under the spelling
# "shift+a", which Bubble Tea can never emit (Shift+A -> "A"). The binding
# was dead on arrival; config validation — the one place to catch it — missed
# it because the emit-ability guard was gated on named keys.
#
# This scenario proves the fix end-to-end through the REAL af binary:
#   Part A (deterministic, the validation contract that gates boot):
#     `af config validate` rejects shift+a / ctrl+shift+a / alt+shift+a /
#     alt+ctrl+shift+a with a non-zero exit and a message naming the dead key,
#     while it ACCEPTS the reachable rune combos the fix must preserve
#     (Q, alt+a, ctrl+a).
#   Part B (the boot the validation gates): booting af with `quit = ["shift+a"]`
#     does NOT reach the TUI first frame — the launch is refused at config
#     load, exactly the failure mode the fix turns from a silent dead binding
#     into a loud boot error.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# cheap_config <keys-toml> — write a sandbox config.toml whose instances are
# bash, then append a [keys] block. The container's validate path is identical
# to a user's: LoadConfigReadOnly runs normalizeOverrides -> normalizeKeySpec.
cheap_config() {
    printf 'default_program = "claude"\n\n[program_overrides]\nclaude = "bash"\n\n%s\n' "$1"
}

# run_validate captures output and exit code WITHOUT tripping set -e (an `if`
# condition is exempt), mirroring scripts/tui-2453-scenario.sh's Part B.
run_validate() {
    if out="$(AGENT_FACTORY_HOME="$home" "$bin" config validate 2>&1)"; then rc=0; else rc=$?; fi
}

expect_reject() {
    local spec="$1" label="$2"
    cheap_config "[keys]\nquit = [\"$spec\"]" > "$cfg"
    # cheap_config uses a literal \n; convert to real newlines.
    printf '%b' "$(cat "$cfg")" > "$cfg"
    run_validate
    if [ "$rc" -eq 0 ]; then
        _af_fail "#4040: $label: $spec was ACCEPTED; expected a validation error"; return 1
    fi
    if ! printf '%s' "$out" | grep -qF "\"$spec\" is not a valid key"; then
        _af_fail "#4040: $label: $spec rejected, but the error did not name the dead key. out=$out"; return 1
    fi
    _af_log "assert OK: $label: $spec rejected at config validation naming the dead key (rc=$rc)"
}

expect_accept() {
    local action="$1" spec="$2" label="$3"
    cheap_config "[keys]\n$action = [\"$spec\"]" > "$cfg"
    printf '%b' "$(cat "$cfg")" > "$cfg"
    run_validate
    if [ "$rc" -ne 0 ]; then
        _af_fail "#4040: $label: $action=$spec was REJECTED; the shift fix must not reject reachable combos. out=$out"; return 1
    fi
    if ! printf '%s' "$out" | grep -q 'config OK'; then
        _af_fail "#4040: $label: $spec accepted but no 'config OK' line. out=$out"; return 1
    fi
    _af_log "assert OK: $label: $action=$spec accepted (reachable combo preserved)"
}

drive_validate_contract() {
    local bin home cfg out rc
    bin="$(_af_resolve_bin)"
    home="$(mktemp -d)"
    cfg="$home/config.toml"

    # Part A.1 — every shift+-inclining rune combo is dead and must be rejected,
    # with an error that names the offending key string.
    expect_reject "shift+a"            "letter (shift+)"
    expect_reject "ctrl+shift+a"       "letter (ctrl+shift+)"
    expect_reject "alt+shift+a"        "letter (alt+shift+)"
    expect_reject "alt+ctrl+shift+a"   "letter (alt+ctrl+shift+)"
    expect_reject "shift+0"            "digit (shift+)"
    expect_reject "shift+/"            "symbol (shift+)"
    expect_reject "shift+å"            "multi-byte rune (shift+)"

    # Part A.2 — the reachable combos Bubble Tea CAN emit still validate. The
    # fix narrowed only the dead path; over-rejection here would be a regression.
    expect_accept "quit" "Q"            "uppercase rune"
    expect_accept "new"  "alt+a"        "alt+rune"
    expect_accept "up"   "ctrl+a"       "ctrl+letter"
    expect_accept "up"   "alt+ctrl+a"   "alt+ctrl+letter"

    rm -rf "$home"
    echo "PASS: #4040 config validate shifts+rune rejected, reachable combos preserved"
}

# drive_boot_refused_with_dead_keys — the boot the validation gate protects.
# With `quit = ["shift+a"]` in config.toml, launching af must NOT reach the TUI
# first frame ("Agent Factory"): the config load refuses and the process exits.
# Before the fix this WOULD have booted and silently installed an unreachable
# quit binding; now it refuses loudly.
drive_boot_refused_with_dead_keys() {
    export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    # Seed a config so the only thing wrong is the dead key binding.
    af_set_config "$(cat <<'TOML'
default_program = "claude"

[program_overrides]
claude = "bash"

[keys]
quit = ["shift+a"]
TOML
)"

    local bin
    bin="$(_af_resolve_bin)"

    # Launch af directly in the driver session (NOT via af_boot — af_boot waits
    # for the first frame, which must NOT arrive here).
    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    tmux new-session -d -s "$AF_DRIVER_SESSION" -x "$AF_DRIVER_COLS" -y "$AF_DRIVER_ROWS"
    tmux set-option -t "$AF_DRIVER_SESSION" window-size manual >/dev/null 2>&1 || true
    af_send_literal "cd $AF_DRIVER_REPO && $bin"
    af_send Enter

    # Poll a short deadline: the first frame ("Agent Factory") must NOT arrive,
    # while the config error naming the dead key DOES appear on the pane.
    local deadline screen saw_first_frame saw_error
    deadline=$(( $(_af_now) + 25 ))
    saw_first_frame=no; saw_error=no
    while [ "$(_af_now)" -lt "$deadline" ]; do
        screen="$(af_capture)"
        if printf '%s\n' "$screen" | grep -qE 'Agent Factory'; then
            saw_first_frame=yes
            break
        fi
        if printf '%s\n' "$screen" | grep -qE 'shift\+a.*is not a valid key|is not a valid key'; then
            saw_error=yes
        fi
        # af exited back to a shell prompt — config load refused.
        if printf '%s\n' "$screen" | grep -qE '\$ $'; then
            [ "$saw_error" = yes ] && break || true
        fi
        sleep "$AF_DRIVER_POLL"
    done

    if [ "$saw_first_frame" = yes ]; then
        _af_fail "#4040: af booted the TUI despite a dead shift+a binding — the fix did not gate boot"
        printf '%s\n' "$screen" >&2
        return 1
    fi
    if [ "$saw_error" = no ]; then
        _af_fail "#4040: af did not boot but no dead-key error was visible on the pane"
        printf '%s\n' "$screen" >&2
        return 1
    fi
    _af_log "assert OK: af refuses to boot with quit=[shift+a], surfacing the dead-key error instead"

    # Cleanup the tmux session so the box is reusable.
    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    echo "PASS: #4040 af boot refused with a dead shift+<rune> binding"
}

# drive_reachable_alt_rune_dispatches — the no-regression leg, mirroring the
# plan's step (b). A reachable rune+modifier override loads at boot AND
# dispatches on the live key. `new = ["alt+a"]` replaces the default `n`, so
# Alt+a MUST open the name prompt (the new action). This is the half the fix
# must preserve: rejecting dead shift+<rune> combos must not also reject the
# reachable alt+<rune> path.
#
# Driven MANUALLY (not via af_boot) because af_boot's tail calls af_focus_tree,
# whose menu-row matcher is hardcoded to the default footer chips ('n new'
# and '? help · q quit'). Overriding `new` changes the 'n new' chip to
# 'alt+a new', which would falsely fail af_focus_tree even though the TUI is
# perfectly healthy. Launching in the driver session directly and waiting on
# 'Agent Factory' isolates the boot assertion from the footer-chip assumption.
drive_reachable_alt_rune_dispatches() {
    export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    af_set_config "$(cat <<'TOML'
default_program = "claude"

[program_overrides]
claude = "bash"

[keys]
new = ["alt+a"]
TOML
)"

    local bin
    bin="$(_af_resolve_bin)"

    # Manual launch (af_boot's af_focus_tree assumes the default footer chips).
    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    tmux new-session -d -s "$AF_DRIVER_SESSION" -x "$AF_DRIVER_COLS" -y "$AF_DRIVER_ROWS"
    tmux set-option -t "$AF_DRIVER_SESSION" window-size manual >/dev/null 2>&1 || true
    af_send_literal "cd $AF_DRIVER_REPO && $bin"
    af_send Enter
    # Reaching the first frame proves the reachable alt+a override loads at
    # boot — the mirror of Part B, where the dead shift+a override refused boot.
    af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" 'af boots with the reachable alt+a override' || return 1
    af_ensure_nav

    # THE assertion: Alt+a dispatches the new action — the name prompt opens.
    # tmux's `M-a` sends Alt+a, the exact keycombo Bubble Tea emits as "alt+a"
    # and normalizeKeySpec accepts (KeyRunes + Alt). Before the fix this would
    # ALSO have worked (alt+rune was always reachable); the point is that it
    # STILL works after the shift+<rune> fix narrowed the dead path.
    af_send M-a
    af_wait_for 'submit name' "$AF_DRIVER_TIMEOUT" \
        'Alt+a opens the new-session prompt (reachable override dispatches live)' || return 1
    _af_log "assert OK: alt+a (reachable rune+modifier) boots and dispatches the new action"

    # The default `n` must be released by the override (new = ["alt+a"] REPLACES
    # the default). Dismiss the prompt and confirm `n` no longer opens it.
    af_send Escape
    af_wait_gone 'submit name' "$AF_DRIVER_TIMEOUT" 'name prompt dismissed' || true
    af_send n
    # Give the keystroke a moment to land, then assert the prompt did NOT open.
    local deadline screen
    deadline=$(( $(_af_now) + 3 ))
    while [ "$(_af_now)" -lt "$deadline" ]; do
        screen="$(af_capture)"
        if printf '%s\n' "$screen" | grep -qE 'submit name'; then
            _af_fail "#4040: n still opened the new prompt after the alt+a override replaced the default"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
    _af_log "assert OK: default n is released by the reachable alt+a override"

    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    echo 'PASS: #4040 reachable alt+a override boots, dispatches the new action, and releases the default n'
}

drive_validate_contract
drive_boot_refused_with_dead_keys
drive_reachable_alt_rune_dispatches
echo 'PASS: #4040 shift+<rune> dead bindings rejected at validation and boot; reachable combos preserved'
