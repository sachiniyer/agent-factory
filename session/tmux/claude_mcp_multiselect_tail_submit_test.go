package tmux

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7873, found by the independent review of #7872. On a
// short pane whose top cut has scrolled the "N new MCP servers found" header
// off, the multi-server picker read clear one paint instant before the case
// #7870 fixed: its footer row had not begun, and the pane's last content was
// the "Enable selected" row — whole, begun, or carrying the ❯ cursor. The
// create typed its prompt into the picker, and Enter confirmed whichever row
// owned the cursor, pre-checked servers that run code among them.
//
// A pane that holds over the picker's footer — whole or begun — now holds
// over the "Enable selected" row as last content too, directly under the
// picker's checkbox rows or the pane's top edge, at any top cut. The hold is
// silent, as painting, the way the begun-footer read's is: no refusal, and
// the mcp-partial hold kind.

// The frame #7873 filed: rows 4–15 of the 40-column capture — the wrapped
// header's second row down to "Enable selected" — with no footer row, or
// with the footer painted only as far as its leading space, which is no row
// at all. Plus the issue's two variants: the submit row begun, and the ❯
// cursor on it.
func TestCheckAndHandleTrustPrompt_Issue7873PickerBeforeItsFooterBeginsHolds(t *testing.T) {
	_, _, errLog := captureTrustPromptLogs(t)
	pane := []string{
		"  project",
		"  Select any you wish to enable.",
		"",
		"  MCP servers may execute code or",
		"  access system resources. All tool",
		"  calls require approval. Learn more",
		"  in the MCP documentation.",
		"",
		"  ❯ [✔] probe",
		"    [✔] probe-two",
		"    [✔] probe-three",
	}
	for _, last := range []string{
		"       Enable selected",
		"       Enab",
		"     ❯ Enable selected",
	} {
		frame := strings.Join(append(append([]string{}, pane...), last), "\n")
		// The ❯ variant has its cursor on the submit row: no server row
		// carries one.
		if claudeTrustRowOf(last).selected {
			frame = strings.Join(append(mcpServersUncursored(pane), last), "\n")
		}
		requireHeldWithoutAKey(t, frame, fmt.Sprintf(
			"rows 4-14 of the 40-column capture over %q (#7873)", last))
		require.Equal(t, DialogHoldMCPPartial, claudeDialogHoldKind(claudeScreenOf(frame)),
			"a pre-footer frame is a partial hold, the begun-footer read's kind")
		// The footer painted only as far as its leading space is no row at
		// all: blank rows are not content.
		requireHeldWithoutAKey(t, frame+"\n ",
			"the same frame with the footer's leading space painted")
	}
	require.Zero(t, strings.Count(errLog.String(), "refusing to answer"),
		"a pre-footer frame holds silently, as painting, the way a begun footer does")
}

// mcpServersSubmitCut is one way the picker's "Enable selected" row ends a
// pane: the row's text as painted, and whether it carries the ❯ cursor —
// which no other row on the same pane can then carry.
type mcpServersSubmitCut struct {
	last   string
	cursor bool
}

// mcpServersSubmitRowCuts returns row — a picker's "Enable selected" row —
// painted whole, with the ❯ cursor on it, and as every begun prefix of
// either: each proper prefix of the row's visible text that leaves the row's
// label non-empty, deduplicated by that label with the whitespace removed
// (a cut inside the row's trailing space is the row begun no further). The
// raw row carries SGR styling; a cut must be of what a reader sees, so the
// prefixes are taken of it stripped.
func mcpServersSubmitRowCuts(row string) []mcpServersSubmitCut {
	visible := string(visibleTextOf(row))
	var cuts []mcpServersSubmitCut
	for _, variant := range []struct {
		whole  string
		cursor bool
	}{{visible, false}, {mcpServersSubmitCursorRow(visible), true}} {
		cuts = append(cuts, mcpServersSubmitCut{variant.whole, variant.cursor})
		seen := map[string]bool{}
		for i := range variant.whole {
			if i == 0 || !utf8.RuneStart(variant.whole[i]) {
				continue
			}
			cut := variant.whole[:i]
			label := claudeSquash(claudeTrustRowOf(cut).label)
			if label == "" || seen[label] {
				continue
			}
			seen[label] = true
			cuts = append(cuts, mcpServersSubmitCut{cut, variant.cursor})
		}
	}
	return cuts
}

