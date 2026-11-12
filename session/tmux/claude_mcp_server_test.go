package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// Regression tests for #7163.
//
// Claude Code 2.1.322 reworded its single-server MCP approval dialog. None of
// the older wording's anchors survived ("Do you trust this new MCP server?",
// the Yes/No rows), so af recognized nothing: CheckAndHandleTrustPrompt
// reported the pane clear and the create path typed the user's prompt into the
// picker.
//
// The fixtures under testdata/ are real captures of claude 2.1.322 taken with
// af's own capture flags (-p -e -J) in a fresh worktree whose .mcp.json names
// one server, in a throwaway HOME and CLAUDE_CONFIG_DIR, after af's folder-trust
// answer. The cursor-on-all-future capture is the same dialog after one Up; the
// composer capture is what the pane shows once Enter confirms the preselection.
//
// The labels are spelled out here rather than taken from the implementation, so
// these tests state the dialog's wording independently of the code under test.
const (
	mcpServerUseLabel     = "Use this MCP server"
	mcpServerUseAllLabel  = "Use this and all future MCP servers in this project"
	mcpServerDeclineLabel = "Continue without using this MCP server"
)

var mcpServerOptions = []string{mcpServerUseLabel, mcpServerUseAllLabel, mcpServerDeclineLabel}

func readTrustFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

const (
	mcpServerDialogFixture         = "claude_2_1_322_mcp_server_dialog.ansi"
	mcpServerDialogWideFixture     = "claude_2_1_322_mcp_server_dialog_wide.ansi"
	mcpServerDialogOnAllFixture    = "claude_2_1_322_mcp_server_dialog_cursor_on_all_future.ansi"
	mcpServerComposerFixture       = "claude_2_1_322_composer_after_mcp_decline.ansi"
	mcpServerMultiServerFixture    = "claude_2_1_322_mcp_multi_server_dialog.ansi"
	mcpServerCapturedPreselection  = mcpServerDeclineLabel
	mcpServerCapturedAfterOneUpRow = mcpServerUseAllLabel
)

// plainMCPServerFrame is the 80-column capture with its SGR styling stripped
// and the cursor moved onto selected. Only the cursor column changes, so every
// other row is exactly what claude 2.1.322 painted.
func plainMCPServerFrame(t *testing.T, selected string) string {
	t.Helper()
	plain := ansiCSISequence.ReplaceAllString(readTrustFixture(t, mcpServerDialogFixture), "")
	lines := strings.Split(plain, "\n")
	moved := 0
	for i, line := range lines {
		label := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), claudeTrustSelectionGlyph))
		for _, option := range mcpServerOptions {
			if label != option {
				continue
			}
			moved++
			if option == selected {
				lines[i] = "  " + claudeTrustSelectionGlyph + " " + option
			} else {
				lines[i] = "    " + option
			}
		}
	}
	require.Equal(t, len(mcpServerOptions), moved, "the fixture must paint every option row exactly once")
	return strings.Join(lines, "\n")
}

// mcpServerPane models the 2.1.322 picker as a live dialog: Down/Up move the
// cursor (wrapping, as Claude Code's pickers do), and Enter COMMITS the label
// under it. The committed label is the oracle — "Use this MCP server" and the
// all-future row are both a grant af must never make.
type mcpServerPane struct {
	mu        sync.Mutex
	selected  int
	committed []string
	// frames is what the pane shows for a given selected label. A label without
	// an entry is rendered from the real capture with the cursor moved.
	frames   map[string]string
	composer string
	t        *testing.T
}

func newMCPServerPane(t *testing.T, preselected string, frames map[string]string) *mcpServerPane {
	t.Helper()
	idx := -1
	for i, option := range mcpServerOptions {
		if option == preselected {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0)
	return &mcpServerPane{
		selected: idx,
		frames:   frames,
		composer: readTrustFixture(t, mcpServerComposerFixture),
		t:        t,
	}
}

func (p *mcpServerPane) capture() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.committed) > 0 {
		return p.composer
	}
	label := mcpServerOptions[p.selected]
	if frame, ok := p.frames[label]; ok {
		return frame
	}
	return plainMCPServerFrame(p.t, label)
}

