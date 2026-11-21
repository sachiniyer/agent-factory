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

// pendingExitPicker is pendingExit for the multi-server picker, whose footer
// ("Space to select · Esc to reject all") is not a folder-trust footer, so
// pendingExit's guard would not pass. It checks the last content row is the
// picker's footer instead, then reads the notice there exactly as pendingExit
// does.
func pendingExitPicker(t *testing.T, capture, notice string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(string(visibleTextOf(lines[last]))) == "" {
		last--
	}
	require.GreaterOrEqual(t, last, 0)
	require.Equal(t, claudeMCPServersFooter, claudeTrustRowOf(string(visibleTextOf(lines[last]))).label,
		"the capture's last row is the picker's whole footer: %q", lines[last])
	lines[last] = "   " + notice
	return strings.Join(lines, "\n") + "\n"
}

func TestIssue7618PendingExitDialogNeverReadsClear(t *testing.T) {
	for _, name := range []string{
		"claude_2_0_25_legacy_folder_trust_dialog.ansi",
		"claude_2_0_50_legacy_folder_trust_dialog.ansi",
		"claude_2_1_19_legacy_folder_trust_dialog.ansi",
		"claude_2_1_328_folder_trust_dialog_80x10.ansi",
		// The 2.1.322 MCP-server dialog's footer is the reworded folder-trust
		// footer word for word (claudeMCPServerFooter == claudeFolderTrustFooter),
		// so the same footer-swap that holds the folder-trust dialogs behind
		// the notice holds this one. The cursor sits on the decline row on the
		// default capture and on "Use this and all future MCP servers in this
		// project" on the after-one-Up one; both grant no permission to af,
		// and both must hold, neither read clear (#7663 gap).
		"claude_2_1_322_mcp_server_dialog.ansi",
		"claude_2_1_322_mcp_server_dialog_cursor_on_all_future.ansi",
		"claude_2_1_322_mcp_server_dialog_wide.ansi",
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

// TestIssue7618PendingExitMCPDialogNeverReadsClear covers the 2.1.322 MCP
// dialogs whose footers the folder-trust swap loop does not substitute: the
// multi-server picker, whose footer (claudeMCPServersFooter) is its own. One
// exit key pressed while the picker is up replaces its footer row with the
// notice; before this fix no clause in the gate held that pane, so
// CheckAndHandleTrustPrompt returned false and af delivered the user's prompt
// and pressed Enter past the gate into the live modal (#7652's gap, narrowed
// by #7663). af must hold either dialog and send nothing, the folder-trust
// dialogs' identical footer-swap having already proven the mechanism.
// Per #7663 the hold is unnamed (DialogHoldOther): the pane proves an MCP
// dialog, not the folder-trust one.
func TestIssue7618PendingExitMCPDialogNeverReadsClear(t *testing.T) {
	for _, name := range []string{
		"claude_2_1_322_mcp_multi_server_dialog.ansi",
		"claude_2_1_322_mcp_multi_server_dialog_three.ansi",
		"claude_2_1_322_mcp_multi_server_dialog_three_wide.ansi",
		// #7725: the picker behind the notice holds wherever a reader holds
		// it behind its own footer — the scrolled six-server window, a pane
		// narrow enough to wrap the header (the complete reading's 44-column
		// floor), and a server name long enough to wrap.
		"claude_2_1_322_mcp_multi_server_dialog_six.ansi",
		"claude_2_1_322_mcp_multi_server_dialog_six_scrolled.ansi",
		"claude_2_1_322_mcp_multi_server_dialog_40_columns.ansi",
		"claude_2_1_322_mcp_multi_server_dialog_long_name.ansi",
	} {
		for _, notice := range []string{"Press Ctrl-C again to exit", "Press Ctrl-D again to exit"} {
			content := pendingExitPicker(t, readTrustFixture(t, name), notice)
			requireRemnantHold(t, name+" with "+notice, content)
			require.Equal(t, DialogHoldOther, claudeDialogHoldKind(claudeScreenOf(content)),
				"%s with %s: the picker behind the notice is not the folder-trust dialog", name, notice)
		}
	}
	// The single-server dialog with the notice is named not at all either:
	// the rewrap matcher proves the MCP-server dialog, not the folder-trust
	// one, so af holds it as DialogHoldOther (#7663 policy).
	for _, name := range []string{
		"claude_2_1_322_mcp_server_dialog.ansi",
		"claude_2_1_322_mcp_server_dialog_cursor_on_all_future.ansi",
	} {
		content := pendingExit(t, readTrustFixture(t, name), "Press Ctrl-C again to exit")
		requireRemnantHold(t, name+" with the notice", content)
		require.Equal(t, DialogHoldOther, claudeDialogHoldKind(claudeScreenOf(content)),
			"%s with the notice: the MCP-server dialog is not the folder-trust one", name)
	}
}

// cropToContentRows trims capture to its last n content-bearing lines: a pane
// too short for the whole dialog, with its head scrolled off the top.
func cropToContentRows(t *testing.T, capture string, n int) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), n)
	return strings.Join(lines[len(lines)-n:], "\n")
}