// mcpServersSubmitCursorRow is the picker's "Enable selected" row with the
// ❯ cursor on it, in the columns Claude Code gives it: two columns before
// the label's, the way its server rows carry it ("  ❯ [✔] probe").
func mcpServersSubmitCursorRow(row string) string {
	label := strings.IndexFunc(row, func(r rune) bool { return r != ' ' })
	if label < 2 {
		return claudeTrustSelectionGlyph + " " + strings.TrimSpace(row)
	}
	return row[:label-2] + claudeTrustSelectionGlyph + " " + row[label:]
}

// mcpServersUncursored returns pane with the ❯ cursor taken off its
// checkbox row: a capture with the cursor on the submit row shows no other
// ❯, so a frame built with one owes the server rows back their plain form.
func mcpServersUncursored(pane []string) []string {
	out := make([]string, len(pane))
	for i, row := range pane {
		out[i] = row
		visible := string(visibleTextOf(row))
		if r := claudeTrustRowOf(visible); r.selected && claudeMCPServerCheckboxRow(r.label) {
			out[i] = strings.Replace(visible, claudeTrustSelectionGlyph+" ", "  ", 1)
		}
	}
	return out
}

// mcpServersSubmitRowOf is the index of the picker's "Enable selected" row
// in rows, which run down to the picker's footer.
func mcpServersSubmitRowOf(t *testing.T, rows []string, fixture string) int {
	t.Helper()
	for i := len(rows) - 2; i >= 0; i-- {
		if claudeTrustRowOf(string(visibleTextOf(rows[i]))).label == claudeMCPServersSubmitLabel {
			return i
		}
	}
	require.FailNow(t, "the fixture shows no submit row", "%s", fixture)
	return -1
}

// Every real capture of the picker, on a pane whose top edge has scrolled
// its header row off — the header's first row no longer on it — holds when
// its "Enable selected" row is the pane's last content: whole, begun at any
// point of the row, or behind the ❯ cursor, with nothing but blank rows
// below it. Each of these read clear on master: no clause read the picker
// before its footer began without the header to anchor on.
func TestCheckAndHandleTrustPrompt_Issue7873PickerBelowItsHeaderHoldsBeforeTheFooter(t *testing.T) {
	_, _, errLog := captureTrustPromptLogs(t)
	for _, fixture := range mcpServersPickerCaptures {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		submit := mcpServersSubmitRowOf(t, rows, fixture)
		cuts := mcpServersSubmitRowCuts(rows[submit])
		for top := header + 1; top <= submit; top++ {
			pane := rows[top:submit]
			t.Run(fmt.Sprintf("%s, from its row %d", fixture, top+1), func(t *testing.T) {
				for _, cut := range cuts {
					above := pane
					if cut.cursor {
						above = mcpServersUncursored(pane)
					}
					for _, trail := range []int{0, 1} {
						content := strings.Join(append(append([]string{}, above...), cut.last), "\n") + strings.Repeat("\n", trail)
						requireHeldWithoutAKey(t, content, fmt.Sprintf(
							"its submit row painted as %q, %d blank rows below: the pane holds over the footer's paint the instant after, and this is the instant before (#7873)",
							cut.last, trail))
						require.Equal(t, DialogHoldMCPPartial, claudeDialogHoldKind(claudeScreenOf(content)),
							"the pre-footer hold is a partial hold, the begun-footer read's kind")
					}
				}
			})
		}
	}
	require.Zero(t, strings.Count(errLog.String(), "refusing to answer"),
		"a pre-footer frame holds silently, as painting, the way a begun footer does")
}

