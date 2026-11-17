package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7463. af read the 2.1.322 multi-server picker only from
// its header down. On a pane too short for the whole picker, the header scrolls
// off; on one narrower than its footer, the footer wraps. Either way the pane
// read clear, and the create typed its prompt and pressed Enter into a picker
// that has every server pre-checked, which enables every server. The picker now
// holds, with no key, whenever its own trailing rows are the pane's last
// content: its footer, under its "Enable selected" row.

// The frame #7463 filed: the six-server capture on a pane 12 rows tall.
func TestCheckAndHandleTrustPrompt_Issue7463PickerWithItsHeaderScrolledOffHolds(t *testing.T) {
	_, _, errLog := captureTrustPromptLogs(t)
	frame := strings.Join([]string{
		"  Select any you wish to enable.",
		"",
		"  MCP servers may execute code or access system resources. All tool calls",
		"  require approval. Learn more in the MCP documentation.",
		"",
		"  ❯ [✔] probe-1",
		"    [✔] probe-2",
		"    [✔] probe-3",
		"    [✔] probe-4",
		"  ↓ [✔] probe-5",
		"       Enable selected",
		" Space to select · Esc to reject all",
	}, "\n")
	requireHeldWithoutAKey(t, frame, "the six-server picker on a pane 12 rows tall")
	require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer"), "the hold is reported once, not once per poll")
	require.Contains(t, errLog.String(), "header off", "the refusal says what af saw")
}

// mcpServersPickerRowsOf is fixture's rows down to the picker's footer, and the
// index of the first row of its header.
func mcpServersPickerRowsOf(t *testing.T, fixture string) (rows []string, header int) {
	t.Helper()
	rows = strings.Split(readTrustFixture(t, fixture), "\n")
	last, header := -1, -1
	for i, line := range rows {
		label := claudeTrustRowOf(string(visibleTextOf(line))).label
		if label != "" {
			last = i
		}
		if header < 0 && claudeMCPServersHeaderStart.MatchString(label) {
			header = i
		}
	}
	require.Equal(t, claudeMCPServersFooter, claudeTrustRowOf(string(visibleTextOf(rows[last]))).label, "%s ends with the picker's footer", fixture)
	require.GreaterOrEqual(t, header, 0, "%s shows the picker's header", fixture)
	return rows[:last+1], header
}

// Every real capture of the picker, on a pane too short to show its header,
// holds without a key: its last rows down to its footer, at every height from
// the footer alone to the row just below the header, with the footer on the
// pane's last row or one blank row under it. #7463 filed the six-server
// capture at 12 rows; the six-server capture scrolled to its last server and
// the 40-column capture, cropped to 8 to 12 rows, read clear the same way.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServersPickerBelowItsHeaderHolds(t *testing.T) {
	for _, fixture := range []string{
		mcpServersTwoFixture, mcpServersThreeFixture, mcpServersThreeWideFixture,
		mcpServersSixFixture, mcpServersSixScrolledFixture, mcpServers40ColumnsFixture, mcpServersLongNameFixture,
	} {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		for height := 1; height < len(rows)-header; height++ {
			t.Run(fmt.Sprintf("%s, its last %d rows", fixture, height), func(t *testing.T) {
				for _, trail := range []int{0, 1} {
					pane := append(append([]string{}, rows[len(rows)-height:]...), make([]string, trail)...)
					requireHeldWithoutAKey(t, strings.Join(pane, "\n"), fmt.Sprintf("%d blank rows below", trail))
				}
			})
		}
	}
}

// mcpServersPickerAt is the three-server picker as a pane width columns wide
// paints it, each row wrapped at its spaces and indented as the 2.1.322
// captures indent it.
func mcpServersPickerAt(width int) []string {
	rows := []string{strings.Repeat("─", width)}
	add := func(indent int, row string) {
		indent = min(indent, width-1)
		for _, piece := range wrapAtSpaces(row, width-indent) {
			rows = append(rows, strings.Repeat(" ", indent)+piece)
		}
	}
	add(2, "3 new MCP servers found in this project")
	add(2, claudeMCPServersSubtitle)
	rows = append(rows, "")
	add(2, "MCP servers may execute code or access system resources. All tool calls require approval. Learn more in the MCP documentation.")
	rows = append(rows, "")
	add(2, "❯ [✔] probe")
	add(4, "[✔] probe-two")
	add(4, "[✔] probe-three")
	add(7, claudeMCPServersSubmitLabel)
	add(1, claudeMCPServersFooter)
	return rows
}

