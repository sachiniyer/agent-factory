package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyPaintedDialogs are the legacy folder-trust dialog in each shape af
// knows, at a width: plain, indented two columns, and in a box with its footer
// below the box. Each is returned with the index of its cursor row.
func legacyPaintedDialogs(width int) map[string][]string {
	plain := legacyTrustDialogRows(width, true)
	content := max(1, width-4)
	boxed := []string{"╭" + strings.Repeat("─", width-2) + "╮"}
	for _, row := range legacyTrustDialogRows(content, false) {
		boxed = append(boxed, "│ "+row+strings.Repeat(" ", max(0, content-len([]rune(row))))+" │")
	}
	boxed = append(boxed, "╰"+strings.Repeat("─", width-2)+"╯")
	boxed = append(boxed, indentedRows(width, "  "+legacyTrustFooter)...)
	return map[string][]string{
		"plain": plain,
		"indented two columns": indentedRows(width,
			"  Do you trust the files in this folder?", "", "  /home/u/proj", "",
			"  Claude Code may read files in this folder.", "",
			"  ❯ 1. Yes, proceed", "    2. No, exit", "", "  "+legacyTrustFooter),
		"in a box, its footer below it": boxed,
		"short labels, no body":         {"Do you trust the files in this folder?", "❯ 1. Yes", "  2. No"},
	}
}

// legacyOptionRows is where a painted legacy dialog's option block runs: from
// its cursor row to its last option row.
func legacyOptionRows(rows []string) (first, last int) {
	first = -1
	for i, row := range rows {
		label := strings.TrimSpace(strings.Trim(strings.TrimSpace(row), claudeTrustBoxDrawing))
		switch {
		case first < 0 && strings.HasPrefix(label, claudeTrustSelectionGlyph):
			first, last = i, i
		case first >= 0 && label != "" && !claudeTrustFrameRule(row) && !strings.HasPrefix(label, "Enter"):
			last = i
		case first >= 0:
			return first, last
		}
	}
	return first, last
}

// legacyPaintSteps is the dialog painted top-down from the step its question
// is whole until its option block is: every row, and each option row at every
// character after its leading chrome. It leaves out each step whose last row
// already reads as a No label the grammar knows ("2. No" reads as "No"), and
// stops before the last option row's label is whole. The grammar answers those
// steps, before the dialog is whole, which is #7584's second part. It also
// leaves out a step whose last row reads as a composer's draft line
// (claudeComposerPromptText): "❯ 1", the ordinal before its dot, is a draft of
// "1" as much as a cursor row begun, and the reader holds no draft.
func legacyPaintSteps(rows []string) []string {
	first, last := legacyOptionRows(rows)
	question := claudeSquash(claudeLegacyTrustQuestion)
	var frames []string
	for k := 0; k < first; k++ {
		if strings.Contains(claudeSquash(strings.Join(rows[:k+1], "")), question) {
			frames = append(frames, strings.Join(rows[:k+1], "\n"))
		}
	}
	for k := first; k <= last; k++ {
		row := []rune(strings.TrimRight(rows[k], " "))
		lead := len(row) - len([]rune(strings.TrimLeft(string(row), " │")))
		label := strings.TrimRight(string(row), " │")
		for c := lead; c <= len(row); c++ {
			if k == last && strings.TrimRight(string(row[:c]), " │") == label {
				break
			}
			if _, no := claudeLegacyOptionKinds(claudeTrustRowOf(string(row[:c])).label); no && k > first {
				continue
			}
			if draft, prompt := claudeComposerPromptText(string(row[:c]), false); prompt && draft != "" {
				continue
			}
			frames = append(frames, strings.Join(append(append([]string(nil), rows[:k]...), string(row[:c])), "\n"))
		}
	}
	return frames
}

// legacyPaintSettings are what can sit above the dialog when Claude Code
// paints it: nothing, or a composer an earlier Claude left in the pane, with
// and without its status row.
func legacyPaintSettings(width int) map[string]string {
	rule := strings.Repeat("─", width)
	return map[string]string{
		"nothing above":                      "",
		"an earlier ruled composer":          rule + "\n❯ \n" + rule + "\n",
		"an earlier composer and its status": rule + "\n❯ \n" + rule + "\n  ? for shortcuts\n",
	}
}

// TestIssue7584LegacyDialogHoldsWhileItsOptionRowsPaint pins #7584's first
// property: the legacy folder-trust dialog painted from the top down holds at
// every step from its question until its option block is whole. Before it, a
// row only begun read clear, and the create typed its prompt and pressed Enter
// into the dialog: "❯", "❯ 1." or "❯ 1. Yes," as the cursor row, and "2",
// "2." or "2. No," under it.
func TestIssue7584LegacyDialogHoldsWhileItsOptionRowsPaint(t *testing.T) {
	for _, width := range []int{12, 20, 40, 80} {
		for shape, rows := range legacyPaintedDialogs(width) {
			for setting, above := range legacyPaintSettings(width) {
				for i, frame := range legacyPaintSteps(rows) {
					requireHeldWithoutAKey(t, above+frame, fmt.Sprintf(
						"the legacy dialog, %s, at %d columns, under %s, paint step %d must hold without a key",
						shape, width, setting, i))
				}
			}
		}
	}
}

