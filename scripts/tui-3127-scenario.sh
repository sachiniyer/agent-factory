#!/usr/bin/env bash
# Real-daemon drive for #3127 (swap a limited session to a configured account),
# via:
#
#   scripts/testbox.sh scenario scripts/tui-3127-scenario.sh
#
# The behaviour under test is a DAEMON decision, not a TUI one: when an unpinned
# session hits a usage limit and the operator has opted in with both
# limit_auto_resume and an explicit limit_account_candidates list, the daemon
# stops every credential-bearing pane, commits a new identity durably, and brings
# the session back on that account with a fresh conversation. Three cases matter,
# and #3127 names all three: a candidate is available; every candidate is itself
# limited; the session carries an explicit --account pin.
#
# Why a driver scenario and not only unit tests: every unit test in daemon/ and
# session/ swaps a seam — a fake backend, a stubbed evidence loader, a mock tmux
# executor. None of them proves that a REAL daemon polling a REAL tmux pane
# reaches the decision at all. The ways this ships broken are all invisible to
# those tests: the limit never being detected off live pane content, the account
# boundary refusing the replacement launch it just proved, the replacement pane
# coming up on the ambient identity while the row reports the new account.
#
# THE STAND-IN IS THE POINT, not a compromise. scripts/container/configure-
# playtest-agent.sh installs a bash stand-in whose basename is not "claude", and
# the account boundary refuses to scope a command it cannot prove is a direct
# agent invocation — so that default stand-in can never be swapped. This scenario
# installs its own AS $HOME/bin/claude, which is provable, and walls every
# identity EXCEPT the swap target. That one conditional is what makes the whole
# flow observable end to end: the wall clearing is itself evidence that the
# replacement pane really came up under the selected CLAUDE_CONFIG_DIR, while on
# the two paths where af must NOT move, the wall is where the session stays and
# there is a steady state to assert rather than a four-second race.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_COLS=140 AF_DRIVER_ROWS=40
export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

# The pattern the daemon matches the stand-in's banner with. Deliberately unlike
# any real provider's wording: it must not collide with the built-in patterns,
# and a reader has to be able to tell fixture from product. The banner itself is
# written inside the stand-in heredoc below, which is quoted so the shell cannot
# expand a variable into it.
readonly LIMIT_PATTERN='PLAYTEST-QUOTA-WALL'

readonly CANDIDATE_ACCOUNT='swap-target'
readonly PINNED_ACCOUNT='pinned-identity'

cleanup() {
    af_quit >/dev/null 2>&1 || true
}
trap cleanup EXIT

# install_claude_standin writes the provable agent stand-in. It reports its own
# identity on the first line so the pane itself is evidence of which credential
# root it received, then parks with the banner only when unscoped.
install_claude_standin() {
    mkdir -p "$HOME/bin"
    cat >"$HOME/bin/claude" <<'STANDIN'
#!/usr/bin/env bash
# Play-test stand-in for #3127. Not a real agent: it exists to make the account a
# pane runs under visible, and to park every identity EXCEPT the swap target at a
# usage-limit wall.
#
# "every except one" rather than "ambient only" is what gives the pinned and
# no-candidate cases a STEADY state to assert. A successful swap clears the wall
# within a poll or two, so the limited state on that path is transient and racy;
# on the paths where af must NOT move, the wall is where the session stays.
identity="${CLAUDE_CONFIG_DIR:-}"
if [ -n "$identity" ]; then
    printf 'PLAYTEST-IDENTITY: account root %s\n' "$identity"
else
    printf 'PLAYTEST-IDENTITY: ambient\n'
fi
case "$identity" in
*/swap-target) : ;;   # the one identity with quota
*) printf 'PLAYTEST-QUOTA-WALL: this playtest identity is out of quota\n' ;;
esac
exec bash --noprofile --norc -i
STANDIN
    chmod +x "$HOME/bin/claude"
    _af_log "installed provable claude stand-in at $HOME/bin/claude"
}

