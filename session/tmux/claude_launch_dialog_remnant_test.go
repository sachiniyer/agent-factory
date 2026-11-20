package tmux

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7618 item 2: a pane that shows any part of a
// folder-trust dialog never reads clear. Each capture below is a settled real
// one, taken with af's own capture flags and no key sent, and each read clear
// before the remnant reader.

// remnantNarrowFixtures are settled real dialogs on panes of 4 to 15 columns,
// 60 rows tall. Claude Code overprints and cuts the option rows there, so no
// reader found an option label above the footer.
var remnantNarrowFixtures = []string{
	"claude_2_0_25_reworded_arm_folder_trust_dialog_12_columns.ansi",
	"claude_2_0_50_reworded_arm_folder_trust_dialog_10_columns.ansi",
	"claude_2_1_19_reworded_arm_folder_trust_dialog_4_columns.ansi",
	"claude_2_1_19_reworded_arm_folder_trust_dialog_10_columns.ansi",
	"claude_2_1_20_folder_trust_dialog_9_columns.ansi",
	"claude_2_1_113_folder_trust_dialog_10_columns.ansi",
	"claude_2_1_200_folder_trust_dialog_10_columns.ansi",
	"claude_2_1_200_folder_trust_dialog_configured_15_columns.ansi",
	"claude_2_1_323_folder_trust_dialog_8_columns.ansi",
	"claude_2_1_327_folder_trust_dialog_4_columns.ansi",
	"claude_2_1_328_folder_trust_dialog_4_columns.ansi",
	"claude_2_1_329_folder_trust_dialog_4_columns.ansi",
}

// remnantShortFixtures are settled real 2.1.328 dialogs on panes of 6 to 9
// rows, where it paints the cursor's "No, exit" row alone under its guide row.
var remnantShortFixtures = []string{
	"claude_2_1_328_folder_trust_dialog_80x6.ansi",
	"claude_2_1_328_folder_trust_dialog_80x9.ansi",
	"claude_2_1_328_folder_trust_dialog_40x7.ansi",
}

// remnantFooterTailFixtures are settled real dialogs on panes too small for
// anything but the end of the footer.
var remnantFooterTailFixtures = []string{
	"claude_2_1_328_folder_trust_dialog_10x4.ansi",
	"claude_2_1_200_folder_trust_dialog_14x3.ansi",
}

func requireRemnantHold(t *testing.T, name, content string) {
	t.Helper()
	handled, cmds := pollStaticPane(t, content, 4)
	require.True(t, handled, "%s: the pane shows part of a folder-trust dialog and read clear (#7618):\n%s", name, visibleTextOf(content))
	require.Empty(t, sentKeystrokes(cmds), "%s: a hold sends no key; got %v", name, cmds)
}

func requireRemnantClear(t *testing.T, name, content string) {
	t.Helper()
	handled, cmds := pollStaticPane(t, content, 4)
	require.False(t, handled, "%s: no dialog is on this pane, and a hold stalls the create or flags the session:\n%s", name, visibleTextOf(content))
	require.Empty(t, sentKeystrokes(cmds), "%s", name)
}

func TestIssue7618NarrowPaneDialogNeverReadsClear(t *testing.T) {
	for _, name := range remnantNarrowFixtures {
		content := readTrustFixture(t, name)
		requireRemnantHold(t, name, content)
	}
}

func TestIssue7618ShortPaneDialogNeverReadsClear(t *testing.T) {
	for _, name := range remnantShortFixtures {
		content := readTrustFixture(t, name)
		requireRemnantHold(t, name, content)
	}
	// A pane whose top cuts the guide row: its last word, then the option
	// rows as 12 columns leave them, then the footer.
	requireRemnantHold(t, "a cut guide row",
		" guide\n\n  1.Yes, I\n ❯  trust th\n    folder\n  2. No, exi\n\n Enter to\n confirm ·\n Esc to\n cancel")
}

