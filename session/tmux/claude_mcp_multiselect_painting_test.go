package tmux

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7504, found by the independent review of #7497.
// Claude Code 2.1.322 paints its multi-server picker top-down, and these
// frames of that paint read clear:
//   - the picker under a composer an earlier Claude left above it, as a
//     relaunch in the same pane leaves one;
//   - a row only begun;
//   - a header or server name the pane wraps.
// A clear pane lets the create type its prompt and press Enter, which
// enables every pre-checked server once the picker finishes painting.

// mcpServersPickerCaptures is every real capture of the multi-server picker.
var mcpServersPickerCaptures = []string{
	mcpServersTwoFixture, mcpServersThreeFixture, mcpServersThreeWideFixture,
	mcpServersSixFixture, mcpServersSixScrolledFixture, mcpServers40ColumnsFixture, mcpServersLongNameFixture,
}

// staleComposerRows is what an earlier Claude left on screen above the
// picker: the issue's ruled composer with Claude Code's U+00A0 after its ❯,
// that composer with a draft, and the two real 2.1.322 captures of the
// composer after an MCP dialog, whole.
func staleComposerRows(t *testing.T) map[string][]string {
	t.Helper()
	rule := strings.Repeat("─", 12)
	return map[string][]string{
		"the issue's ruled composer":      {rule, claudeTrustSelectionGlyph + "\u00a0", rule, "? for shortcuts"},
		"a ruled composer with a draft":   {rule, claudeTrustSelectionGlyph + "\u00a0fix it", rule},
		"the composer after a decline":    strings.Split(strings.TrimRight(readTrustFixture(t, mcpServerComposerFixture), "\n"), "\n"),
		"the composer after a reject-all": strings.Split(strings.TrimRight(readTrustFixture(t, mcpServersRejectedFixture), "\n"), "\n"),
	}
}

// rowStarts returns row painted as far as its first letter and as far as each
// of its words, short of the whole row, as visible text.
func rowStarts(row string) []string {
	runes := []rune(string(visibleTextOf(row)))
	var starts []string
	first := true
	for c := 1; c < len(runes); c++ {
		if runes[c-1] == ' ' {
			continue
		}
		if first || runes[c] == ' ' {
			starts = append(starts, string(runes[:c]))
		}
		first = false
	}
	return starts
}

// The issue's frame: the two-server picker painted from its top rule down to
// its "Enable selected" row, under a ruled composer an earlier Claude left
// with U+00A0 after its ❯. Then every capture painted to each of its rows from
// the header down, under each stale composer. The whole picker under one still
// holds, with no Esc.
func TestCheckAndHandleTrustPrompt_Issue7504PickerPaintingUnderAStaleComposerHolds(t *testing.T) {
	issue := "────────────\n❯\u00a0\n────────────\n? for shortcuts\n" + strings.Join([]string{
		"────────────────────────────────────────────────────────────────────────────────",
		"  2 new MCP servers found in this project",
		"  Select any you wish to enable.",
		"",
		"  MCP servers may execute code or access system resources. All tool calls",
		"  require approval. Learn more in the MCP documentation.",
		"",
		"  ❯ [✔] probe",
		"    [✔] probe-two",
		"       Enable selected",
	}, "\n")
	requireHeldWithoutAKey(t, issue, "the issue's frame (#7504)")
	for _, fixture := range mcpServersPickerCaptures {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		for name, composer := range staleComposerRows(t) {
			for k := header; k < len(rows); k++ {
				frame := strings.Join(append(slices.Clone(composer), rows[:k+1]...), "\n")
				requireHeldWithoutAKey(t, frame, fmt.Sprintf("%s, painted to its row %d, under %s", fixture, k, name))
			}
		}
	}
}