// #7725 item 1: the picker behind the exit notice holds in every shape a
// reader holds the picker behind its own footer. The complete grammar only
// reads a whole picker at least 44 columns wide with its header on screen and
// no composer above it; the tail reader (#7463) holds it when the header has
// scrolled off or the footer wraps, and the unreadable and painting readers
// hold a wrapped picker and one under a stale composer (#7504).
func TestIssue7725PendingExitPickerEveryShape(t *testing.T) {
	two := readTrustFixture(t, "claude_2_1_322_mcp_multi_server_dialog.ansi")
	longName := readTrustFixture(t, "claude_2_1_322_mcp_multi_server_dialog_long_name.ansi")
	for _, notice := range []string{"Press Ctrl-C again to exit", "Press Ctrl-D again to exit"} {
		for _, tt := range []struct {
			name, content string
		}{
			// The two-server capture cropped so the header is off screen.
			{"the two-server capture's last 9 rows, header off", cropToContentRows(t, two, 9)},
			{"the two-server capture's last 3 rows, header off", cropToContentRows(t, two, 3)},
			{"a footer and the submit row alone", cropToContentRows(t, two, 2)},
			// The wrapped long server name with the header off the top.
			{"the long-name capture's last 4 rows", cropToContentRows(t, longName, 4)},
		} {
			content := pendingExitPicker(t, tt.content, notice)
			requireRemnantHold(t, tt.name+" with "+notice, content)
			require.Equal(t, DialogHoldOther, claudeDialogHoldKind(claudeScreenOf(content)),
				"%s with %s: the picker behind the notice is not the folder-trust dialog", tt.name, notice)
		}
	}
	// A picker under the ruled composer an earlier Claude left above it (the
	// #7504 shape). The composer's ❯ is above the picker's header, so the
	// grammar and the unreadable hold both decline the pane; its own rows
	// still prove it.
	stale := composerRule + "\n❯ \n" + composerRule + "\n  ? for shortcuts\n\n"
	content := stale + pendingExitPicker(t, two, "Press Ctrl-C again to exit")
	requireRemnantHold(t, "the picker under a stale composer", content)
	require.Equal(t, DialogHoldOther, claudeDialogHoldKind(claudeScreenOf(content)),
		"the picker under a stale composer: the picker behind the notice is not the folder-trust dialog")
}

// #7725 item 2: a last row that is a prefix of the exit notice — the notice
// still being painted where the footer was — under a dialog whose footer is
// already gone holds like the footer-erased frame. The swap proves the rows
// above with a footer in the notice's place, the same proof the whole notice
// gets, so the prefix holds where the whole notice does.
func TestIssue7725PendingExitNoticePrefixHolds(t *testing.T) {
	for _, prefix := range []string{"P", "Press", "Press Ctrl-C", "Press Ctrl-C ag", "Press Ctrl-C again to exi"} {
		content := pendingExit(t, readTrustFixture(t, "claude_2_1_322_mcp_server_dialog.ansi"), prefix)
		requireRemnantHold(t, "the MCP-server dialog under "+prefix, content)
		require.Equal(t, DialogHoldOther, claudeDialogHoldKind(claudeScreenOf(content)),
			"the MCP-server dialog under %s", prefix)
	}
	// The picker's own footer is substituted for the notice's start too.
	for _, prefix := range []string{"Press Ctrl-C ag", "Press Ctrl-D ag"} {
		content := pendingExitPicker(t, readTrustFixture(t, "claude_2_1_322_mcp_multi_server_dialog.ansi"), prefix)
		requireRemnantHold(t, "the picker under "+prefix, content)
		require.Equal(t, DialogHoldOther, claudeDialogHoldKind(claudeScreenOf(content)),
			"the picker under %s", prefix)
	}
	// The notice's start wrapped onto two rows.
	requireRemnantHold(t, "a wrapped notice prefix",
		"Learn more\n\n❯ 1. Yes,\n  proceed\n  2. No, exit\n\nPress Ctrl-C\nagain to ex")
}