func TestIssue7618FooterTailNeverReadsClear(t *testing.T) {
	for _, name := range remnantFooterTailFixtures {
		content := readTrustFixture(t, name)
		requireRemnantHold(t, name, content)
	}
	for _, content := range []string{
		" Esc to\n cancel",
		" · Esc\n to\n cancel",
		" confirm · Esc to exit",
		" rm ·\n Esc\n to\n can\n cel",
	} {
		requireRemnantHold(t, "footer tail", content)
	}
}

// pendingExit is a settled capture with its last painted row, the footer,
// reading the notice Claude Code shows there after one exit key press.
func pendingExit(t *testing.T, capture, notice string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(string(visibleTextOf(lines[last]))) == "" {
		last--
	}
	require.GreaterOrEqual(t, last, 0)
	require.True(t, claudeTrustFooterWhole(claudeTrustRowOf(string(visibleTextOf(lines[last]))).label),
		"the capture's last row is its whole footer: %q", lines[last])
	lines[last] = "   " + notice
	return strings.Join(lines, "\n") + "\n"
}

func TestIssue7618PendingExitDialogNeverReadsClear(t *testing.T) {
	for _, name := range []string{
		"claude_2_0_25_legacy_folder_trust_dialog.ansi",
		"claude_2_0_50_legacy_folder_trust_dialog.ansi",
		"claude_2_1_19_legacy_folder_trust_dialog.ansi",
		"claude_2_1_328_folder_trust_dialog_80x10.ansi",
	} {
		for _, notice := range []string{"Press Ctrl-C again to exit", "Press Ctrl-D again to exit"} {
			content := pendingExit(t, readTrustFixture(t, name), notice)
			requireRemnantHold(t, name+" with "+notice, content)
		}
	}
	// The notice wrapped by a narrow pane, over the legacy option rows.
	requireRemnantHold(t, "wrapped notice",
		"Learn more\n\n❯ 1. Yes,\n  proceed\n  2. No, exit\n\nPress Ctrl-C\nagain to exit")
}

