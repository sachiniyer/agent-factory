#!/usr/bin/env bash
# Regression scenario for the legacy phrase quote-injection bug:
#   scripts/testbox.sh scenario scripts/tui-3302-legacy-quote-scenario.sh
#
# Mirrors scripts/tui-3302-scenario.sh in spirit, but the fake agent here
# quotes the legacy phrase INSIDE ITS OWN TRANSCRIPT (not as a dialog),
# with the composer painted below. The fix (claudeLegacyTrustPickerIsLast)
# must refuse this pane: no Enter is injected between the agent's quote
# line and its composer.
#
# The fake agent never paints a real picker — it paints a quoted mention
# followed by its composer. After af's daemon poll, the agent must observe
# its own arbitrary "echo:" line that it was set up to print AFTER the
# quote — meaning no Enter was injected into the composer (an injected
# Enter would deliver a blank line / commit nothing, which the fake agent
# logs as 'received::end').
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox

TRUST_LOG="$HOME/sandbox/trust-3302-legacy-quote.log"
FAKE_BIN_DIR="$HOME/sandbox/bin"
mkdir -p "$FAKE_BIN_DIR"
rm -f "$TRUST_LOG"

# Phase 1 paints the real legacy dialog once (af dismisses it with bare Enter).
# Phase 2 quotes the legacy phrase as ordinary output and then renders a
# "❯" composer that echoes every line it receives. A blank "received:" line
# in the log means af injected an Enter into the composer phase — the bug.
# Uses <<EOF (unquoted) so $TRUST_LOG is expanded by the OUTER shell into
# the script body, while \$answer / \$line / \$line are passed through to
# the inner script verbatim. Same pattern as scripts/tui-3302-scenario.sh.
cat > "$FAKE_BIN_DIR/claude" <<EOF
#!/usr/bin/env bash
# Phase 1: paint the real legacy dialog; block for the dismissal Enter.
printf 'Do you trust the files in this folder?\n'
printf '\xe2\x9d\xaf Yes  No\n'
IFS= read -r answer
printf 'dialog-answer:%s:end\n' "\$answer" >> '$TRUST_LOG'
# Phase 2: the agent is now in its composer, and its visible output QUOTES
# the legacy phrase as ordinary output before the composer cursor. This is
# the bug's repro shape: a quoted mention above a working composer.
printf 'I remember when Claude asked: "Do you trust the files in this folder?"\n'
printf 'That prompt is gone now.\n'
printf '\xe2\x9d\xaf composer ready\n'
# The composer echoes every line it receives so the test can see injected keys.
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

# Create the instance: af must dismiss the real legacy dialog with Enter
# (dialog-answer recorded), then the agent moves into its composer phase,
# quotes the legacy phrase in its own output, and renders the composer.
af_new_instance dlgq

af_wait_for_file_content() {
    local re="$1" timeout="${2:-$AF_DRIVER_TIMEOUT}" label="${3:-$1}"
    local deadline; deadline=$(( $(_af_now) + timeout ))
    while ! grep -qE -- "$re" "$TRUST_LOG" 2>/dev/null; do
        if [ "$(_af_now)" -ge "$deadline" ]; then
            _af_fail "#3302-legacy-quote: timed out waiting for: $label — log: [$(cat "$TRUST_LOG" 2>/dev/null)]"
            return 1
        fi
        sleep "$AF_DRIVER_POLL"
    done
}

# Wait for the dismissal Enter to land.
af_wait_for_file_content '^dialog-answer::end$' "$AF_DRIVER_TIMEOUT" \
    'dlgq legacy dialog answered with a bare Enter'

# Now the agent is in its composer phase. Its visible pane quotes the
# legacy phrase above the composer — exactly the bug-report shape. af's
# daemon poll runs `CheckAndHandleTrustPrompt` against this pane: BEFORE
# the fix, this injected Enter; AFTER the fix, nothing.
#
# Give af's poll a few seconds to potentially mis-fire. If it fires, the
# composer echoes the empty line: a `^received::end$` line appears in
# the log. If the fix works, NO such line appears during this idle window.
sleep "${AF_DRIVER_TIMEOUT:-10}"

if grep -qE '^received::end$' "$TRUST_LOG" 2>/dev/null; then
    _af_fail "#3302-legacy-quote: af injected Enter into the composer that quotes the legacy phrase — log: [$(cat "$TRUST_LOG")]"
    exit 1
fi

echo "tui-3302-legacy-quote-scenario: PASS"
