package tmux

import (
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// Regression tests for #7166.
//
// With two or more servers in .mcp.json, Claude Code 2.1.322 shows a
// multi-select picker with every server PRE-CHECKED. af recognized none of it,
// reported the pane clear, and the create typed the prompt into the picker —
// measured, the prompt was lost and its Enter unchecked the first server.
//
// The property (decided on #7166): af never enables an MCP server for the
// user. It recognizes the picker structurally and answers it with Esc — the
// picker's own "reject all" — and never Space or Enter. After the Esc it
// re-reads the pane; a picker still there is refused, never sent a second key.
//
// The fixtures are real captures of claude 2.1.322 with two and three servers,
// at 80 and 160 columns, taken like the single-server ones (see
// claude_mcp_server_test.go), plus the composer the pane shows after Esc.
const (
	mcpServersTwoFixture       = mcpServerMultiServerFixture
	mcpServersThreeFixture     = "claude_2_1_322_mcp_multi_server_dialog_three.ansi"
	mcpServersThreeWideFixture = "claude_2_1_322_mcp_multi_server_dialog_three_wide.ansi"
	mcpServersRejectedFixture  = "claude_2_1_322_composer_after_mcp_reject_all.ansi"
)

// mcpServersPane models the picker as a live dialog: Escape rejects every
// server and closes it; Space toggles a server; Enter toggles the cursor row.
// The oracle is what the picker was SENT — af must send it Escape and nothing
// else — and whether it closed with nothing enabled.
type mcpServersPane struct {
	mu      sync.Mutex
	picker  string
	after   string
	closed  bool
	ignores bool // the picker never reacts (af must refuse rather than resend)
	keys    []string
}

func (p *mcpServersPane) capture() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.after
	}
	return p.picker
}

func (p *mcpServersPane) key(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, name)
	if name == "Escape" && !p.closed && !p.ignores {
		p.closed = true
	}
}

func (p *mcpServersPane) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.keys...)
}

func mcpServersSession(t *testing.T, pane *mcpServersPane) *TmuxSession {
	t.Helper()
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "send-keys") {
				for _, name := range injectedKeyNames([]string{joined}) {
					pane.key(name)
				}
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
	return newTmuxSession(toTmuxName("mcps", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec)
}

// pollMCPServers runs CheckAndHandleTrustPrompt one daemon poll apart, the way
// task.DismissTrustPrompt does, and returns each poll's verdict.
func pollMCPServers(t *testing.T, session *TmuxSession, polls int) []bool {
	t.Helper()
	advance := setClaudeTrustClock(t)
	var verdicts []bool
	for i := 0; i < polls; i++ {
		advance(time.Second)
		verdicts = append(verdicts, session.CheckAndHandleTrustPrompt())
	}
	return verdicts
}

// The property: af answers the picker with exactly one Esc — never Space, never
// Enter, never the prompt — and only after the picker has had the settle delay
// to start reading input. Once it closes, the pane is clear. On the base branch
// the picker is not recognized at all: no key, the pane reported clear on the
// first poll, and the create types into it.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServersPickerRejectsAllWithEsc(t *testing.T) {
	for _, tt := range []struct{ name, fixture string }{
		{"two servers, 80 columns", mcpServersTwoFixture},
		{"three servers, 80 columns", mcpServersThreeFixture},
		{"three servers, 160 columns", mcpServersThreeWideFixture},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pane := &mcpServersPane{
				picker: readTrustFixture(t, tt.fixture),
				after:  readTrustFixture(t, mcpServersRejectedFixture),
			}
			session := mcpServersSession(t, pane)

			verdicts := pollMCPServers(t, session, 3)
			require.Equal(t, []bool{true, true, false}, verdicts,
				"the picker is in the way until Esc closes it; then the composer is clear")
			require.Equal(t, []string{"Escape"}, pane.sent(),
				"the only key the picker may receive is one Esc — never Space or Enter")
			require.True(t, pane.closed, "Esc must reach the picker and reject every server")

			keystroke, _, ok := session.recentDialogKeystroke()
			require.True(t, ok, "the Esc must be recorded so a death right after it is attributed to the picker")
			require.Contains(t, keystroke.choice, "/mcp", "the diagnostic must say where the servers can be enabled later")
			err := session.sessionGoneError("capture-pane", errors.New("exit status 1"))
			require.ErrorIs(t, err, ErrSessionGone)
			require.Contains(t, err.Error(), "after af answered its", "Esc commits this picker, so af did answer it")
			require.Contains(t, err.Error(), "/mcp")
		})
	}
}

