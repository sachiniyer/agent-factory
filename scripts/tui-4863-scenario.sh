#!/usr/bin/env bash
# Real-TUI + CLI drive for #4863's shift+<rune> dead-binding fix.
#
#   scripts/testbox.sh scenario scripts/tui-4863-scenario.sh
#
# The bug: `normalizeKeySpec` accepted `quit = ["shift+a"]` (and the
# ctrl/alt+shift variants) and installed the binding under the spelling
# "shift+a", which Bubble Tea can never emit (Shift+A -> "A"). The binding
# was dead on arrival; config validation — the one place to catch it — missed
# it because the emit-ability guard was gated on named keys.
#
# The fix rejects shift+<rune> at the WRITE path (`af config set keys`, via
# keys.ValidateOverrides), but a config that ALREADY contains one must not
# break the upgrade: ValidateOverrides runs in every LoadConfig, including the
# daemon's, so a hard error would turn a silently inert binding into a refusal
# to start. So the LOAD path applies the "warn now, reject later" policy
# (#4599): warn naming the key and saying it will never fire, then skip it.
#
# This scenario proves the fix end-to-end through the REAL af binary:
#   Part A (deterministic, the validation contract that gates boot):
#     `af config validate` ACCEPTS a config whose quit is a dead shift+<rune>
#     override, printing a "will never fire" warning that names the dead key
#     alongside "config OK", and it still ACCEPTS the reachable rune combos
#     (Q, alt+a, ctrl+a).
#   Part B (the boot the policy gates): booting af with `quit = ["shift+a"]`
#     REACHES the TUI first frame — the load warned and skipped the dead
#     binding instead of refusing to start — and the dead binding is skipped,
#     not installed: Shift+A (emitted as "A") does NOT quit, while the default
#     `q` (restored when the dead override was dropped) still does.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# cheap_config <keys-toml> — write a sandbox config.toml whose instances are
# bash, then append a [keys] block. The container's validate path is identical
# to a user's: LoadConfigReadOnly runs discardDeadShiftRuneOverrides ->
# keys.ValidateOverrides.
cheap_config() {
    printf 'default_program = "claude"\n\n[program_overrides]\nclaude = "bash"\n\n%s\n' "$1"
}

# run_validate captures output and exit code WITHOUT tripping set -e (an `if`
# condition is exempt), mirroring scripts/tui-2453-scenario.sh's Part B.
run_validate() {
    if out="$(AGENT_FACTORY_HOME="$home" "$bin" config validate 2>&1)"; then rc=0; else rc=$?; fi
}

# expect_warn_accept <spec> <label> — `af config validate` ACCEPTS a config
# whose quit is a dead shift+<rune> override (warn-and-skip), and the output
# names the dead key in a "will never fire" warning alongside the "config OK"
# line. The load path warns and skips; only the write path (`af config set
# keys`) rejects a NEW dead binding.
expect_warn_accept() {
    local spec="$1" label="$2"
    cheap_config "[keys]\nquit = [\"$spec\"]" > "$cfg"
    # cheap_config uses a literal \n; convert to real newlines.
    printf '%b' "$(cat "$cfg")" > "$cfg"
    run_validate
    if [ "$rc" -ne 0 ]; then
        _af_fail "#4863: $label: $spec was REJECTED; the load path should warn-and-skip an existing dead binding. out=$out"; return 1
    fi
    if ! printf '%s' "$out" | grep -q 'config OK'; then
        _af_fail "#4863: $label: $spec accepted but no 'config OK' line. out=$out"; return 1
    fi
    if ! printf '%s' "$out" | grep -qF "\"$spec\""; then
        _af_fail "#4863: $label: $spec accepted but the warning did not name the dead key. out=$out"; return 1
    fi
    if ! printf '%s' "$out" | grep -q 'will never fire'; then
        _af_fail "#4863: $label: $spec accepted but the warning did not say it will never fire. out=$out"; return 1
    fi
    _af_log "assert OK: $label: $spec accepted at config validation with a dead-key warning (rc=$rc)"
}

expect_accept() {
    local action="$1" spec="$2" label="$3"
    cheap_config "[keys]\n$action = [\"$spec\"]" > "$cfg"
    printf '%b' "$(cat "$cfg")" > "$cfg"
    run_validate
    if [ "$rc" -ne 0 ]; then
        _af_fail "#4863: $label: $action=$spec was REJECTED; the shift fix must not reject reachable combos. out=$out"; return 1
    fi
    if ! printf '%s' "$out" | grep -q 'config OK'; then
        _af_fail "#4863: $label: $spec accepted but no 'config OK' line. out=$out"; return 1
    fi
    _af_log "assert OK: $label: $action=$spec accepted (reachable combo preserved)"
}

