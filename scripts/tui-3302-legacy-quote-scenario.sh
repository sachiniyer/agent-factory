#!/usr/bin/env bash
# Regression scenario for the legacy phrase quote-injection bug:
#   scripts/testbox.sh scenario scripts/tui-3302-legacy-quote-scenario.sh
#
# Mirrors scripts/tui-3302-scenario.sh in spirit, but the fake agent here
# quotes the legacy phrase INSIDE ITS OWN TRANSCRIPT (not as a dialog),
# with the composer painted below. The fix (claudeLegacyTrustDialogOf)
# must refuse this pane: no Enter is injected between the agent's quote
# line and its composer.
#
# The fake agent first paints the boxed legacy dialog, pausing half-drawn
# (af must send nothing until the whole picker is up, then one Enter), and
# then paints a quoted mention followed by a framed composer whose draft
# reads "Yes No". An Enter injected into that composer is logged as
# 'received::end'; a key sent into the half-drawn picker as 'early-input'.
set -euo pipefail

# shellcheck source=/dev/null
source /src/scripts/tui-driver.sh

export AF_DRIVER_REPO="$HOME/sandbox/mock-repo"

af_reset_sandbox

TRUST_LOG="$HOME/sandbox/trust-3302-legacy-quote.log"
FAKE_BIN_DIR="$HOME/sandbox/bin"
mkdir -p "$FAKE_BIN_DIR"
rm -f "$TRUST_LOG"

# Phase 1 paints the legacy dialog in its boxed layout (labelled options,
# "Enter to confirm" footer last) one piece at a time, and PAUSES with only
# "❯ 1. Yes, proceed" painted — the half-drawn stacked frame (#4743 Codex P1).
# Before painting "2. No, exit" it checks whether input already arrived: a key
# sent into the half-drawn picker is logged as 'early-input'. It then reads the
# dismissal; a bare Enter logs 'dialog-answer::end'.
#
# Phase 2 quotes the legacy question in prose AND as a whole row of ordinary
# output, then paints a framed composer whose draft reads "Yes No" (#4743 Codex
# P2), and echoes every line it receives. A blank "received:" line in the log
# means af injected an Enter into the composer phase — the bug.
# Uses <<EOF (unquoted) so $TRUST_LOG is expanded by the OUTER shell into
# the script body, while \$answer / \$line / \$RULE are passed through to
# the inner script verbatim. Same pattern as scripts/tui-3302-scenario.sh.
cat > "$FAKE_BIN_DIR/claude" <<EOF
#!/usr/bin/env bash
RULE='──────────────────────────────────────────────'
# Phase 1: paint the boxed legacy dialog, pausing half-drawn.
printf '╭%s╮\n' "\$RULE"
printf '│ Do you trust the files in this folder?       │\n'
printf '│                                              │\n'
printf '│ Claude Code may read files in this folder.   │\n'
printf '│                                              │\n'
printf '│ \xe2\x9d\xaf 1. Yes, proceed                            │\n'
sleep 6
if read -r -t 0; then printf 'early-input\n' >> '$TRUST_LOG'; fi
printf '│   2. No, exit                                │\n'
printf '╰%s╯\n' "\$RULE"
printf '   Enter to confirm · Esc to exit\n'
IFS= read -r answer
printf 'dialog-answer:%s:end\n' "\$answer" >> '$TRUST_LOG'
clear
# Phase 2: the agent is now in its composer, and its visible output QUOTES
# the legacy question above a framed composer whose draft reads "Yes No".
printf 'I remember when Claude asked: "Do you trust the files in this folder?"\n'
printf 'Do you trust the files in this folder?\n'
printf '╭%s╮\n' "\$RULE"
printf '│ \xe2\x9d\xaf Yes No                                     │\n'
printf '╰%s╯\n' "\$RULE"
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

# Wait for the dismissal Enter to land. The dialog sits half-drawn for 6s
# first, so allow for that on top of the usual timeout.
af_wait_for_file_content '^dialog-answer::end$' "$(( AF_DRIVER_TIMEOUT + 10 ))" \
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

if grep -qx 'early-input' "$TRUST_LOG" 2>/dev/null; then
    _af_fail "#4743: af sent a key into the half-drawn legacy picker (only '1. Yes, proceed' painted) — log: [$(cat "$TRUST_LOG")]"
    exit 1
fi

if grep -qE '^received::end$' "$TRUST_LOG" 2>/dev/null; then
    _af_fail "#3302-legacy-quote: af injected Enter into the composer that quotes the legacy phrase — log: [$(cat "$TRUST_LOG")]"
    exit 1
fi

echo "tui-3302-legacy-quote-scenario: PASS"