// The first sight of the picker gets no key: Claude Code drops keys sent while
// it is still painting a picker, and af sends this one only one Esc.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServersPickerWaitsToSettle(t *testing.T) {
	pane := &mcpServersPane{picker: readTrustFixture(t, mcpServersThreeFixture)}
	session := mcpServersSession(t, pane)
	setClaudeTrustClock(t)
	require.True(t, session.CheckAndHandleTrustPrompt(), "the picker is in the way")
	require.Empty(t, pane.sent(), "no key on the poll that first sees the picker")
}

// After its one Esc, af re-reads the pane. A picker still on screen is refused
// — reported once, and never sent a second Esc, Space or Enter, however many
// polls follow. A second Esc that reached the composer would be Claude Code's
// interrupt and rewind key.
func TestCheckAndHandleTrustPrompt_ClaudeMCPServersPickerStillThereAfterEscIsRefused(t *testing.T) {
	_, _, errLog := captureTrustPromptLogs(t)
	pane := &mcpServersPane{picker: readTrustFixture(t, mcpServersTwoFixture), ignores: true}
	session := mcpServersSession(t, pane)

	for poll, handled := range pollMCPServers(t, session, 6) {
		require.True(t, handled, "poll %d: the picker is still in the way", poll)
	}
	require.Equal(t, []string{"Escape"}, pane.sent(), "exactly one Esc, ever, and nothing else")
	require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer"), "the refusal is reported once")
	require.Contains(t, errLog.String(), "/mcp", "the refusal must say where servers are enabled later")
}

// The picker is painted top-down like the other launch dialogs. A frame that is
// a proper prefix of it — cut from the real capture — holds without a key.
func TestCheckAndHandleTrustPrompt_PartiallyRenderedClaudeMCPServersPickerHolds(t *testing.T) {
	full := strings.Split(ansiCSISequence.ReplaceAllString(readTrustFixture(t, mcpServersThreeFixture), ""), "\n")
	rowOf := func(want string) int {
		for i, line := range full {
			if strings.Contains(line, want) {
				return i
			}
		}
		t.Fatalf("the fixture has no row containing %q", want)
		return -1
	}
	for _, tt := range []struct {
		name string
		last int
	}{
		{"header only", rowOf("3 new MCP servers found in this project")},
		{"header and body", rowOf("require approval")},
		{"first server", rowOf("[✔] probe")},
		{"every server, no submit row", rowOf("probe-three")},
		{"submit row, no footer", rowOf("Enable selected")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Join(full[:tt.last+1], "\n") + "\n\n\n"
			handled, cmds := pollStaticPane(t, content, 4)
			require.True(t, handled, "a partially painted picker must hold, not report the pane clear")
			require.Empty(t, sentKeystrokes(cmds), "a partially painted picker receives no key; got %v", cmds)
		})
	}
}

// The picker quoted in output — with the agent's composer, or anything else,
// painted below its footer — is not the picker, and gets no Esc. An Esc typed
// into a working Claude pane interrupts the agent.
func TestCheckAndHandleTrustPrompt_QuotedClaudeMCPServersPickerInjectsNothing(t *testing.T) {
	picker := readTrustFixture(t, mcpServersThreeFixture)
	composer := readTrustFixture(t, mcpServersRejectedFixture)
	// The picker pasted into a multi-line composer draft: the composer's own ❯
	// line sits above the pasted header, so the pane has two ❯ rows.
	rule := strings.Repeat("─", 80)
	var draft strings.Builder
	draft.WriteString(rule + "\n" + claudeTrustSelectionGlyph + " why does this picker check every server?\n")
	for _, line := range strings.Split(ansiCSISequence.ReplaceAllString(picker, ""), "\n") {
		if text := strings.TrimSpace(line); text != "" && strings.Trim(text, "─") != "" {
			draft.WriteString("  " + line + "\n")
		}
	}
	draft.WriteString(rule + "\n")
	for _, tt := range []struct{ name, content string }{
		{"the picker pasted into a multi-line composer draft", draft.String()},
		{"the captured picker above the agent's composer", strings.TrimRight(picker, "\n") + "\n" + composer},
		{"the captured picker with trailing prose", strings.TrimRight(picker, "\n") + "\nThat is the picker af rejects.\n"},
		{"the header quoted above the composer", "  2 new MCP servers found in this project\n" + composer},
		{"a one-server count is not this picker", strings.Replace(ansiCSISequence.ReplaceAllString(picker, ""), "3 new MCP servers", "1 new MCP servers", 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, cmds := pollStaticPane(t, tt.content, 4)
			require.Empty(t, sentKeystrokes(cmds), "no key may be injected into a pane that only quotes the picker; got %v", cmds)
		})
	}
}