// A pane narrower than the footer wraps it, and wraps the "Enable selected" row
// below 22 columns. The picker holds without a key whole, header and all, and
// at every height down to its whole footer. Before #7463 the whole picker read
// clear under 36 columns, where the unreadable hold no longer found its footer
// on one row.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServersPickerWithItsFooterWrappedHolds(t *testing.T) {
	for width := 12; width <= 36; width++ {
		t.Run(fmt.Sprintf("at %d columns", width), func(t *testing.T) {
			rows := mcpServersPickerAt(width)
			footerRows := len(wrapAtSpaces(claudeMCPServersFooter, width-1))
			for height := len(rows); height >= footerRows; height-- {
				requireHeldWithoutAKey(t, strings.Join(rows[len(rows)-height:], "\n"), fmt.Sprintf("its last %d rows", height))
			}
		})
	}
}

// The picker's last rows as Claude quotes them in a transcript, over a real
// 2.1.322 composer, read clear: the composer is what says no dialog is live,
// and its rules and prompt are content below the quoted footer. Cut at the
// quoted footer, before Claude paints its composer, the same screen holds
// without a key until the composer arrives. So does the picker pasted into the
// composer's draft, while its bottom rule is not yet painted; once it is, the
// pane reads clear.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServersPickerTailQuotedAboveAComposerReadsClear(t *testing.T) {
	picker := strings.Split(strings.TrimRight(string(visibleTextOf(readTrustFixture(t, mcpServersSixFixture))), "\n"), "\n")
	var logical []string
	for _, row := range picker {
		if !claudeTrustFrameRule(row) {
			logical = append(logical, strings.TrimRight(row, " "))
		}
	}
	// Its rows below the header: with the header quoted too, the cut is the
	// whole picker, which af answers with Esc as it answers the live one.
	quotes := map[string][]string{
		"its submit row and footer":   logical[len(logical)-2:],
		"its last three rows":         logical[len(logical)-3:],
		"its server rows and footer":  logical[len(logical)-7:],
		"its body down to its footer": logical[2:],
	}
	for _, fixture := range []string{mcpServerComposerFixture, mcpServersRejectedFixture} {
		screen := strings.Split(strings.TrimRight(readTrustFixture(t, fixture), "\n"), "\n")
		status := -1
		for i, line := range screen {
			if strings.Contains(line, "/effort") {
				status = i
				break
			}
		}
		require.Positive(t, status, "%s has its status row above its composer", fixture)
		for name, quote := range quotes {
			for _, how := range []string{"flush", "indented", "behind its bullet"} {
				t.Run(fmt.Sprintf("%s, %s, %s", fixture, name, how), func(t *testing.T) {
					for _, width := range []int{24, 40, 80} {
						quoted := append(append([]string{""}, quotedInTranscript(quote, how, width)...), "")
						rows := append(append(append([]string{}, screen[:status]...), quoted...), screen[status:]...)
						at := fmt.Sprintf("at %d columns", width)

						handled, cmds := pollStaticPane(t, strings.Join(rows, "\n"), 4)
						require.False(t, handled, "%s, under Claude's composer:\n%s", at, strings.Join(rows, "\n"))
						require.Empty(t, sentKeystrokes(cmds))

						if how == "behind its bullet" && len(quote) == 2 {
							// "⏺ Enable selected" is no row the picker
							// paints, so the cut owes no hold.
							continue
						}
						painting := rows[:status+len(quoted)-1]
						requireHeldWithoutAKey(t, strings.Join(painting, "\n"), at+", cut at the quoted footer")
					}
				})
			}
		}
	}

	rule := strings.Repeat("─", 80)
	draft := []string{rule, claudeTrustSelectionGlyph + " why does this picker end with"}
	for _, row := range logical[len(logical)-3:] {
		draft = append(draft, "  "+row)
	}
	painting := strings.Join(draft, "\n")
	requireHeldWithoutAKey(t, painting, "the picker's last rows pasted into a composer draft, its bottom rule not yet painted")
	settled := painting + "\n" + rule + "\n  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents"
	handled, cmds := pollStaticPane(t, settled, 4)
	require.False(t, handled, "the picker's last rows in a composer draft, under its bottom rule:\n%s", settled)
	require.Empty(t, sentKeystrokes(cmds))
}
