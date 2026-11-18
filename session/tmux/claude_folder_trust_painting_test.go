package tmux

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7505, found by the independent review of #7497.
// Claude Code paints the reworded folder-trust dialog top-down, and these
// frames of that paint read clear:
//   - its title and path, and its paragraph before the question is whole;
//   - at 10 columns or fewer, an option row whose label the pane wraps, once
//     the label's next row is painted ("❯ 1. Yes," over "I");
//   - at any width, the option row cut at its ordinal ("❯ 1.").
// A clear pane lets the create type its prompt and press Enter into the
// dialog.

// folderTrustOrdinalOptions are the dialog's options as 2.1.322 paints them,
// each led by its ordinal.
var folderTrustOrdinalOptions = []string{"1. " + claudeTrustYesLabel, "2. " + claudeTrustNoLabel}

// folderTrustPaintFrom returns the synthetic dialog's rows at width, with the
// cursor on option selected, and the rows its title ends and its footer
// starts on.
func folderTrustPaintFrom(width, selected int) (rows []string, title, footer int) {
	frame := renderSyntheticClaudeFolderTrust(width, folderTrustOrdinalOptions, selected, "")
	rows = strings.Split(strings.TrimSuffix(frame.text, "\n"), "\n")[:frame.contentRows]
	title = -1
	for i, row := range rows {
		if strings.Contains(string(visibleTextOf(row)), "workspace:") {
			title = i
			break
		}
	}
	return rows, title, frame.footerRow
}

// The issue's frame: at 10 columns, the dialog painted down to its option
// label's third row. Then the dialog at 4 to 24 columns, and at 40 and 80,
// with the cursor on either option, painted from its whole title down to each
// row above the footer, and to each row's first letter and word ends. Every
// width is painted with nothing above it; 6, 10, 24 and 80 columns also under
// each stale composer an earlier Claude can leave above it. Every frame holds,
// with no key.
func TestCheckAndHandleTrustPrompt_Issue7505FolderTrustPaintedFromItsHeadHolds(t *testing.T) {
	rows, _, _ := folderTrustPaintFrom(10, 0)
	issue := -1
	for i, row := range rows {
		if strings.TrimSpace(string(visibleTextOf(row))) == "this" {
			issue = i
		}
	}
	require.Positive(t, issue, "precondition: the 10-column dialog wraps its affirmative label onto a row that reads \"this\"")
	requireHeldWithoutAKey(t, strings.Join(rows[:issue+1], "\n"), "the issue's frame (#7505)")

	stale := staleComposerRows(t)
	widths := []int{40, 80}
	for width := 4; width <= 24; width++ {
		widths = append(widths, width)
	}
	for _, width := range widths {
		composers := map[string][]string{"nothing": nil}
		if slices.Contains([]int{6, 10, 24, 80}, width) {
			maps.Copy(composers, stale)
		}
		for selected := range folderTrustOrdinalOptions {
			rows, title, footer := folderTrustPaintFrom(width, selected)
			for k := title; k < footer; k++ {
				steps := []string{rows[k]}
				if k > title {
					steps = append(rowStarts(rows[k]), rows[k])
				}
				for _, step := range steps {
					painted := append(slices.Clone(rows[:k]), step)
					for name, composer := range composers {
						requireHeldWithoutAKey(t, strings.Join(append(slices.Clone(composer), painted...), "\n"),
							fmt.Sprintf("at %d columns, option %d selected, painted as far as %q, under %s", width, selected+1, step, name))
					}
				}
			}
		}
	}
}

// What is no dialog being painted stays clear: the dialog quoted above a
// working agent's ruled or boxed composer, cut at each row and each row's
// first letter and word ends, and its title alone quoted the same way. The
// composer's rules below the dialog's head say the dialog is output.
func TestCheckAndHandleTrustPrompt_Issue7505FolderTrustQuotedAboveAComposerStaysClear(t *testing.T) {
	frames := map[string]string{
		"the title quoted above the ruled composer": "  " + syntheticFolderTrustTitle + ruledComposerBelow,
		"the title quoted above the boxed composer": "  " + syntheticFolderTrustTitle + boxedComposerBelow,
	}
	for _, width := range []int{6, 10, 16, 24, 40, 80} {
		rows, title, footer := folderTrustPaintFrom(width, 0)
		for k := title; k <= footer; k++ {
			for _, step := range append(rowStarts(rows[k]), rows[k]) {
				painted := strings.Join(append(slices.Clone(rows[:k]), step), "\n")
				at := fmt.Sprintf("at %d columns, cut at %q", width, step)
				frames[at+", above the ruled composer"] = painted + ruledComposerBelow
				frames[at+", above the boxed composer"] = painted + boxedComposerBelow
			}
		}
	}
	for name, content := range frames {
		handled, cmds := pollStaticPane(t, content, 4)
		require.False(t, handled, "%s:\n%s", name, content)
		require.Empty(t, sentKeystrokes(cmds), "%s", name)
	}
}
