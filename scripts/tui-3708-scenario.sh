#!/usr/bin/env bash
# Real-TUI drive for #3708 (the `,` config editor ignored --daemon-url), via:
#
#   scripts/testbox.sh scenario scripts/tui-3708-scenario.sh
#
# The bug: the editor read this machine's config in-process and wrote this
# machine's control socket, in a session pointed at another host. An operator
# running `af --daemon-url http://box:8443` opened `,`, saw their own laptop's
# values under their own path, edited one, and changed their own laptop — with a
# success line saying so, and nothing on screen admitting the target had been
# ignored.
#
# Why this cannot be a unit test alone. The ui/ tests drive the pane's save seam
# and a stub daemon; they prove the routing decision and the no-fallback rule,
# but they never launch af, never parse --daemon-url, and never render a header
# to a terminal. The three things that only a real drive can show are exactly the
# three an operator relies on: that the flag reaches this pane at all through the
# real root command, that the values on screen are the OTHER machine's, and that
# after an edit the two config.toml files on disk have diverged in the right
# direction.
#
# The fixture is a SECOND af daemon in this same container, with its own AF home
# and a loopback listener. Same box, two homes: that is the smallest thing that
# makes "which machine" a question with a checkable answer, and it keeps the
# whole scenario inside the container fence.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

REMOTE_HOME="$HOME/sandbox/remote-home"
REMOTE_PORT="${AF_3708_PORT:-8899}"
REMOTE_URL="http://127.0.0.1:$REMOTE_PORT"
REMOTE_TOML="$REMOTE_HOME/config.toml"
LOCAL_TOML="$AGENT_FACTORY_HOME/config.toml"

# The two agents each home starts on. They must differ: every assertion below is
# "which of these two strings is on screen / in that file", and identical values
# would make a completely unrouted editor pass.
REMOTE_AGENT="aider"
LOCAL_AGENT="claude"
# What the drive edits it to. A third distinct value, so "the edit landed" and
# "the edit landed on the right machine" stay separate questions.
EDITED_AGENT="codex"

REMOTE_PID=""

# start_remote_daemon brings up the stand-in "other host": a second af daemon on
# its own AF home, serving the HTTP API on loopback. Tokenless, because the
# listener is loopback-only inside a container.
start_remote_daemon() {
    local bin; bin="$(_af_resolve_bin)"
    # Idempotent start. A leg that FAILS before its own stop_remote_daemon leaves
    # the previous daemon holding the port, and the next leg's health probe would
    # then be answered by that stale daemon — whose home this function is about to
    # delete. Observed on the run that found the clipped header: leg 1 timed out
    # and leg 2 reported a daemon "up" that was not the one it started.
    stop_remote_daemon
    rm -rf "$REMOTE_HOME"
    mkdir -p "$REMOTE_HOME"
    cat >"$REMOTE_TOML" <<EOF
schema_version = 1
# hand-written, and it must survive an edit through the pane
default_program = "$REMOTE_AGENT"
listen_addr = "127.0.0.1:$REMOTE_PORT"
EOF
    AGENT_FACTORY_HOME="$REMOTE_HOME" AGENT_FACTORY_AUTO_UPDATE=false \
        "$bin" --daemon >"$REMOTE_HOME/daemon.log" 2>&1 &
    REMOTE_PID=$!

    local deadline; deadline=$(( $(_af_now) + 60 ))
    until curl -fsS "$REMOTE_URL/v1/health" >/dev/null 2>&1; do
        if ! kill -0 "$REMOTE_PID" 2>/dev/null; then
            _af_fail "the remote daemon exited during startup:"; cat "$REMOTE_HOME/daemon.log" >&2; return 1
        fi
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "the remote daemon never served $REMOTE_URL/v1/health:"; cat "$REMOTE_HOME/daemon.log" >&2; return 1
        fi
        sleep 0.5
    done
    _af_log "remote daemon up on $REMOTE_URL (pid $REMOTE_PID, home $REMOTE_HOME)"
}

# stop_remote_daemon reaps ONLY the pid this script started. Never a pkill: the
# container hosts the sandbox's own daemon and tmux server too.
stop_remote_daemon() {
    [ -n "$REMOTE_PID" ] || return 0
    kill "$REMOTE_PID" 2>/dev/null || true
    wait "$REMOTE_PID" 2>/dev/null || true
    REMOTE_PID=""
}
trap stop_remote_daemon EXIT