// #7730 review, R1: Claude Code paints top-down, so the exit notice lands on
// the footer's first row before the footer's own tail under it is repainted.
// Once a launch dialog's body is proven above the notice — whole or a notice
// prefix — the pane holds while the trailing rows read as a suffix of that
// dialog's own footer, the same footer-swap proof as the notice-last frames.
func TestIssue7725PendingExitNoticeOverFooterTailHolds(t *testing.T) {
	// A real capture with the notice where the footer was and the footer's
	// own tail appended below it, the frame a wrapped footer leaves while
	// the notice is painted over its first row.
	for _, tt := range []struct {
		name, capture, tail string
		want                DialogHoldKind
	}{
		{"the MCP-server dialog under the shared footer's tail", "claude_2_1_322_mcp_server_dialog.ansi", " to cancel", DialogHoldOther},
		{"the picker under its footer's tail", "claude_2_1_322_mcp_multi_server_dialog.ansi", " all", DialogHoldOther},
		{"the legacy dialog under its own footer's tail", "claude_2_1_19_legacy_folder_trust_dialog.ansi", " to exit", DialogHoldFolderPartial},
	} {
		var content string
		if tt.tail == " all" {
			content = pendingExitPicker(t, readTrustFixture(t, tt.capture), "Press Ctrl-C again to exit") + tt.tail
		} else {
			content = pendingExit(t, readTrustFixture(t, tt.capture), "Press Ctrl-C again to exit") + tt.tail
		}
		requireRemnantHold(t, tt.name, content)
		require.Equal(t, tt.want, claudeDialogHoldKind(claudeScreenOf(content)),
			"%s: the dialog behind the notice is named only by what the pane proves", tt.name)
	}
	// The single-server dialog at 28 columns, the notice over the footer's
	// first row with "to cancel" still painted below it.
	requireRemnantHold(t, "a narrow single-server dialog with its footer's tail below the notice",
		"\n────────────────────────────\n"+
			"  New MCP server found in\n  this project: probe\n\n"+
			"  MCP servers may execute\n  code or access system\n  resources. All tool\n  calls require approval.\n  Learn more in the MCP\n  documentation.\n\n"+
			"  ❯ Use this MCP server\n    Use this and all future\n    MCP servers in this\n    project\n    Continue without using\n    this MCP server\n\n"+
			"  Press Ctrl-C again to exit\n  to cancel")
	// The picker at 32 columns, the notice over the footer's first row with
	// "all" still painted below it.
	requireRemnantHold(t, "a narrow picker with its footer's tail below the notice",
		"────────────────────────────────\n"+
			"  3 new MCP servers found in\n  this project\n  Select any you wish to enable.\n\n"+
			"  MCP servers may execute code\n  or access system resources.\n  All tool calls require\n  approval. Learn more in the\n  MCP documentation.\n\n"+
			"    [✔] probe\n    [✔] probe-two\n    [✔] probe-three\n     ❯ Enable selected\n"+
			"  Press Ctrl-C again to exit\n all")
	// A notice prefix over the tail, mid-paint — #7730 review's R2 pane, a
	// live folder-trust dialog once pinned clear as "no dialog".
	content := "Do you trust the files in this folder?\n\n❯ 1. Yes, proceed\n  2. No, exit\n\n Press Ctrl-C ag\n cancel"
	requireRemnantHold(t, "the legacy dialog under a notice prefix with the footer's tail", content)
	require.Equal(t, DialogHoldFolderPartial, claudeDialogHoldKind(claudeScreenOf(content)),
		"the pane proves the legacy dialog's option rows, so the hold is named")
	// The notice itself wrapped onto two rows over the tail.
	requireRemnantHold(t, "a wrapped notice over the footer's tail",
		"\n────────────────────────────\n"+
			"  New MCP server found in\n  this project: probe\n\n"+
			"  MCP servers may execute\n  code or access system\n  resources. All tool\n  calls require approval.\n  Learn more in the MCP\n  documentation.\n\n"+
			"  ❯ Use this MCP server\n    Use this and all future\n    MCP servers in this\n    project\n    Continue without using\n    this MCP server\n\n"+
			"  Press Ctrl-C\n  again to exit\n  to cancel")
	// A tail licenses the swap only as a suffix of that dialog's own footer:
	// a suffix of another dialog's footer, or of no footer's, leaves the
	// pane the way the notice's readers left it, and a notice in a
	// composer's status row stays the composer's own.
	mcp28 := "\n────────────────────────────\n" +
		"  New MCP server found in\n  this project: probe\n\n" +
		"  ❯ Use this MCP server\n    Continue without using\n    this MCP server\n"
	pick32 := "────────────────────────────────\n" +
		"  3 new MCP servers found in\n  this project\n  Select any you wish to enable.\n\n" +
		"  ❯ [✔] probe\n    [✔] probe-two\n       Enable selected\n"
	for _, tt := range []struct{ name, content string }{
		{"the single-server dialog over the picker's footer tail", mcp28 + "  Press Ctrl-C again to exit\n  all"},
		{"the picker over the shared footer's tail", pick32 + "  Press Ctrl-C again to exit\n  to cancel"},
		{"the dialog over a tail no footer ends with", mcp28 + "  Press Ctrl-C again to exit\n  some other row"},
		{"a ruled composer with a notice prefix over a tail",
			composerRule + "\n❯ \n" + composerRule + "\n  Press Ctrl-C ag\n cancel"},
	} {
		requireRemnantClear(t, tt.name, tt.content)
	}
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
		// #7725: a notice only begun is the notice's — under a composer's
		// closing line, alone, or over text it does not name a dialog under.
		{"a notice prefix in a ruled composer's status row", ruled + "  Press Ctrl-C ag"},
		{"a notice prefix in a boxed composer's status row", boxed + "  Press Ctrl-C ag"},
		{"a notice prefix alone", "Press Ctrl-C ag"},
		{"a notice prefix under transcript", "● Done.\n\nPress Ctrl-C ag"},
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
