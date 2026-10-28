#!/usr/bin/env bash
# Run only via scripts/testbox.sh scenario or in its isolated playtest container.
# Real tmux driver: first-run -> task manager -> failed create -> retained form ->
# successful stand-in session -> first attached terminal, plus tiny geometry.
set -euo pipefail
source /src/scripts/tui-driver.sh
export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=36
mkdir -p "$HOME/sandbox/recovery-live"
af_boot
af_wait_for 'No sessions yet'
af_refute_screen 'No tasks —'
af_capture > "$HOME/sandbox/recovery-live/zero-sessions.txt"
af_send m
af_wait_for 'No tasks'
af_capture > "$HOME/sandbox/recovery-live/zero-tasks.txt"
af_send Escape
af_wait_gone 'No tasks'
# A declared docker backend is refused by this sandbox's real daemon.
mkdir -p "$AF_DRIVER_REPO/.agent-factory"
printf '{"backend":"docker"}\n' > "$AF_DRIVER_REPO/.agent-factory/config.json"
af_focus_tree
af_send n
af_wait_for 'submit name'
af_send_literal 'retained-draft'
af_send Enter
af_wait_for 'Cannot create session'
af_capture > "$HOME/sandbox/recovery-live/create-failed.txt"
af_send Space
af_wait_for 'submit name'
af_assert_screen 'retained-draft'
# The draft can recover by selecting a supported backend without losing its name.
af_send C-r
af_wait_for 'Select backend'
af_send Down
af_send Enter
af_wait_for 'submit name'
rm "$AF_DRIVER_REPO/.agent-factory/config.json"
af_send Enter
af_wait_for 'retained-draft.*●'
af_select retained-draft
af_open_pane
af_enter_interactive
af_send_to_pane 'echo FIRST_SESSION_REACHED'
af_wait_for 'FIRST_SESSION_REACHED'
af_capture > "$HOME/sandbox/recovery-live/first-session.txt"
af_exit_interactive
af_resize 39 9
af_wait_for 'Terminal too small'
af_capture > "$HOME/sandbox/recovery-live/too-small.txt"
printf 'PASS #3915 real driver onboarding, retained failed create, task empty, terminal fallback\n'