# seed_local_config gives THIS machine a different agent, so a pane that fell
# back to the in-process read shows the wrong string rather than nothing.
seed_local_config() {
    # af_set_config is the driver's own writer for the sandbox config.toml.
    # af_reset_sandbox deliberately KEEPS config.toml (it mirrors `af reset`'s
    # state/configuration split), so each leg re-seeds rather than inheriting
    # whatever the previous leg's edit left behind.
    #
    # The [program_overrides] block is not decoration. Writing config.toml at all
    # shadows the sandbox's config.json (#1030), which is where the container put
    # its cheap-agent overrides — so without these, this scenario would leave the
    # sandbox trying to exec a real agent binary, and anything run afterwards in a
    # REUSED container (the driver self-test, say) fails at "create instance".
    # Every agent these legs can leave behind is mapped, including the edited one.
    af_set_config "schema_version = 1
default_program = \"$LOCAL_AGENT\"

[program_overrides]
$LOCAL_AGENT = \"bash\"
$REMOTE_AGENT = \"bash\"
$EDITED_AGENT = \"bash\""
}

# open_config_editor presses `,` from nav mode and waits for the pane.
open_config_editor() {
    af_ensure_nav
    af_focus_tree
    af_send ','
    # The first manifest row is the marker. NOT the header's path: the header is
    # clipped to the overlay width, and a remote label ends "…/remote-home/confi…"
    # with "config.toml" truncated away — the very clipping this scenario asserts
    # on. NOT a bare "Config" either, which would also match chrome.
    af_wait_for 'default_program' "$AF_DRIVER_TIMEOUT" 'config editor overlay' || return 1
}

# edit_selected_key replaces the value of the row under the cursor. The pane
# opens on default_program (the first tier-1 manifest entry) and pre-fills the
# field with the live value, so the field is cleared before typing.
edit_selected_key() {
    local value="$1"
    af_send Enter
    af_wait_for 'save' "$AF_DRIVER_TIMEOUT" 'value field open' || return 1
    # Clear the pre-filled value one keystroke at a time; the field takes runes,
    # so a paste of "" would be a no-op rather than a clear.
    local i
    for i in $(seq 1 40); do af_send BSpace; done
    af_send_literal "$value"
    af_send Enter
    af_wait_for "set default_program = $value" "$AF_DRIVER_TIMEOUT" 'the pane echoes the write' || return 1
}

# assert_file_has / assert_file_lacks are the on-disk half. The screen can be
# made to say anything; these two say which machine actually changed.
assert_file_has() {
    local path="$1" needle="$2" why="$3"
    if ! grep -Eq -- "$needle" "$path"; then
        _af_fail "$why — $path does not contain '$needle':"; cat "$path" >&2; return 1
    fi
    _af_log "assert OK: $why"
}

assert_file_lacks() {
    local path="$1" needle="$2" why="$3"
    if grep -Eq -- "$needle" "$path"; then
        _af_fail "$why — $path unexpectedly contains '$needle':"; cat "$path" >&2; return 1
    fi
    _af_log "assert OK: $why"
}

# drive_remote_target is #3708 itself: the whole round trip against another
# daemon, read and write.
drive_remote_target() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
    af_reset_sandbox
    seed_local_config
    start_remote_daemon || return 1

    # The one line that makes this a remote session. AF_DAEMON_URL is the env
    # spelling of --daemon-url and resolves through the identical code path
    # (apiclient/target.go), so it exercises the real flag plumbing.
    export AF_DRIVER_LAUNCH_ENV="AF_DAEMON_URL=$REMOTE_URL"
    af_boot || return 1
    open_config_editor || return 1

    local screen; screen="$(af_capture)"

    # THE read assertion. On master this shows "claude" — this machine's value —
    # in a session pointed at the other daemon.
    if ! printf '%s\n' "$screen" | grep -q "$REMOTE_AGENT"; then
        _af_fail "#3708: the pane is not showing the REMOTE daemon's default_program ($REMOTE_AGENT):"
        printf '%s\n' "$screen" >&2; return 1
    fi
    if printf '%s\n' "$screen" | grep -qE "default_program.*$LOCAL_AGENT"; then
        _af_fail "#3708: the pane is showing THIS machine's default_program ($LOCAL_AGENT) in a remote session:"
        printf '%s\n' "$screen" >&2; return 1
    fi
    _af_log "assert OK: the pane shows the remote daemon's values"

    # THE header assertion: the daemon URL first, then its own path,
    # un-abbreviated. The order is not cosmetic and this drive is what settled it
    # — the header is clipped at the OVERLAY's width (72 columns inside this
    # 120-column terminal, not 120), so a path-first label rendered as
    # "…/remote-home/config.toml on http://127.0.0…" and truncated away the very
    # half that says which machine. Asserting on the URL is therefore also the
    # regression test for that clipping.
    if ! printf '%s\n' "$screen" | grep -q "$REMOTE_URL"; then
        _af_fail "#3708: the header does not name the daemon ('$REMOTE_URL'):"
        printf '%s\n' "$screen" >&2; return 1
    fi
    if ! printf '%s\n' "$screen" | grep -q "$REMOTE_URL · $REMOTE_HOME"; then
        _af_fail "#3708: the header does not name the daemon followed by ITS OWN path ('$REMOTE_URL · $REMOTE_HOME/config.toml'):"
        printf '%s\n' "$screen" >&2; return 1
    fi
    _af_log "assert OK: the header names the daemon, then its own path, with the URL intact after clipping"

    # THE write assertion, in both directions.
    edit_selected_key "$EDITED_AGENT" || return 1
    assert_file_has "$REMOTE_TOML" "default_program = '$EDITED_AGENT'" \
        "the edit reached the REMOTE daemon's config" || return 1
    assert_file_has "$REMOTE_TOML" "# hand-written" \
        "the remote write preserved the file's comments" || return 1
    assert_file_has "$LOCAL_TOML" "default_program = \"$LOCAL_AGENT\"" \
        "THIS machine's config was left exactly as it was" || return 1
    # The KEY, not the bare agent name: the seed's [program_overrides] maps every
    # agent this scenario can produce to bash, so "codex" legitimately appears in
    # this file. What must not appear is codex as this machine's default_program.
    assert_file_lacks "$LOCAL_TOML" "default_program = ['\"]$EDITED_AGENT['\"]" \
        "the edit did not also land on this machine as its default_program" || return 1

    af_send Escape
    unset AF_DRIVER_LAUNCH_ENV
    stop_remote_daemon
}