drive_validate_contract() {
    local bin home cfg out rc
    bin="$(_af_resolve_bin)"
    home="$(mktemp -d)"
    cfg="$home/config.toml"

    # Part A.1 — every shift+-inclining rune combo is dead, but an EXISTING one
    # must load (warn-and-skip): validate exits 0 with "config OK" AND a
    # warning that names the dead key and says it will never fire.
    expect_warn_accept "shift+a"            "letter (shift+)"
    expect_warn_accept "ctrl+shift+a"       "letter (ctrl+shift+)"
    expect_warn_accept "alt+shift+a"        "letter (alt+shift+)"
    expect_warn_accept "alt+ctrl+shift+a"   "letter (alt+ctrl+shift+)"
    expect_warn_accept "shift+0"            "digit (shift+)"
    expect_warn_accept "shift+/"            "symbol (shift+)"
    expect_warn_accept "shift+å"            "multi-byte rune (shift+)"

    # Part A.2 — the reachable combos Bubble Tea CAN emit still validate. The
    # fix narrowed only the dead path; over-rejection here would be a regression.
    expect_accept "quit" "Q"            "uppercase rune"
    expect_accept "new"  "alt+a"        "alt+rune"
    expect_accept "up"   "ctrl+a"       "ctrl+letter"
    expect_accept "up"   "alt+ctrl+a"   "alt+ctrl+letter"

    rm -rf "$home"
    echo "PASS: #4863 config validate warns+skips dead shift+<rune>, reachable combos preserved"
}

# drive_boot_warns_and_skips_dead_keys — the boot the warn-and-skip policy
# gates. With `quit = ["shift+a"]` in config.toml, af must now REACH the TUI
# first frame: the load warned and skipped the dead override (instead of
# refusing to start, the old reject policy's failure mode over an inert
# binding). And the dead binding is skipped, not installed: pressing Shift+A
# — which Bubble Tea emits as the rune "A", never "shift+a" — must NOT quit,
# while the default `q` (restored when the dead override was dropped) still
# does.
drive_boot_warns_and_skips_dead_keys() {
    export AF_DRIVER_COLS=100 AF_DRIVER_ROWS=30
    export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

    af_reset_sandbox
    # Seed a config so the only thing notable is the dead key binding.
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
    # for the first frame, which IS the assertion here, so launch directly and
    # wait on it).
    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    tmux new-session -d -s "$AF_DRIVER_SESSION" -x "$AF_DRIVER_COLS" -y "$AF_DRIVER_ROWS"
    tmux set-option -t "$AF_DRIVER_SESSION" window-size manual >/dev/null 2>&1 || true
    af_send_literal "cd $AF_DRIVER_REPO && $bin"
    af_send Enter

    # The boot the warn-and-skip policy gates: af REACHES the TUI first frame
    # despite the dead shift+a override — the load warned and skipped it
    # instead of refusing to start.
    af_wait_for 'Agent Factory' "$AF_DRIVER_TIMEOUT" \
        'af boots with a dead shift+a override (warn-and-skip, not refused)' || return 1
    af_ensure_nav

    # THE first assertion: the dead binding is skipped, not installed. Pressing
    # Shift+A — which Bubble Tea emits as the rune "A", never "shift+a" — must
    # NOT quit. tmux send-keys "A" sends the literal uppercase rune, the exact
    # bytes a real Shift+A produces. The TUI stays on the first frame.
    af_send A
    af_wait_for 'Agent Factory' 5 \
        'Shift+A does not quit (dead shift+a override was skipped, not installed)' || return 1

    # THE second assertion: the default `q` is restored — the dead override was
    # dropped, reverting quit to its default — so `q` still quits and af exits
    # back to the shell.
    af_send q
    af_wait_gone 'Agent Factory' "$AF_DRIVER_TIMEOUT" \
        'q still quits after the dead shift+a override was dropped (default restored)' || return 1

    # Cleanup the tmux session so the box is reusable.
    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    echo "PASS: #4863 af boots with a dead shift+a override (warned and skipped); Shift+A does not quit, q still does"
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
            _af_fail "#4863: n still opened the new prompt after the alt+a override replaced the default"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
    _af_log "assert OK: default n is released by the reachable alt+a override"

    tmux kill-session -t "$AF_DRIVER_SESSION" 2>/dev/null || true
    echo 'PASS: #4863 reachable alt+a override boots, dispatches the new action, and releases the default n'
}

drive_validate_contract
drive_boot_warns_and_skips_dead_keys
drive_reachable_alt_rune_dispatches
echo 'PASS: #4863 shift+<rune> dead bindings warn-and-skip on load; write path rejects; reachable combos preserved'
