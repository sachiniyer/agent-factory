package tmux

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7866: a pane the gate holds over a dialog's WHOLE
// footer also holds when that footer is replaced by a proper prefix of itself
// that begins a row — the same dialog caught while its footer still paints.
// That is #7390's premise, applied to #7618's narrow captures.
//
// claudeLaunchDialogFooterPainting (the re-wrap reader's begun-footer clause)
// reads a begun footer only under whole option labels, which it reads up from
// the footer. A narrow pane overprints the labels ("1.Yes" over "❯ trust th")
// or cuts them ("fold"), and a short pane keeps only the cursor's, so neither
// it nor the remnant reader — which wanted the footer whole — held the frame:
// a real narrow dialog read clear while its footer painted, and the create
// typed the user's prompt into it. The remnant reader's under-guide clause now
// reads a footer only begun the way it reads a whole one: same rows above it,
// same ❯-row and closed-box guards, asked of every content row the way the
// whole-footer suffix is asked.

// footerPrefixCut is a capture's pane cut so that a proper footer prefix is
// its last content.
type footerPrefixCut struct {
	prefix  string // the footer prefix the cut leaves, squashed
	content string // the pane's lines down to and including the cut row
}

// claudeLaunchDialogFooterPrefixCuts renders every cut of the capture's pane
// whose last content is a proper prefix of a footer that begins a row — one
// cut per rune boundary strictly inside the pane's whole-footer suffix. A
// boundary at a row start keeps every footer row above it whole; one inside a
// row truncates that row's line to the prefix's own runes.
func claudeLaunchDialogFooterPrefixCuts(t *testing.T, capture string, footers []string) []footerPrefixCut {
	t.Helper()
	pane := claudeLaunchDialogPaneOf(claudeScreenOf(capture))
	end := -1
	for _, footer := range footers {
		if strings.HasSuffix(pane.text, footer) {
			end = len(pane.text) - len(footer)
		}
	}
	require.GreaterOrEqual(t, end, 0, "the capture ends with the footer")
	_, ok := pane.rowStart(end)
	require.True(t, ok, "the footer begins a row")
	var cuts []footerPrefixCut
	for p := end + 1; p < len(pane.text); p++ {
		if !utf8.RuneStart(pane.text[p]) {
			continue
		}
		row := len(pane.starts) - 1
		for pane.starts[row] >= p {
			row--
		}
		line := pane.index[row]
		last := squashedPrefix(pane.lines[line], pane.text[pane.starts[row]:p])
		require.Equal(t, pane.text[pane.starts[row]:p], claudeSquash(last),
			"the cut must leave exactly the prefix of the row at %d", p)
		cuts = append(cuts, footerPrefixCut{
			prefix:  pane.text[end:p],
			content: strings.Join(append(append([]string{}, pane.lines[:line]...), last), "\n"),
		})
	}
	return cuts
}

// squashedPrefix returns the longest prefix of line whose squashed text is a
// prefix of keep.
func squashedPrefix(line, keep string) string {
	var squashed strings.Builder
	for i, r := range line {
		if unicode.IsSpace(r) {
			continue
		}
		squashed.WriteRune(r)
		if !strings.HasPrefix(keep, squashed.String()) {
			return line[:i]
		}
	}
	return line
}

// remnantPaintingNarrowFixtures are the remnantNarrowFixtures whose every
// proper-footer-prefix cut read clear on master — the rows this test calls
// master-red. All but two of them: the 2_1_113 and 2_1_200 captures at 10
// columns already held at every cut on master (under
// claudeFolderTrustPaintingOf), so they sit in the settled control table
// below.
var remnantPaintingNarrowFixtures = []string{
	"claude_2_0_25_reworded_arm_folder_trust_dialog_12_columns.ansi",
	"claude_2_0_50_reworded_arm_folder_trust_dialog_10_columns.ansi",
	"claude_2_1_19_reworded_arm_folder_trust_dialog_4_columns.ansi",
	"claude_2_1_19_reworded_arm_folder_trust_dialog_10_columns.ansi",
	"claude_2_1_20_folder_trust_dialog_9_columns.ansi",
	"claude_2_1_200_folder_trust_dialog_configured_15_columns.ansi",
	"claude_2_1_323_folder_trust_dialog_8_columns.ansi",
	"claude_2_1_327_folder_trust_dialog_4_columns.ansi",
	"claude_2_1_328_folder_trust_dialog_4_columns.ansi",
	"claude_2_1_329_folder_trust_dialog_4_columns.ansi",
}

// Every narrow and short-pane folder-trust capture, cut after every proper
// prefix of its footer that begins a row, holds: it held over the whole
// footer, and the prefix is the same footer still painting. On master each of
// these cuts read clear — the bug #7866 reported.
func TestIssue7866NarrowPaneDialogHoldsAtEveryFooterPrefix(t *testing.T) {
	for _, name := range append(append([]string{}, remnantPaintingNarrowFixtures...), remnantShortFixtures...) {
		capture := readTrustFixture(t, name)
		cuts := claudeLaunchDialogFooterPrefixCuts(t, capture, claudeLaunchDialogFooters)
		require.NotEmpty(t, cuts, "%s: the capture paints no proper footer prefix", name)
		for _, cut := range cuts {
			requireHeldWithoutAKey(t, cut.content, fmt.Sprintf(
				"%s, its footer painted only as far as %q: the pane holds over the whole footer, and the prefix is the same dialog still painting (#7866)",
				name, cut.prefix))
		}
	}
}