# drive_config_assistant_is_refused covers the adjacent surface. The assistant
# hand-edits the config file next to it — this machine's — so under a remote
# target it must refuse by name rather than quietly administer the wrong host,
# one keystroke away from the pane that now administers the right one.
drive_config_assistant_is_refused() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
    af_reset_sandbox
    seed_local_config
    start_remote_daemon || return 1

    export AF_DRIVER_LAUNCH_ENV="AF_DAEMON_URL=$REMOTE_URL"
    af_boot || return 1
    open_config_editor || return 1
    af_send C
    af_wait_for 'local-only' "$AF_DRIVER_TIMEOUT" 'the assistant refuses a remote target' || return 1
    local screen; screen="$(af_capture)"
    if ! printf '%s\n' "$screen" | grep -q "$REMOTE_URL"; then
        _af_fail '#3708: the assistant refusal does not name the daemon it declined to administer:'
        printf '%s\n' "$screen" >&2; return 1
    fi
    _af_log 'assert OK: the config assistant refuses a remote target and names it'

    unset AF_DRIVER_LAUNCH_ENV
    stop_remote_daemon
}

# drive_local_target_unchanged is the control leg, and it is the one that would
# catch a fix that routed everything unconditionally. With no target set the pane
# must behave exactly as it always did: this machine's values, this machine's
# bare path, no daemon URL anywhere.
drive_local_target_unchanged() {
    export AF_DRIVER_COLS=120 AF_DRIVER_ROWS=40
    af_reset_sandbox
    seed_local_config

    unset AF_DRIVER_LAUNCH_ENV
    af_boot || return 1
    open_config_editor || return 1

    local screen; screen="$(af_capture)"
    if ! printf '%s\n' "$screen" | grep -q "$LOCAL_TOML"; then
        _af_fail "the local pane does not name this machine's config path ($LOCAL_TOML):"
        printf '%s\n' "$screen" >&2; return 1
    fi
    if printf '%s\n' "$screen" | grep -q ' on http://'; then
        _af_fail 'a local session must not name a daemon URL in the header:'
        printf '%s\n' "$screen" >&2; return 1
    fi
    if ! printf '%s\n' "$screen" | grep -q "$LOCAL_AGENT"; then
        _af_fail "the local pane does not show this machine's default_program ($LOCAL_AGENT):"
        printf '%s\n' "$screen" >&2; return 1
    fi
    _af_log 'assert OK: with no target the pane is unchanged — local values, bare local path'

    edit_selected_key "$EDITED_AGENT" || return 1
    assert_file_has "$LOCAL_TOML" "default_program = '$EDITED_AGENT'" \
        "a local edit still writes this machine's config" || return 1

    af_send Escape
}

rc=0
drive_remote_target || rc=1
drive_config_assistant_is_refused || rc=1
drive_local_target_unchanged || rc=1

if [ "$rc" -eq 0 ]; then
    echo "[tui-driver] #3708 SCENARIO PASSED (3/3 legs)"
else
    echo "[tui-driver] #3708 SCENARIO FAILED"
fi
exit "$rc"