# write_swap_config replaces the sandbox's play-test config with the opt-in this
# feature requires. Both keys are needed: limit_auto_resume alone must NOT rotate
# anything, which the third case below relies on.
#
# It deliberately writes NO program_overrides, and deletes the play-test config
# the sandbox shipped with, because the account boundary will not scope a program
# it cannot prove af itself chose. `trustBase` (session/program_resolution.go) is
# true only when the effective program_overrides entry is BYTE-FOR-BYTE af's own
# detected built-in, and af detects the stand-in above as
# the quoted $HOME/bin/claude followed by --dangerously-skip-permissions. Writing
# the bare path instead — the obvious thing, and what the first draft of this
# scenario did — is a different string, so the base arguments stop being declared
# and every candidate
# is refused with "could not be proven to be a direct claude invocation". Letting
# the built-in win is what makes this sandbox able to exercise the feature at all.
#
# TOML rather than JSON because af materializes config.toml on first run and reads
# it in preference afterwards: a stale program_overrides left there outranks
# anything written to config.json.
write_swap_config() {
    local candidates="$1"
    rm -f "$AGENT_FACTORY_HOME/config.json" "$AGENT_FACTORY_HOME/config.toml"
    cat >"$AGENT_FACTORY_HOME/config.toml" <<CONFIG
default_program = "claude"
limit_auto_resume = true
limit_account_candidates = [$candidates]
limit_retry_interval = "30m"
daemon_poll_interval = "2s"

[limit_patterns]
claude = "$LIMIT_PATTERN"
CONFIG
    _af_log "config: limit_auto_resume=true limit_account_candidates=[$candidates]"
}

register_accounts() {
    local bin; bin="$(_af_resolve_bin)"
    "$bin" accounts add claude "$CANDIDATE_ACCOUNT" >/dev/null
    "$bin" accounts add claude "$PINNED_ACCOUNT" >/dev/null
    _af_log "registered claude accounts: $CANDIDATE_ACCOUNT, $PINNED_ACCOUNT"
}

# create_session makes a session through the CLI rather than af_new_instance.
# The driver's helper waits for the ready dot, and every session here parks at
# the playtest wall on its first frame instead — reaching [limit] without ever
# being ready is the fixture working, not a failure to wait for.
create_session() {
    local bin; bin="$(_af_resolve_bin)"
    (cd "$AF_DRIVER_REPO" && "$bin" sessions create --name "$@") >/dev/null
}

# session_field prints one JSON field of a session row, by title. The CLI is the
# read surface on purpose: it is the daemon's own projection, so a value read
# here is the value a user would see, not a test-local reconstruction.
session_field() {
    local title="$1" field="$2" bin
    bin="$(_af_resolve_bin)"
    (cd "$AF_DRIVER_REPO" && "$bin" sessions get "$title" --json 2>/dev/null) \
        | python3 -c "
import json,sys
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
row = doc.get('data', doc)
value = row.get('$field', '')
if value is None:
    value = ''
if isinstance(value, bool):
    value = 'true' if value else 'false'
print(value)
"
}

# wait_for_session_field polls the daemon's projection until field == want.
wait_for_session_field() {
    local title="$1" field="$2" want="$3" what="$4"
    local deadline; deadline=$(( $(_af_now) + ${5:-120} ))
    local got=''
    while [ "$(_af_now)" -lt "$deadline" ]; do
        got="$(session_field "$title" "$field")"
        if [ "$got" = "$want" ]; then
            _af_log "assert OK: $what ($field=$got)"
            return 0
        fi
        sleep 2
    done
    _af_fail "$what: $field was '$got', want '$want'"
    (cd "$AF_DRIVER_REPO" && "$(_af_resolve_bin)" sessions get "$title" --json) >&2 || true
    return 1
}