// TestIssue7584WholeLegacyDialogIsStillAnswered: the reader holds only while
// the option block is begun, so a whole, settled legacy dialog is answered as
// before, with one Enter on its preselection, and a create never stalls on it.
// The grammar answers a question painted on one row of its own, so this runs
// at the widths where the question fits on one. It polls once, as
// TestCheckAndHandleTrustPrompt_RealLegacyFolderTrustDialogStillFiresEnter
// does: the legacy branch taps Enter on every poll that still shows the
// dialog, and a static pane never stops showing it.
func TestIssue7584WholeLegacyDialogIsStillAnswered(t *testing.T) {
	for _, width := range []int{40, 80} {
		for shape, rows := range legacyPaintedDialogs(width) {
			for setting, above := range legacyPaintSettings(width) {
				if setting != "nothing above" && shape == "short labels, no body" {
					continue
				}
				if width < 80 && strings.HasPrefix(shape, "in a box") {
					continue
				}
				t.Run(fmt.Sprintf("%s at %d columns under %s", shape, width, setting), func(t *testing.T) {
					handled, cmds := runTrustPromptCheck(t, ProgramClaude, above+strings.Join(rows, "\n"))
					require.True(t, handled, "the whole legacy dialog is in the way")
					require.Equal(t, []string{"Enter"}, injectedKeyNames(sentKeystrokes(cmds)),
						"the whole legacy dialog keeps its Enter on Claude Code's preselection")
				})
			}
		}
	}
}

// legacyQuotingDrafts are composer drafts that quote the legacy dialog's
// question and options: both options on the prompt row, as the inline picker
// paints them and as a user types them, the stacked block typed on one row, and
// the question and block over several rows.
var legacyQuotingDrafts = map[string][]string{
	"Yes No":                        {"Yes No"},
	"Yes 2. No":                     {"Yes 2. No"},
	"Yes, proceed No, exit":         {"Yes, proceed No, exit"},
	"the block on one row":          {"1. Yes, proceed 2. No, exit"},
	"the question and its options":  {"Do you trust the files in this folder? Yes No"},
	"the question over the options": {"Do you trust the files in this folder?", "1. Yes, proceed", "2. No, exit"},
	"the options alone":             {"Yes, proceed", "No, exit"},
}

// TestIssue7584SettledScreensAreLeftAlone: the screens a create meets once a
// dialog is gone, or that a working agent shows, read as before.
//   - Each paint step of the legacy dialog, quoted above a composer drawn
//     last, reads clear and gets no key: a working agent frames its composer
//     with rules, so the reader declines a question with one below it.
//   - A user's draft quoting the question and its options, under the question
//     quoted above it, reads clear and gets no key: in every composer mode,
//     ruled and boxed, and with no frame at all where the draft puts both
//     options on the prompt row. The dialog never paints a draft line
//     (#7594's CI run, TestCheckAndHandleTrustPrompt_YesNoComposerDraftIsNotTheLegacyPicker).
//   - The idle composer Claude Code shows once its dialog is answered reads
//     clear.
//   - The real reworded dialog (testdata, #7575) is still answered.
func TestIssue7584SettledScreensAreLeftAlone(t *testing.T) {
	for _, lead := range []string{
		"Do you trust the files in this folder?",
		"I was asked: \"Do you trust the files in this folder?\"\nDo you trust the files in this folder?",
	} {
		for name, draft := range legacyQuotingDrafts {
			for _, prompt := range composerModePrompts {
				for _, boxed := range []bool{false, true} {
					requireClearWithoutAKey(t, lead+"\n"+strings.Join(drawnComposer(40, prompt, draft, boxed), "\n"),
						fmt.Sprintf("a draft quoting %s under %q, boxed %v", name, prompt, boxed))
				}
			}
			// Unframed, only under prose: with the question the pane's first
			// content, "❯ Yes No" is the inline picker itself, which master
			// answers (TestCheckAndHandleTrustPrompt_RealLegacyFolderTrustDialogStillFiresEnter).
			if strings.HasPrefix(lead, "I was asked") && len(draft) == 1 && strings.Contains(draft[0], "No") {
				requireClearWithoutAKey(t, lead+"\n"+claudeTrustSelectionGlyph+" "+draft[0],
					fmt.Sprintf("an unframed draft putting both options on the prompt row: %s", name))
			}
		}
	}

	for _, width := range []int{20, 40, 80} {
		rule := strings.Repeat("─", width)
		composers := map[string]string{
			"a ruled composer and its status": rule + "\n❯ \n" + rule + "\n  ? for shortcuts",
			"a ruled composer with a draft":   rule + "\n❯ explain the dialog above\n" + rule,
			"a boxed composer": "╭" + strings.Repeat("─", width-2) + "╮\n│ > " +
				strings.Repeat(" ", max(0, width-6)) + " │\n╰" + strings.Repeat("─", width-2) + "╯",
		}
		for shape, rows := range legacyPaintedDialogs(width) {
			frames := append(legacyPaintSteps(rows), strings.Join(rows, "\n"))
			for name, composer := range composers {
				for i, frame := range frames {
					requireClearWithoutAKey(t, frame+"\n\n"+composer, fmt.Sprintf(
						"the legacy dialog, %s, at %d columns, paint step %d, quoted above %s",
						shape, width, i, name))
				}
			}
		}
	}

	for _, name := range []string{mcpServerComposerFixture, mcpServersRejectedFixture} {
		requireClearWithoutAKey(t, readTrustFixture(t, name), "the idle composer after a dialog: "+name)
	}

	for _, name := range []string{rewordedDialogFixture, rewordedDialogNoLinkFixture} {
		handled, cmds := pollStaticPane(t, readTrustFixture(t, name), 4)
		require.True(t, handled, "the real reworded dialog is in the way: %s", name)
		require.Equal(t, []string{"Down"}, injectedKeyNames(sentKeystrokes(cmds)),
			"the real reworded dialog is still answered: %s", name)
	}
}