// The paint stopped inside a row: "❯ [✔]" before the server's name,
// "Enable" or "Space to select ·". Each is none of the grammar's whole rows,
// so the picker read clear alone as well as under a stale composer. The
// header is cut no shorter than its count and "new MCP servers", the most a
// wrap leaves on its first row.
func TestCheckAndHandleTrustPrompt_Issue7504PickerPaintedPartWayThroughARowHolds(t *testing.T) {
	issueComposer := staleComposerRows(t)["the issue's ruled composer"]
	for _, fixture := range mcpServersPickerCaptures {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		for k := header; k < len(rows); k++ {
			for _, start := range rowStarts(rows[k]) {
				if k == header && !claudeMCPServersHeaderStart.MatchString(strings.TrimSpace(start)) {
					continue
				}
				painted := append(slices.Clone(rows[:k]), start)
				requireHeldWithoutAKey(t, strings.Join(painted, "\n"), fmt.Sprintf("%s alone, painted as far as %q", fixture, start))
				requireHeldWithoutAKey(t, strings.Join(append(slices.Clone(issueComposer), painted...), "\n"),
					fmt.Sprintf("%s under the issue's composer, painted as far as %q", fixture, start))
			}
		}
	}
}

// At 40 columns Claude Code wraps the header ("3 new MCP servers found in
// this" over "project"), and a long server name wraps onto a row of its own.
// The grammar reads the header only as one row and each server only as one
// checkbox row, so every step of the 40-column paint, and the long-name paint
// from its wrapped row down, read clear with nothing above the picker.
func TestCheckAndHandleTrustPrompt_Issue7504PickerWithAWrappedRowHoldsWhilePainting(t *testing.T) {
	for _, fixture := range []string{mcpServers40ColumnsFixture, mcpServersLongNameFixture} {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		for k := header; k < len(rows)-1; k++ {
			requireHeldWithoutAKey(t, strings.Join(rows[:k+1], "\n"), fmt.Sprintf("%s alone, painted to its row %d", fixture, k))
		}
	}
}

// What is no picker being painted stays clear: the picker quoted whole, cut
// inside a row, or only its header, above a working agent's composer; the
// picker pasted into a composer draft, over the draft's closing rule; and the
// real composer captures alone. The composer below the header is a rule or a
// second ❯ the picker never paints.
func TestCheckAndHandleTrustPrompt_Issue7504PickerQuotedAboveAComposerStaysClear(t *testing.T) {
	composer := strings.TrimRight(readTrustFixture(t, mcpServerComposerFixture), "\n")
	frames := map[string]string{
		"the composer after a decline, alone":          composer,
		"the composer after a reject-all, alone":       strings.TrimRight(readTrustFixture(t, mcpServersRejectedFixture), "\n"),
		"the header quoted above the composer":         "  2 new MCP servers found in this project\n" + composer,
		"the wrapped header quoted above the composer": "  3 new MCP servers found in this\n  project\n" + composer,
	}
	rule := strings.Repeat("─", 80)
	for _, fixture := range mcpServersPickerCaptures {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		// The picker pasted into a multi-line composer draft, as far as each
		// of its rows, under the draft's ❯ and over its closing rule, with
		// and without the status row below (#7507 review).
		for k := header; k < len(rows); k++ {
			draft := append([]string{rule, claudeTrustSelectionGlyph + " Explain this picker:"}, rows[header:k+1]...)
			draft = append(draft, rule)
			frames[fmt.Sprintf("%s to its row %d, in a composer draft", fixture, k)] = strings.Join(draft, "\n")
			frames[fmt.Sprintf("%s to its row %d, in a composer draft over its status row", fixture, k)] = strings.Join(draft, "\n") + "\n  ? for shortcuts"
		}
		frames[fixture+" whole, above the composer"] = strings.Join(rows, "\n") + "\n" + composer
		frames[fixture+" whole, above the ruled composer"] = strings.Join(rows, "\n") + ruledComposerBelow
		for k := header; k < len(rows); k++ {
			for _, start := range rowStarts(rows[k]) {
				painted := strings.Join(append(slices.Clone(rows[:k]), start), "\n")
				frames[fmt.Sprintf("%s cut at %q, above the ruled composer", fixture, start)] = painted + ruledComposerBelow
				frames[fmt.Sprintf("%s cut at %q, above the boxed composer", fixture, start)] = painted + boxedComposerBelow
			}
		}
	}
	for name, content := range frames {
		handled, cmds := pollStaticPane(t, content, 4)
		require.False(t, handled, "%s:\n%s", name, content)
		require.Empty(t, sentKeystrokes(cmds), "%s", name)
	}
}