func (p *mcpServerPane) key(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.committed) > 0 {
		return
	}
	switch name {
	case "Down":
		p.selected = (p.selected + 1) % len(mcpServerOptions)
	case "Up":
		p.selected = (p.selected - 1 + len(mcpServerOptions)) % len(mcpServerOptions)
	case "Enter":
		p.committed = append(p.committed, mcpServerOptions[p.selected])
	}
}

func (p *mcpServerPane) committedLabels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.committed...)
}

// mcpServerSession binds a Claude session to the live picker and returns the
// send-keys commands it issues.
func mcpServerSession(t *testing.T, pane *mcpServerPane) (*TmuxSession, *[]string) {
	t.Helper()
	keys := &[]string{}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "send-keys") {
				return nil
			}
			*keys = append(*keys, joined)
			for _, name := range injectedKeyNames([]string{joined}) {
				pane.key(name)
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return []byte("0 0 0"), nil
			}
			return []byte(pane.capture()), nil
		},
	}
	return newTmuxSession(toTmuxName("mcp", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec), keys
}

// The property #7163 names: af answers the reworded dialog, and the row it
// confirms is the decline row Claude Code preselected — checked by WHICH label
// the cursor was on when Enter arrived, against the real captures at both
// widths. On master the dialog is not recognized at all: the check reports the
// pane clear, nothing is committed, and the create types into the picker.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServerDialogConfirmsTheDeclineRow(t *testing.T) {
	for _, tt := range []struct{ name, fixture string }{
		{"80 columns", mcpServerDialogFixture},
		{"160 columns", mcpServerDialogWideFixture},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pane := newMCPServerPane(t, mcpServerCapturedPreselection, map[string]string{
				mcpServerDeclineLabel: readTrustFixture(t, tt.fixture),
			})
			session, keys := mcpServerSession(t, pane)

			require.True(t, session.CheckAndHandleTrustPrompt(),
				"the captured 2.1.322 MCP-server dialog is in the way of the composer, so the pane is not clear")
			require.Equal(t, []string{mcpServerDeclineLabel}, pane.committedLabels(),
				"af must confirm exactly the preselected decline row; keystrokes were %v", *keys)
			require.Equal(t, []string{"Enter"}, injectedKeyNames(*keys),
				"accepting the preselection is one Enter and no movement key; got %v", *keys)

			keystroke, _, ok := session.recentDialogKeystroke()
			require.True(t, ok, "the Enter must be recorded so a death right after it is attributed to the dialog")
			require.Equal(t, mcpServerDeclineLabel, keystroke.choice,
				"the diagnostic must name the option af confirmed")

			require.False(t, session.CheckAndHandleTrustPrompt(),
				"once the dialog is answered the composer is on screen and the pane is clear")
			require.Equal(t, []string{"Enter"}, injectedKeyNames(*keys), "nothing more is typed after the dialog closes")
		})
	}
}

// The other half of the contract: af accepts only the decline preselection.
// When either "Use" row owns the cursor — in the real capture, after one Up —
// af must send nothing at all, keep reporting the dialog in the way, and say
// why once rather than on every poll. Moving the cursor itself is not af's call
// either: the contract is Claude Code's preselection or nothing.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServerDialogRefusesAnyOtherSelection(t *testing.T) {
	for _, tt := range []struct {
		name     string
		selected string
		frames   map[string]string
	}{
		{"all-future row selected (real capture)", mcpServerCapturedAfterOneUpRow, map[string]string{
			mcpServerUseAllLabel: readTrustFixture(t, mcpServerDialogOnAllFixture),
		}},
		{"use-this-server row selected", mcpServerUseLabel, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			pane := newMCPServerPane(t, tt.selected, tt.frames)
			session, keys := mcpServerSession(t, pane)

			for poll := 0; poll < 4; poll++ {
				require.True(t, session.CheckAndHandleTrustPrompt(),
					"poll %d: a dialog af refuses is still in the way; reporting the pane clear would type the prompt into it", poll)
			}
			require.Empty(t, *keys, "af must not press any key when the decline row is not the one selected")
			require.Empty(t, pane.committedLabels())
			require.Contains(t, errLog.String(), mcpServerDeclineLabel,
				"the refusal must name the only option af will confirm")
			require.Contains(t, errLog.String(), tt.selected, "the refusal must name what was selected instead")
			require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer"),
				"the refusal is reported once per dialog, not once per poll")
		})
	}
}

