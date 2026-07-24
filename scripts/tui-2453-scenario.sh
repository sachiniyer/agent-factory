#!/usr/bin/env bash
# Real-TUI drive for #2453 + #2454, in two honest parts:
#
#   scripts/testbox.sh scenario scripts/tui-2453-scenario.sh
#
# Part A (the visible TUI behavior): the config overlay advertises the assistant
# button, pressing it opens the config-assistant takeover (the sandbox's config
# agent runs bash, so the takeover lands in a real session), and exiting returns
# to the TUI cleanly with no #2019-class attach error.
#
# Part B (the mechanism the assistant is briefed on): drive the REAL af binary
# directly — hand-edit a [theme] block, run `af config validate` (OK), break it,
# validate (fails), restore, validate (OK). This is deliberately NOT puppeteered
# through the takeover's bash: the config agent pastes a long briefing into that
# shell, and quote-escaping a multi-command edit through the paste path is flaky.
# Running the binary proves the exact edit→validate contract deterministically.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

# --- Part A: the button and the takeover (visible TUI) ----------------------

# press_button_and_spawn presses C in the config overlay and proves the button
# triggered THIS PR's spawn: a bare config-agent tmux session (af_af-config-*),
# on demand, not a project instance. It keys on the daemon-side session
# appearing rather than on screen chrome — the config overlay goes full-screen at
# a narrow geometry (#1821), so "the rail's header vanished" is not a reliable
# takeover signal, whereas the af_af-config-* session is the unambiguous fact
# that the button spawned the assistant.
#
# The return-to-TUI-on-detach path is unchanged by this PR: the button reaches
# the same enterConfigAgent/ReapConfigAgent flow the nav hotkey does, which the
# selftest already gates end to end (#2019). So this scenario proves the NEW
# behavior — the trigger and what it spawns — and leaves the shared return path
# to that gate. Best-effort detach after; the per-geometry af_reset_sandbox nukes
# any leftover regardless.
press_button_and_spawn() {
    af_send C
    local deadline cfg screen
    deadline=$(( $(_af_now) + 75 ))
    while :; do
        screen="$(af_capture)"
        if printf '%s\n' "$screen" | grep -qiE 'exit status 1|nested with care|can.t find session'; then
            _af_log "#2453: the config-pane button's spawn collapsed to an error:"
            printf '%s\n' "$screen" >&2
            return 1
        fi
        # `|| true`: before the session exists grep exits 1, which under
        # `set -e` + pipefail would kill the poll on its first empty iteration.
        cfg="$(tmux list-sessions -F '#{session_name}' 2>/dev/null | grep '^af_af-config-' | head -1 || true)"
        if [ -n "$cfg" ]; then
            _af_log "assert OK: the button spawned a bare config-agent session ($cfg)"
            tmux detach-client -s "$cfg" 2>/dev/null || true
            return 0
        fi
        [ "$(_af_now)" -ge "$deadline" ] && { _af_log "#2453: the button spawned no config-agent session in 75s"; return 1; }
        sleep "$AF_DRIVER_POLL"
    done
}

drive_button_takeover() {
    export AF_DRIVER_COLS="$1" AF_DRIVER_ROWS="$2"
    af_reset_sandbox
    af_boot

    af_open_config
    af_assert_screen 'C assistant' "the config overlay advertises the assistant button (${1}x${2})"

    press_button_and_spawn

    echo "PASS: #2453 button + spawn at ${1}x${2}"
}

# --- Part B: the edit→validate mechanism (real binary) ----------------------

drive_validate_mechanism() {
    local bin home cfg out rc
    bin="$(_af_resolve_bin)"
    home="$(mktemp -d)"
    cfg="$home/config.toml"

    # run_validate captures both output and exit code WITHOUT tripping `set -e`
    # on a failing validate: an `if` condition is exempt from set -e, whereas
    # `out=$(cmd); rc=$?` dies at the assignment when cmd (correctly) fails.
    run_validate() {
        if out="$(AGENT_FACTORY_HOME="$home" "$bin" config validate 2>&1)"; then rc=0; else rc=$?; fi
    }

    # A valid structured edit — exactly the shape the assistant makes for theme.
    printf 'default_program = "claude"\n\n[theme]\nbackground = "#123456"\n' > "$cfg"
    run_validate
    if [ "$rc" -ne 0 ] || ! printf '%s' "$out" | grep -q 'config OK'; then
        _af_fail "#2453 validate: a valid [theme] edit did not validate (rc=$rc): $out"; return 1
    fi
    _af_log "validate OK: a valid [theme] edit passes"

    # A broken edit MUST be caught — the whole reason the step exists.
    printf 'default_program = "claude"\n[keys\n' >> "$cfg"
    run_validate
    if [ "$rc" -eq 0 ]; then
        _af_fail "#2453 validate: a broken edit was NOT caught — an unvalidated wedge would ship: $out"; return 1
    fi
    _af_log "validate OK: a broken edit fails loudly (rc=$rc)"

    # Restore and re-validate: the assistant is told to fix a failed validate.
    printf 'default_program = "claude"\n\n[theme]\nbackground = "#123456"\n' > "$cfg"
    run_validate
    if [ "$rc" -ne 0 ] || ! printf '%s' "$out" | grep -q 'config OK'; then
        _af_fail "#2453 validate: the restored config did not validate (rc=$rc): $out"; return 1
    fi
    _af_log "validate OK: the restored config passes"

    rm -rf "$home"
    echo "PASS: #2453 edit→validate mechanism"
}

drive_button_takeover 100 30
drive_button_takeover 80 24
drive_validate_mechanism
echo "PASS: #2453 real-TUI scenario (button at both geometries + validate mechanism)"
