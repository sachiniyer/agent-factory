#!/usr/bin/env bash
# Real-TUI acceptance for #3302, via:
#
#   scripts/testbox.sh scenario scripts/tui-3302-scenario.sh
#
# #3302 fixed CheckAndHandleTrustPrompt reporting a found-but-undismissed
# trust dialog as "no dialog", which let the create path type the user's
# prompt into the visible dialog. The failure injection (send-keys refused,
# unreadable pane) is pinned by unit tests in session/tmux; what a real drive
# must prove is that the changed loop semantics still carry the LIVE flows:
#
#   1. A create whose agent raises the claude folder-trust dialog still gets
#      the dialog dismissed and reaches ready — the true-return no longer
#      being reachable-only-on-success must not wedge or fail the loop.
#   2. The dialog is answered with a bare Enter, and the mission text lands
#      in the composer that appears AFTER dismissal — never in the dialog.
#      The fake agent is the oracle: it logs the exact line that answered its
#      dialog phase and every line its composer phase received, so a prompt
#      typed into the dialog is a hard, unfakeable assertion failure.
#
# The fake agent is a script NAMED claude (DetectAgentFromCommand keys on
# filepath.Base, #1116), so the real ProgramClaude dismissal branch runs.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox

TRUST_LOG="$HOME/sandbox/trust-3302.log"
FAKE_BIN_DIR="$HOME/sandbox/bin"
mkdir -p "$FAKE_BIN_DIR"
rm -f "$TRUST_LOG"

# Phase 1 paints the old-wording folder-trust dialog (claudeTrustPromptPresent
# matches it verbatim) and blocks until a line answers it. Phase 2 renders a
# "❯" composer (the claude readiness glyph) and echoes every delivered line so
# the pane shows the mission after delivery.
cat > "$FAKE_BIN_DIR/claude" <<EOF
#!/usr/bin/env bash
printf 'Do you trust the files in this folder?\n'
printf '\xe2\x9d\xaf Yes  No\n'
IFS= read -r answer
printf 'dialog-answer:%s:end\n' "\$answer" >> '$TRUST_LOG'
printf '\n\xe2\x9d\xaf composer ready\n'
while IFS= read -r line; do
    printf 'received:%s:end\n' "\$line" >> '$TRUST_LOG'
    printf 'echo:%s\n' "\$line"
done
EOF
chmod +x "$FAKE_BIN_DIR/claude"

af_set_config "default_program = \"claude\"

[program_overrides]
claude = \"$FAKE_BIN_DIR/claude\""

af_boot

# --- 1. TUI-gesture create (n): dialog agent must still reach ready --------
# af_new_instance waits for the ● ready dot, which only appears once the
# daemon's create path got past WaitForReady + DismissTrustPrompt. A wedged
# or failed dismissal loop times this out.
af_new_instance dlg
af_wait_for_file_content() {
    local re="$1" timeout="${2:-$AF_DRIVER_TIMEOUT}" label="${3:-$1}"
    local deadline; deadline=$(( $(_af_now) + timeout ))
    while ! grep -qE -- "$re" "$TRUST_LOG" 2>/dev/null; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "#3302: timed out waiting for trust log: $label — log: [$(cat "$TRUST_LOG" 2>/dev/null)]"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}
af_wait_for_file_content '^dialog-answer::end$' "$AF_DRIVER_TIMEOUT" \
    'dlg dialog answered with a bare Enter'

# --- 2. Daemon create path with a prompt: the literal #3302 flow -----------
rm -f "$TRUST_LOG"
MISSION='MISSION-3302-DELIVERED'
"$(_af_resolve_bin)" sessions create m3302 --prompt "$MISSION" \
    --repo "$AF_DRIVER_REPO" >/dev/null

af_wait_for_file_content '^dialog-answer::end$' "$AF_DRIVER_TIMEOUT" \
    'm3302 dialog answered with a bare Enter'
af_wait_for_file_content "^received:.*${MISSION}.*:end$" "$AF_DRIVER_TIMEOUT" \
    'mission delivered to the composer phase'

# The dialog phase must never have seen the mission: exactly one
# dialog-answer line, and it is the bare-Enter one already matched above.
if [ "$(grep -c '^dialog-answer:' "$TRUST_LOG")" != 1 ] \
    || grep -q "^dialog-answer:.*${MISSION}" "$TRUST_LOG"; then
    _af_fail "#3302: the mission reached the trust dialog — log: [$(cat "$TRUST_LOG")]"
    exit 1
fi

# --- 3. The TUI shows the delivered mission in the agent pane --------------
af_wait_for 'm3302.*●' "$AF_DRIVER_TIMEOUT" 'm3302 ready in the tree'
af_select m3302
af_open_pane
af_wait_for "echo:${MISSION}" "$AF_DRIVER_TIMEOUT" \
    'the composer echo of the mission is visible in the agent pane'

echo "tui-3302-scenario: PASS"