// The risk of this reader is a hold on a pane that shows no dialog: it stalls
// a create and flags a live session. None of these shows one.
func TestIssue7618RemnantReaderLeavesNonDialogPanesClear(t *testing.T) {
	composer := readTrustFixture(t, "claude_2_1_322_composer_after_mcp_decline.ansi")
	idle := readTrustFixture(t, "claude_2_1_328_idle_composer_80x24.ansi")
	narrowIdle := readTrustFixture(t, "claude_2_1_328_idle_composer_10x24.ansi")
	quoted := "● The dialog ends like this:\n\n  Security guide\n\n  ❯ 1. Yes, I trust this folder\n    2. No, exit\n\n  Enter to confirm · Esc to cancel\n\n"
	narrowQuoted := "● It reads:\n\n Security\n guide\n\n  1.Yes, I\n ❯  trust th\n    folder\n  2. No, exi\n\n Enter to\n confirm ·\n Esc to\n cancel\n\n"
	status := "  ? for shortcuts"
	ruled := composerRule + "\n❯ \n" + composerRule + "\n"
	boxed := "╭────────────────╮\n│ > Try \"fix it\" │\n╰────────────────╯\n"
	for _, tt := range []struct{ name, content string }{
		{"a real idle composer", idle},
		{"a real idle composer at 10 columns", narrowIdle},
		{"a real composer after a declined MCP dialog", composer},
		{"a real 2.1.329 composer showing the exit notice", readTrustFixture(t, "claude_2_1_329_idle_composer_exit_notice_80x24.ansi")},
		{"a real 2.1.329 composer showing the exit notice at 12 columns", readTrustFixture(t, "claude_2_1_329_idle_composer_exit_notice_12x24.ansi")},
		{"a real 2.0.50 composer showing the exit notice", readTrustFixture(t, "claude_2_0_50_idle_composer_exit_notice_80x24.ansi")},
		{"the dialog's bottom quoted above a real composer showing the exit notice",
			quoted + readTrustFixture(t, "claude_2_1_329_idle_composer_exit_notice_80x24.ansi")},
		{"Security guide in scrollback above a real composer", "● See the Security guide\n\n  Security guide\n\n" + idle},
		{"Learn more in scrollback above a real composer", "  Learn more\n\n" + idle},
		{"the dialog's bottom quoted above a real composer", quoted + idle},
		{"a narrow dialog's bottom quoted above a real composer", narrowQuoted + narrowIdle},
		{"the dialog's bottom quoted above a ruled composer", quoted + ruled + status},
		{"the dialog's bottom quoted above a boxed composer", quoted + boxed + status},
		{"a ruled composer with the exit notice in its status row", ruled + "  Press Ctrl-C again to exit"},
		{"a boxed composer with the exit notice in its status row", boxed + "  Press Ctrl-C again to exit"},
		{"a quoted legacy dialog above a ruled composer with the exit notice",
			"Do you trust the files in this folder?\n\n❯ 1. Yes, proceed\n  2. No, exit\n\n" + ruled + "  Press Ctrl-C again to exit"},
		{"a quoted reworded bottom above a boxed composer with the exit notice", quoted + boxed + "  Press Ctrl-D again to exit"},
		{"the exit notice alone", "Press Ctrl-C again to exit"},
		{"the exit notice under transcript", "● Done.\n\nPress Ctrl-C again to exit"},
		{"the footer's last two words", " to\n cancel"},
		{"the footer's last word", " cancel"},
		{"a cut word of the footer", " el"},
		{"the footer cut inside its third word from the end", " sc\n to\n can\n cel"},
		{"Esc to cancel under a prompt row", "❯ Esc to cancel"},
		{"Esc to cancel between a composer's rules", composerRule + "\nEsc to cancel\n" + composerRule},
		{"the footer's end in a boxed draft whose prompt row scrolled off", "│   Esc to       │\n│   cancel       │\n╰────────────────╯"},
		{"a guide row, an option row and the footer in a boxed draft whose prompt row scrolled off",
			"│   Security guide                     │\n│   ❯ No, exit                         │\n│   Enter to confirm · Esc to cancel   │\n╰──────────────────────────────────────╯"},
		{"the footer's end in a boxed draft, closed, over a second edge", "│ Esc to │\n│ cancel │\n╰────────╯\n╰────────╯"},
		{"a guide row and footer with nothing between", "Security guide\n\nEnter to confirm · Esc to cancel"},
		{"a guide row, other text, and the footer", "Security guide\n\nSee the docs\n\nEnter to confirm · Esc to cancel"},
		{"a guide row, an option remnant, and a row below the footer", "Security guide\n\n❯ No, exit\n\nEnter to confirm · Esc to cancel\n? for shortcuts"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requireRemnantClear(t, tt.name, tt.content)
		})
	}
}

func TestClaudeDialogHoldKindNamesARemnant(t *testing.T) {
	require.Equal(t, DialogHoldFolderUnreadable,
		claudeDialogHoldKind(claudeScreenOf(readTrustFixture(t, "claude_2_1_328_folder_trust_dialog_80x6.ansi"))))
	require.Equal(t, DialogHoldFolderPartial, claudeDialogHoldKind(claudeScreenOf(
		pendingExit(t, readTrustFixture(t, "claude_2_1_19_legacy_folder_trust_dialog.ansi"), "Press Ctrl-C again to exit"))))
}