# ---------------------------------------------------------------------------
# Case 1 — a candidate is available: the unpinned session moves to it.
# ---------------------------------------------------------------------------
drive_swap_to_candidate() {
    af_reset_sandbox
    install_claude_standin
    write_swap_config "\"$CANDIDATE_ACCOUNT\""
    register_accounts
    af_boot
    af_ensure_nav
    af_focus_tree

    create_session ambient-swap || return 1

    # NOT the transient limit state: the whole park-decide-replace cycle takes a
    # few seconds, so polling for liveness_name=limit-reached here is a race the
    # scenario loses more often than not. The durable facts are asserted instead,
    # and the daemon's own completion line is what proves this came from the
    # USAGE-LIMIT path rather than from an ordinary create.
    wait_for_session_field ambient-swap account "$CANDIDATE_ACCOUNT" \
        'the daemon committed the configured candidate as the new identity' 180 || return 1
    wait_for_session_field ambient-swap account_auto_selected true \
        'the move is recorded as an af choice, not an explicit pin' 60 || return 1
    if ! grep -q "auto-resumed limit-blocked session \"ambient-swap\".*on claude account \"$CANDIDATE_ACCOUNT\"" \
        "$AGENT_FACTORY_HOME/agent-factory.log"; then
        _af_fail 'the identity change did not come from the usage-limit path'
        grep -E 'ambient-swap' "$AGENT_FACTORY_HOME/agent-factory.log" | tail -20 >&2
        return 1
    fi
    _af_log 'assert OK: the daemon logged the move as a limit-blocked auto-resume'

    af_select ambient-swap || return 1
    af_open_pane || return 1
    # #3127's fourth requirement, in the place it has to be true: the pane itself.
    af_wait_for 'switched from' "$AF_DRIVER_TIMEOUT" \
        'the session says which identity changed, in the session' || return 1

    # And the replacement really is running on the account. This asks the LIVE
    # process for its credential root rather than matching the stand-in's startup
    # line, which is both stronger — a startup line proves what the process was
    # told at exec, an echo proves what it still has — and necessary: the notice
    # above is long enough to push that line out of the visible pane.
    af_enter_interactive || return 1
    af_send_to_pane 'printf "PLAYTEST-ENV: %s\\n" "${CLAUDE_CONFIG_DIR:-none}"'
    af_wait_for "PLAYTEST-ENV: .*accounts/claude/$CANDIDATE_ACCOUNT" "$AF_DRIVER_TIMEOUT" \
        'the replacement pane is running with the selected account root' || return 1
    af_exit_interactive || return 1
    af_hide_pane || return 1
    _af_log 'assert OK: case 1 — an unpinned limited session moved to its configured candidate'
}

# ---------------------------------------------------------------------------
# Case 2 — an explicit --account pin is never overridden.
# ---------------------------------------------------------------------------
drive_pinned_session_is_not_swapped() {
    create_session pinned-swap --account "$PINNED_ACCOUNT" || return 1
    wait_for_session_field pinned-swap liveness_name limit-reached \
        'the pinned session parked at its own wall' 180 || return 1
    # Hold past several poll intervals: the assertion is that nothing happens,
    # so it has to be given the chance to happen.
    sleep 30
    wait_for_session_field pinned-swap account "$PINNED_ACCOUNT" \
        'an explicitly pinned account is never overridden' 10 || return 1
    wait_for_session_field pinned-swap account_auto_selected '' \
        'the pin is still a pin, not an af choice' 10 || return 1
    _af_log 'assert OK: case 2 — an explicit --account pin survived a usage limit'
}

# ---------------------------------------------------------------------------
# Case 3 — the default is off: without the candidate list, nothing rotates.
# ---------------------------------------------------------------------------
drive_optout_keeps_waiting() {
    af_reset_sandbox
    install_claude_standin
    # limit_auto_resume ON, candidate list EMPTY: the second half of the opt-in
    # is missing, so the documented default ("af never rotates accounts on its
    # own") must still hold.
    write_swap_config ''
    register_accounts
    af_boot
    af_ensure_nav
    af_focus_tree

    create_session no-candidates || return 1
    wait_for_session_field no-candidates liveness_name limit-reached \
        'the session parked with no candidate list configured' 120 || return 1
    sleep 30
    wait_for_session_field no-candidates account '' \
        'with no candidate list af kept waiting on the ambient identity' 10 || return 1
    _af_log 'assert OK: case 3 — limit_auto_resume alone rotates nothing'
}

drive_swap_to_candidate || exit 1
drive_pinned_session_is_not_swapped || exit 1
drive_optout_keeps_waiting || exit 1

af_assert_no_orphan_clients || exit 1
echo 'PASS: #3127 a limited session swaps to a configured account, and only then'