// Look, then look again before the irreversible key. If the frame af confirms
// against is not the frame it decided on — the cursor moved off the decline
// row, or the dialog closed and the composer is back — no Enter is sent.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServerDialogIsReReadBeforeEnter(t *testing.T) {
	for _, tt := range []struct{ name, second string }{
		{"cursor moved to the all-future row", mcpServerDialogOnAllFixture},
		{"dialog closed underneath af", mcpServerComposerFixture},
	} {
		t.Run(tt.name, func(t *testing.T) {
			frames := []string{readTrustFixture(t, mcpServerDialogFixture), readTrustFixture(t, tt.second)}
			var cmds []string
			cmdExec := cmd_test.MockCmdExec{
				RunFunc: func(c *exec.Cmd) error {
					cmds = append(cmds, strings.Join(c.Args, " "))
					return nil
				},
				OutputFunc: func(c *exec.Cmd) ([]byte, error) {
					joined := strings.Join(c.Args, " ")
					if strings.Contains(joined, "display-message") {
						return []byte("0 0 0"), nil
					}
					frame := frames[0]
					if strings.Contains(joined, "capture-pane") && len(frames) > 1 {
						frames = frames[1:]
					}
					return []byte(frame), nil
				},
			}
			session := newTmuxSession(toTmuxName("mcp", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec)
			require.True(t, session.CheckAndHandleTrustPrompt(),
				"the dialog was positively identified on the first capture, so the pane is not reported clear")
			require.Empty(t, sentKeystrokes(cmds), "the confirming capture did not show the decline row selected; got %v", cmds)
		})
	}
}

// Claude Code paints this dialog top-down, like the older MCP prompt. A frame
// that is a proper prefix of it — cut from the real capture — must hold: "I
// could not confirm the dialog" and "there is no dialog" are different answers,
// and only the second lets the caller type into the pane.
func TestCheckAndHandleTrustPrompt_PartiallyRenderedClaudeMCPServerDialogHolds(t *testing.T) {
	full := strings.Split(plainMCPServerFrame(t, mcpServerDeclineLabel), "\n")
	rowOf := func(want string) int {
		for i, line := range full {
			if strings.Contains(line, want) {
				return i
			}
		}
		t.Fatalf("the fixture has no row containing %q", want)
		return -1
	}
	upTo := func(row int) string { return strings.Join(full[:row+1], "\n") + "\n\n\n" }

	for _, tt := range []struct{ name, content string }{
		{"preamble only", upTo(rowOf("New MCP server found in this project"))},
		{"preamble and body", upTo(rowOf("require approval"))},
		{"first option", upTo(rowOf(mcpServerUseLabel))},
		{"two options", upTo(rowOf(mcpServerUseAllLabel))},
		{"every option, no footer yet", upTo(rowOf(mcpServerDeclineLabel))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handled, cmds := pollStaticPane(t, tt.content, 4)
			require.True(t, handled, "a partially painted MCP-server dialog must hold, not report the pane clear")
			require.Empty(t, sentKeystrokes(cmds), "a partially painted dialog receives no key; got %v", cmds)
		})
	}
}

// The structural guards the older wording relies on (#4254, #4735) hold for the
// new one: a working agent paints its composer below its output, so the dialog
// quoted in a transcript is never the last thing on screen and is never
// answered. (The multi-server picker Claude Code shows for two or more servers
// is a different dialog; claude_mcp_multiselect_test.go covers it, #7166.)
func TestCheckAndHandleTrustPrompt_QuotedClaudeMCPServerDialogInjectsNothing(t *testing.T) {
	dialog := readTrustFixture(t, mcpServerDialogFixture)
	composer := readTrustFixture(t, mcpServerComposerFixture)
	for _, tt := range []struct{ name, content string }{
		{"the captured dialog above the agent's composer", strings.TrimRight(dialog, "\n") + "\n" + composer},
		{"the captured dialog with trailing prose", strings.TrimRight(dialog, "\n") + "\nThat is the dialog af answers at startup.\n"},
		{"the decline label as a composer draft under the preamble", "  New MCP server found in this project: probe\n\n" +
			strings.Repeat("─", 80) + "\n" + claudeTrustSelectionGlyph + " " + mcpServerDeclineLabel + "\n" + strings.Repeat("─", 80) + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, cmds := pollStaticPane(t, tt.content, 4)
			require.Empty(t, sentKeystrokes(cmds), "no key may be injected into a pane that only quotes the dialog; got %v", cmds)
		})
	}
}
