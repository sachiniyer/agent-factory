package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Which reading of the remnant reader (#7618 item 2) each real capture takes.
// The gate-level tests in claude_launch_dialog_remnant_test.go pin the hold;
// these pin the reason, so a capture held for another reason is noticed.
func TestClaudeLaunchDialogRemnantOfReadsEachRealCapture(t *testing.T) {
	for _, name := range append(append([]string{}, remnantNarrowFixtures...), remnantShortFixtures...) {
		require.Equal(t, claudeLaunchDialogRemnantUnderGuide,
			claudeLaunchDialogRemnantOf(claudeScreenOf(readTrustFixture(t, name))), name)
	}
	for _, name := range remnantFooterTailFixtures {
		require.Equal(t, claudeLaunchDialogRemnantFooterTail,
			claudeLaunchDialogRemnantOf(claudeScreenOf(readTrustFixture(t, name))), name)
	}
	for _, name := range []string{
		"claude_2_0_25_legacy_folder_trust_dialog.ansi",
		"claude_2_1_19_legacy_folder_trust_dialog.ansi",
		"claude_2_1_328_folder_trust_dialog_80x10.ansi",
	} {
		content := pendingExit(t, readTrustFixture(t, name), "Press Ctrl-C again to exit")
		require.Equal(t, claudeLaunchDialogRemnantPendingExit, claudeLaunchDialogRemnantOf(claudeScreenOf(content)), name)
	}
}

// A composer's own rows, and a dialog quoted above one, are no remnant.
func TestClaudeLaunchDialogRemnantOfReadsNoComposer(t *testing.T) {
	idle := readTrustFixture(t, "claude_2_1_328_idle_composer_80x24.ansi")
	for name, content := range map[string]string{
		"a real idle composer":                             idle,
		"a real idle composer at 10 columns":               readTrustFixture(t, "claude_2_1_328_idle_composer_10x24.ansi"),
		"Security guide above a real composer":             "  Security guide\n\n" + idle,
		"the dialog's bottom above a real composer":        "  Security guide\n\n  ❯ No, exit\n\n  Enter to confirm · Esc to cancel\n\n" + idle,
		"the exit notice in a ruled composer's status row": composerRule + "\n❯ \n" + composerRule + "\n  Press Ctrl-C again to exit",
	} {
		require.Equal(t, claudeLaunchDialogRemnantNone, claudeLaunchDialogRemnantOf(claudeScreenOf(content)), name)
	}
}