// #7663 item 3: the notice names the folder-trust dialog only when the pane
// proves it is that one. The MCP server dialog's footer is the reworded
// folder-trust footer word for word, so the end of it names no dialog.
func TestIssue7663RemnantNamesOnlyWhatThePaneProves(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          DialogHoldKind
	}{
		{"the end of the footer two dialogs share", " · Esc to\n cancel", DialogHoldOther},
		{"a real 2.1.328 pane of that footer's end", readTrustFixture(t, "claude_2_1_328_folder_trust_dialog_10x4.ansi"), DialogHoldOther},
		{"a real 2.1.200 pane of that footer's end", readTrustFixture(t, "claude_2_1_200_folder_trust_dialog_14x3.ansi"), DialogHoldOther},
		{"the end of the footer only the folder dialog paints", " · Esc to\n exit", DialogHoldFolderUnreadable},
		{"option characters over the shared footer, no guide row", " N\n Y\n Enter to confirm · Esc to cancel", DialogHoldOther},
		{"option characters over the folder dialog's own footer", " 2. No, exi\n Enter to confirm · Esc to exit", DialogHoldFolderUnreadable},
		{"a guide row above the option remnant", "Security guide\n\n❯ No, exit\n\nEnter to confirm · Esc to cancel", DialogHoldFolderUnreadable},
		{"a cut guide row above the option remnant",
			" guide\n\n  1.Yes, I\n ❯  trust th\n    folder\n  2. No, exi\n\n Enter to\n confirm ·\n Esc to\n cancel", DialogHoldFolderUnreadable},
		{"option characters over an exit notice, no guide row", " 2. No, exi\n\n Press Ctrl-C again to exit", DialogHoldOther},
		{"a guide row and option remnant over an exit notice", "Security guide\n\n 2. No, exi\n\n Press Ctrl-C again to exit", DialogHoldFolderPartial},
		{"a legacy dialog over an exit notice",
			pendingExit(t, readTrustFixture(t, "claude_2_1_19_legacy_folder_trust_dialog.ansi"), "Press Ctrl-C again to exit"), DialogHoldFolderPartial},
	} {
		requireRemnantHold(t, tt.name, tt.content)
		require.Equal(t, tt.want, claudeDialogHoldKind(claudeScreenOf(tt.content)), tt.name)
	}
}

// #7663 item 2: a box's bottom edge excuses a footer only when the footer's
// own rows are inside that box. A lone edge under a remnant is not a draft.
func TestIssue7663LoneBoxEdgeUnderARemnantStillHolds(t *testing.T) {
	narrow := strings.TrimRight(readTrustFixture(t, "claude_2_1_113_folder_trust_dialog_10_columns.ansi"), "\n")
	for name, content := range map[string]string{
		"a guide row, option row and footer over a lone edge":  "Security guide\n❯ No, exit\nEnter to confirm · Esc to cancel\n╰──────╯",
		"the footer's end over a lone edge":                    " Esc\n to\n can\n cel\n╰───╯",
		"a real 2.1.113 dialog at 10 columns over a lone edge": narrow + "\n╰────────╯",
		"a footer whose last row alone carries box sides":      " · Esc to\n│ cancel │\n╰────────╯",
	} {
		requireRemnantHold(t, name, content)
	}
}

// #7663 item 1 bounds the pending-exit reading by the longest notice it
// accepts. These pin the bound's two sides: the notice wrapped a character a
// row still reads, and a key longer than the bound, or rows above the notice
// with no blank between, do not.
func TestIssue7663PendingExitReadsNoFartherThanANotice(t *testing.T) {
	dialog := "Learn more\n\n❯ 1. Yes, proceed\n  2. No, exit\n\n"
	requireRemnantHold(t, "the notice a character a row", dialog+strings.Join(strings.Split("PressCtrl-Cagaintoexit", ""), "\n"))
	requireRemnantHold(t, "the longest key", dialog+"Press Ctrl-Shift-X again to exit")
	requireRemnantClear(t, "a key past the bound", dialog+"Press Ctrl-Shift-C1 again to exit")
	rows := strings.Repeat("x\n", 4000)
	requireRemnantClear(t, "unbroken rows above the notice", rows+"Press Ctrl-C again to exit")
	requireRemnantClear(t, "unbroken rows and no notice", rows+"x")
}
