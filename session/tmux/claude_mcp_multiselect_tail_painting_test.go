package tmux

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Regression tests for #7870. On a pane too short to keep the multi-server
// picker's header row on screen, the picker read clear while its footer was
// still painting: claudeMCPServersTailOf held only the frame that showed the
// footer whole, and claudeMCPServersPaintingOf, the clause after it, anchored
// on the header row. The frame #7870 filed — rows 4–15 of the 40-column
// capture over " Space to select" — read clear at every one of the footer's
// 27 proper prefixes, and the create typed its prompt into the picker, where
// Enter confirmed whichever row owned the cursor, pre-checked servers that
// run code among them.
//
// A pane that holds over the picker's whole footer now holds over every
// proper prefix of it that begins a row, at every top cut — including the
// cuts that scroll the header row off — because the tail reader reads a begun
// footer the way it reads a whole one: the same rows above it, the same ask
// that nothing be painted below it.

// The frame #7870 filed: rows 4–15 of the 40-column capture — the wrapped
// header's second row down to "Enable selected" — over " Space to select",
// the footer painted only that far.
func TestCheckAndHandleTrustPrompt_Issue7870PickerPaintingWithItsHeaderOffHolds(t *testing.T) {
	frame := strings.Join([]string{
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
		"       Enable selected",
		" Space to select",
	}, "\n")
	requireHeldWithoutAKey(t, frame, "rows 4-15 of the 40-column capture over \" Space to select\" (#7870)")
}

// Every real capture of the picker, on a pane whose top edge has scrolled its
// header row off — the header's first row no longer on it — holds at every
// proper prefix of its footer that begins a row, with the footer on the
// pane's last row or one blank row under it. Each of these cuts read clear on
// master: no clause read the begun footer without the header to anchor on.
func TestCheckAndHandleTrustPrompt_Issue7870PickerBelowItsHeaderHoldsAtEveryFooterPrefix(t *testing.T) {
	footers := []string{claudeSquash(claudeMCPServersFooter)}
	for _, fixture := range mcpServersPickerCaptures {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		for top := header + 1; top < len(rows); top++ {
			pane := strings.Join(rows[top:], "\n")
			t.Run(fmt.Sprintf("%s, from its row %d", fixture, top+1), func(t *testing.T) {
				cuts := claudeLaunchDialogFooterPrefixCuts(t, pane, footers)
				require.NotEmpty(t, cuts)
				for _, cut := range cuts {
					for _, trail := range []int{0, 1} {
						content := cut.content + strings.Repeat("\n", trail)
						requireHeldWithoutAKey(t, content, fmt.Sprintf(
							"its footer painted only as far as %q, %d blank rows below: the pane holds over the whole footer, and the prefix is the same footer still painting (#7870)",
							cut.prefix, trail))
					}
				}
			})
		}
	}
}

// The same cuts with the header still on screen are controls: each already
// held on master, under the grammar's partial or the painting clause, and
// still holds — through the tail reader now, before those clauses run.
func TestCheckAndHandleTrustPrompt_Issue7870PickerWithItsHeaderHoldsAtEveryFooterPrefix(t *testing.T) {
	footers := []string{claudeSquash(claudeMCPServersFooter)}
	for _, fixture := range mcpServersPickerCaptures {
		rows, header := mcpServersPickerRowsOf(t, fixture)
		for top := 0; top <= header; top++ {
			pane := strings.Join(rows[top:], "\n")
			t.Run(fmt.Sprintf("%s, from its row %d", fixture, top+1), func(t *testing.T) {
				cuts := claudeLaunchDialogFooterPrefixCuts(t, pane, footers)
				require.NotEmpty(t, cuts)
				for _, cut := range cuts {
					requireHeldWithoutAKey(t, cut.content, fmt.Sprintf(
						"its footer painted only as far as %q (#7870)", cut.prefix))
				}
			})
		}
	}
}

// Ordinary launch and idle frames stay clear: a begun footer is the picker's
// only when the content straight above it is "Enable selected" — or the pane
// shows nothing above it — and nothing is painted below it.
func TestCheckAndHandleTrustPrompt_Issue7870OrdinaryFramesStayClear(t *testing.T) {
	for _, tt := range []struct{ name, content string }{
		{"a real 2.1.328 idle composer at 80 columns", readTrustFixture(t, "claude_2_1_328_idle_composer_80x24.ansi")},
		{"a real 2.1.322 composer after an MCP decline", readTrustFixture(t, mcpServerComposerFixture)},
		{"a real 2.1.322 composer after rejecting all servers", readTrustFixture(t, mcpServersRejectedFixture)},
		{"a prompt echo and its output", "❯ fix the bug\n\n● Fixed it." + ruledComposerBelow},
		{"a checklist in an agent's output over the footer's start",
			"⏺ Steps:\n  [✔] done\n  [✔] wrote the fix\n Space to select"},
		{"a checklist row that ends in the submit row's words over the footer's start",
			"  [✔] done\n  [✔] Enable selected\n Space to select"},
		{"the picker's submit row and begun footer quoted above a composer",
			"● The picker ends with:\n\n       Enable selected\n Space to select" + ruledComposerBelow},
		{"a status line below the footer's start",
			"       Enable selected\n Space to select\n\n  ? for shortcuts"},
	} {
		requireClearWithoutAKey(t, tt.content, fmt.Sprintf(
			"%s: an ordinary launch or idle frame is no picker, and the create types into it (#7870)", tt.name))
	}
}