// The settled captures the under-guide clause holds under an 80-column pane —
// the boxed legacy dialogs and the 2.1.328 one 10 rows tall — keep holding at
// every proper footer prefix too: each of those cuts was already a hold on
// master, under one clause or another, and the clause the remnant reader adds
// only makes it earlier. The two 10-column captures of remnantNarrowFixtures
// whose cuts already held on master under claudeFolderTrustPaintingOf are
// covered here as well: they are controls, not master-red rows.
func TestIssue7866SettledDialogHoldsAtEveryFooterPrefix(t *testing.T) {
	for _, name := range []string{
		"claude_2_0_25_legacy_folder_trust_dialog.ansi",
		"claude_2_0_50_legacy_folder_trust_dialog.ansi",
		"claude_2_1_19_legacy_folder_trust_dialog.ansi",
		"claude_2_1_328_folder_trust_dialog_80x10.ansi",
		"claude_2_1_113_folder_trust_dialog_10_columns.ansi",
		"claude_2_1_200_folder_trust_dialog_10_columns.ansi",
		// The 2.1.322 single-server dialog paints the reworded folder-trust
		// footer word for word (claudeMCPServerFooter).
		"claude_2_1_322_mcp_server_dialog_40_columns.ansi",
	} {
		capture := readTrustFixture(t, name)
		cuts := claudeLaunchDialogFooterPrefixCuts(t, capture, claudeLaunchDialogFooters)
		require.NotEmpty(t, cuts, "%s: the capture paints no proper footer prefix", name)
		for _, cut := range cuts {
			requireHeldWithoutAKey(t, cut.content, fmt.Sprintf(
				"%s, its footer painted only as far as %q (#7866)", name, cut.prefix))
		}
	}
}

// The multi-server picker too, over its own footer ("Space to select · Esc to
// reject all", not one a folder-trust dialog paints): its own readers already
// hold it at every proper prefix that begins a row, and the remnant reader's
// new clause leaves that so.
func TestIssue7866MultiServerPickerHoldsAtEveryFooterPrefix(t *testing.T) {
	capture := readTrustFixture(t, "claude_2_1_322_mcp_multi_server_dialog_40_columns.ansi")
	cuts := claudeLaunchDialogFooterPrefixCuts(t, capture, []string{claudeSquash(claudeMCPServersFooter)})
	require.NotEmpty(t, cuts)
	for _, cut := range cuts {
		requireHeldWithoutAKey(t, cut.content, fmt.Sprintf(
			"the 40-column picker, its footer painted only as far as %q (#7866)", cut.prefix))
	}
}

// The panes too small for anything but the end of a footer
// (remnantFooterTailFixtures) paint no proper prefix of it that begins a row —
// their remnant is a suffix of the footer, not a prefix — so the table over
// footer prefixes has no row for them. Their footer-tail remnant already
// holds, which TestIssue7618FooterTailNeverReadsClear pins.

// A composer or an idle pane is no dialog: none of the added holds may turn
// one. These are the ordinary frames the create types into.
func TestIssue7866OrdinaryFramesStayClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"a real 2.1.328 idle composer at 10 columns", readTrustFixture(t, "claude_2_1_328_idle_composer_10x24.ansi")},
		{"a real 2.1.328 idle composer at 80 columns", readTrustFixture(t, "claude_2_1_328_idle_composer_80x24.ansi")},
		{"a real 2.1.329 composer showing the exit notice", readTrustFixture(t, "claude_2_1_329_idle_composer_exit_notice_80x24.ansi")},
		{"a real 2.1.329 composer showing the exit notice at 12 columns", readTrustFixture(t, "claude_2_1_329_idle_composer_exit_notice_12x24.ansi")},
		{"a real 2.0.50 composer showing the exit notice", readTrustFixture(t, "claude_2_0_50_idle_composer_exit_notice_80x24.ansi")},
		{"a real 2.1.322 composer after an MCP decline", readTrustFixture(t, "claude_2_1_322_composer_after_mcp_decline.ansi")},
		{"a real 2.1.322 composer after rejecting all servers", readTrustFixture(t, "claude_2_1_322_composer_after_mcp_reject_all.ansi")},
		{"a prompt echo and its output", "❯ fix the bug\n\n● Fixed it." + ruledComposerBelow},
		{"prose that happens to begin the footer", "I will now confirm\n\nEnter to"},
		{"a boxed composer draft that begins the footer", "╭──────────────────────────╮\n│ > the modal says Enter to │\n╰──────────────────────────╯\n"},
	} {
		requireClearWithoutAKey(t, tt.content, fmt.Sprintf("%s: an ordinary launch or idle frame is no dialog, and the create types into it (#7866)", tt.name))
	}
}