// The same cuts with the header still on screen are controls: each already
// held on master, under the grammar's partial or the painting clause, and
// still holds — the tail reader's new clause never runs on them.
func TestCheckAndHandleTrustPrompt_Issue7873PickerWithItsHeaderHoldsBeforeTheFooter(t *testing.T) {
	for _, fixture := range mcpServersPickerCaptures {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		submit := mcpServersSubmitRowOf(t, rows, fixture)
		cuts := mcpServersSubmitRowCuts(rows[submit])
		for top := 0; top <= header; top++ {
			pane := rows[top:submit]
			t.Run(fmt.Sprintf("%s, from its row %d", fixture, top+1), func(t *testing.T) {
				for _, cut := range cuts {
					above := pane
					if cut.cursor {
						above = mcpServersUncursored(pane)
					}
					requireHeldWithoutAKey(t, strings.Join(append(append([]string{}, above...), cut.last), "\n"),
						fmt.Sprintf("its submit row painted as %q, already held on master (#7873)", cut.last))
				}
			})
		}
	}
}

// A checklist that ends in the submit row's own words is the picker as far
// as any read of the pane can tell: "[✔] done" and "[ ] todo" ARE its
// checkbox rows, and "Enable selected" alone IS its submit row. It holds —
// the same way the whole tail reader holds a picker af cannot tell from the
// real one: the create's trust budget running out is loud and can be
// retried, an Enter into a live picker is neither.
func TestCheckAndHandleTrustPrompt_Issue7873ChecklistEndingInTheSubmitRowsWordsHolds(t *testing.T) {
	requireHeldWithoutAKey(t,
		"⏺ Steps:\n  [✔] done\n  [ ] todo\nEnable selected",
		"a checklist ending in \"Enable selected\" is indistinguishable from the picker mid-paint (#7873)")
}

// Ordinary launch and idle frames stay clear: a bare "Enable selected" is
// the picker's submit row only when the row straight above it is one the
// picker paints there — a checkbox row or the wrapped rest of a server name
// — or when nothing is above it at all, and only while nothing ordinary is
// painted below it. A selected row that is not a checkbox — a composer's
// prompt or another picker's cursor — is the one row the picker never
// paints between its servers and its submit row.
func TestCheckAndHandleTrustPrompt_Issue7873OrdinaryFramesStayClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"a real 2.1.328 idle composer at 80 columns", readTrustFixture(t, "claude_2_1_328_idle_composer_80x24.ansi")},
		{"a real 2.1.322 composer after an MCP decline", readTrustFixture(t, mcpServerComposerFixture)},
		{"a real 2.1.322 composer after rejecting all servers", readTrustFixture(t, mcpServersRejectedFixture)},
		{"a prompt echo and its output", "❯ fix the bug\n\n● Fixed it." + ruledComposerBelow},
		{"a composer's bare prompt row above a bare submit row", "❯ \n       Enable selected"},
		{"a composer's draft above a bare submit row", "❯ paste: it said Enable\n       Enable selected"},
		{"a checklist in an agent's output under a line beginning Enable",
			"⏺ Steps:\n  [✔] done\n  [ ] todo\nEnable the new flow"},
		{"the submit row's words quoted in prose under a checklist",
			"⏺ Steps:\n  [✔] done\n  [ ] todo\nIt showed \"Enable selected\" and I stopped"},
		{"a status line below the submit row",
			"  [✔] probe\n       Enable selected\n\n  ? for shortcuts"},
		{"a composer rule below the submit row",
			"  [✔] probe\n       Enable selected\n" + strings.Repeat("─", 40)},
	} {
		requireClearWithoutAKey(t, tt.content, fmt.Sprintf(
			"%s: an ordinary launch or idle frame is no picker, and the create types into it (#7873)", tt.name))
	}
}
