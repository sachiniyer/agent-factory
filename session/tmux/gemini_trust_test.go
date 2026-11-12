package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// Regression tests for #7162.
//
// Gemini CLI 0.51.0 shows a folder-trust dialog in every untrusted folder. af
// recognized none of it: CheckAndHandleTrustPrompt's Gemini branch knew only the
// aider-style doc-trust prompt, so it reported the pane clear, and the create
// typed the user's prompt into the dialog — whose picker commits an option on a
// digit and moves on j/k. Measured: "fix the 2 failing tests" recorded
// TRUST_PARENT for the worktree's parent directory.
//
// The fixtures under testdata/ are real captures of gemini-cli 0.51.0 taken with
// af's own capture flags (-p -e -J): a throwaway HOME and GEMINI_CLI_HOME, a
// placeholder API key, a fresh untrusted git worktree, a private tmux server.
// Paths are scrubbed to /home/dev/af-trees/wt at equal width so the box still
// lines up.
//
// The labels are spelled out here rather than taken from the implementation, so
// these tests state the dialog's wording independently of the code under test.
const (
	geminiTestTrustFolder = "Trust folder (wt)"
	geminiTestTrustParent = "Trust parent folder (af-trees)"
	geminiTestDontTrust   = "Don't trust"
)

var geminiTestOptions = []string{geminiTestTrustFolder, geminiTestTrustParent, geminiTestDontTrust}

func geminiTrustFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

// geminiTrustFrameWithCursor is a real capture with its SGR styling stripped and
// the ● indicator moved onto selected (an index into geminiTestOptions, or -1
// for no cursor at all). Only the indicator column changes.
func geminiTrustFrameWithCursor(t *testing.T, fixture string, selected ...int) string {
	t.Helper()
	plain := ansiCSISequence.ReplaceAllString(geminiTrustFixture(t, fixture), "")
	lines := strings.Split(plain, "\n")
	found := 0
	for i, line := range lines {
		for idx, option := range geminiTestOptions {
			if !strings.Contains(line, option) {
				continue
			}
			border := strings.Index(line, "│ ")
			require.GreaterOrEqual(t, border, 0, "option row %q is not inside the dialog's box", line)
			head, rest := line[:border+len("│ ")], line[border+len("│ "):]
			rest = strings.TrimPrefix(strings.TrimPrefix(rest, "● "), "  ")
			indicator := "  "
			for _, s := range selected {
				if s == idx {
					indicator = "● "
				}
			}
			lines[i] = head + indicator + rest
			found++
		}
	}
	require.Equal(t, len(geminiTestOptions), found, "the fixture must paint every option row exactly once")
	return strings.Join(lines, "\n")
}

// geminiTrustPane models the dialog as a live picker: Down/Up move the ●
// (wrapping, as Gemini's selection list does), Enter commits the option under
// it, and a digit commits that option at once — Gemini's own numeric shortcut,
// modelled so a test would see af choose by position if it ever did. After a
// commit the pane shows the real restart frame, then the relaunched composer.
type geminiTrustPane struct {
	mu        sync.Mutex
	fixture   string
	selected  int
	committed []string
	// raw is the real capture for the state it was captured in.
	raw         map[int]string
	restarting  int
	afterCommit []string
	t           *testing.T
}

// newGeminiTrustPane starts the picker with its cursor on selected. captured is
// the cursor the real capture was taken with; that state shows the capture
// byte for byte, and every other state is the capture with the ● moved.
func newGeminiTrustPane(t *testing.T, fixture string, captured, selected int) *geminiTrustPane {
	t.Helper()
	return &geminiTrustPane{
		fixture:    fixture,
		selected:   selected,
		raw:        map[int]string{captured: geminiTrustFixture(t, fixture)},
		restarting: 2,
		afterCommit: []string{
			geminiTrustFixture(t, "gemini_0_51_0_trust_restarting.ansi"),
			geminiTrustFixture(t, "gemini_0_51_0_restarted_composer.ansi"),
		},
		t: t,
	}
}

func (p *geminiTrustPane) capture() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.committed) > 0 {
		if p.restarting > 0 {
			p.restarting--
			return p.afterCommit[0]
		}
		return p.afterCommit[1]
	}
	if frame, ok := p.raw[p.selected]; ok {
		return frame
	}
	return geminiTrustFrameWithCursor(p.t, p.fixture, p.selected)
}

func (p *geminiTrustPane) key(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.committed) > 0 {
		return
	}
	switch name {
	case "Down":
		p.selected = (p.selected + 1) % len(geminiTestOptions)
	case "Up":
		p.selected = (p.selected - 1 + len(geminiTestOptions)) % len(geminiTestOptions)
	case "Enter":
		p.committed = append(p.committed, geminiTestOptions[p.selected])
	case "1", "2", "3":
		p.committed = append(p.committed, geminiTestOptions[name[0]-'1'])
	}
}

func (p *geminiTrustPane) committedLabels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.committed...)
}

// geminiTrustSession binds a Gemini session to pane and returns the send-keys
// commands it issues. content, when non-nil, overrides the pane with a static
// capture.
func geminiTrustSession(t *testing.T, pane *geminiTrustPane, content *string) (*TmuxSession, *[]string) {
	t.Helper()
	keys := &[]string{}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "send-keys") {
				return nil
			}
			*keys = append(*keys, joined)
			if pane != nil {
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
			if content != nil {
				return []byte(*content), nil
			}
			return []byte(pane.capture()), nil
		},
	}
	return newTmuxSession(toTmuxName("gemini", ""), ProgramGemini, NewMockPtyFactory(t), cmdExec), keys
}

// pollGeminiPane runs CheckAndHandleTrustPrompt the way task.DismissTrustPrompt
// does — one daemon poll interval apart — until it reports the pane clear or
// the budget runs out, and returns the last verdict.
func pollGeminiPane(t *testing.T, session *TmuxSession, polls int) bool {
	t.Helper()
	advance := setClaudeTrustClock(t)
	handled := false
	for i := 0; i < polls; i++ {
		advance(time.Second)
		if handled = session.CheckAndHandleTrustPrompt(); !handled {
			break
		}
	}
	return handled
}

// The property #7162 names: af answers the dialog with "Trust folder", chosen
// by LABEL — moving the ● onto it when it is not preselected — and never "Trust
// parent folder" or a digit. Checked by WHICH option the picker committed,
// against every real capture of the dialog. On master the dialog is not
// recognized at all, nothing is committed, and the create types into it.
func TestCheckAndHandleTrustPrompt_GeminiFolderTrustSelectsTrustFolderByLabel(t *testing.T) {
	for _, tt := range []struct {
		name     string
		fixture  string
		captured int
		selected int
		wantKeys []string
	}{
		{"first paint, 80 columns", "gemini_0_51_0_folder_trust.ansi", 0, 0, []string{"Enter"}},
		{"below the tips, 80 columns", "gemini_0_51_0_folder_trust_below_tips.ansi", 0, 0, []string{"Enter"}},
		{"160 columns", "gemini_0_51_0_folder_trust_wide.ansi", 0, 0, []string{"Enter"}},
		{"a repo with commands and an MCP server, body cut to fit", "gemini_0_51_0_folder_trust_discovery.ansi", 0, 0, []string{"Enter"}},
		{"cursor on Trust parent folder (real capture)", "gemini_0_51_0_folder_trust_cursor_on_parent.ansi", 1, 1, []string{"Up", "Enter"}},
		{"cursor on Don't trust", "gemini_0_51_0_folder_trust.ansi", 0, 2, []string{"Up", "Up", "Enter"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pane := newGeminiTrustPane(t, tt.fixture, tt.captured, tt.selected)
			session, keys := geminiTrustSession(t, pane, nil)

			handled := pollGeminiPane(t, session, 10)
			require.False(t, handled, "once Gemini has relaunched with the folder trusted, the pane is clear")
			require.Equal(t, []string{geminiTestTrustFolder}, pane.committedLabels(),
				"af must commit exactly %q; keystrokes were %v", geminiTestTrustFolder, *keys)
			require.Equal(t, tt.wantKeys, injectedKeyNames(*keys),
				"af navigates by label and confirms with Enter — never a digit; got %v", *keys)

			keystroke, _, ok := session.recentDialogKeystroke()
			require.True(t, ok, "the Enter must be recorded so a death right after it is attributed to the dialog")
			require.Equal(t, "Trust folder", keystroke.choice, "the diagnostic must name the option af confirmed")
		})
	}
}

// After the answer Gemini relaunches itself, and for about two seconds the pane
// shows the dialog with the restart notice below it. A prompt typed then reaches
// the process that is exiting, so af holds until the relaunched composer is
// painted below the notice — and then reports the pane clear.
func TestCheckAndHandleTrustPrompt_GeminiFolderTrustHoldsUntilGeminiRelaunches(t *testing.T) {
	restarting := geminiTrustFixture(t, "gemini_0_51_0_trust_restarting.ansi")
	session, keys := geminiTrustSession(t, nil, &restarting)
	require.True(t, pollGeminiPane(t, session, 4), "the restart notice under the dialog means Gemini is relaunching, not ready")
	require.Empty(t, *keys, "nothing may be typed into a relaunching Gemini; got %v", *keys)

	for _, fixture := range []string{
		"gemini_0_51_0_restarted_composer_below_dialog.ansi",
		"gemini_0_51_0_restarted_composer.ansi",
	} {
		content := geminiTrustFixture(t, fixture)
		session, keys := geminiTrustSession(t, nil, &content)
		require.False(t, pollGeminiPane(t, session, 4), "%s: the relaunched composer is below the notice, so the pane is clear", fixture)
		require.Empty(t, *keys, "%s: no key is owed to a pane with no dialog; got %v", fixture, *keys)
	}
}

// The structural guard every trust branch keeps (#4254, #4735): a working
// Gemini pane paints its composer at the bottom, so the dialog quoted in output
// — or the answered dialog's box still visible above the relaunched composer —
// is never the last thing on screen and is never answered. Gemini's
// unauthenticated "Waiting for authentication…" box is a different screen and
// is left alone here.
func TestCheckAndHandleTrustPrompt_GeminiFolderTrustNotOwningThePaneInjectsNothing(t *testing.T) {
	dialog := geminiTrustFixture(t, "gemini_0_51_0_folder_trust.ansi")
	composer := geminiTrustFixture(t, "gemini_0_51_0_trusted_folder_composer.ansi")
	for _, tt := range []struct{ name, content string }{
		{"the captured dialog above a live composer", strings.TrimRight(dialog, "\n") + "\n" + composer},
		{"the captured dialog with trailing output", strings.TrimRight(dialog, "\n") + "\nThat is the dialog af answers at startup.\n"},
		{"the question quoted in prose above the composer", "Gemini asked:\nDo you trust the files in this folder?\n" + composer},
		{"the answered dialog above the relaunched composer", geminiTrustFixture(t, "gemini_0_51_0_restarted_composer_below_dialog.ansi")},
		{"the composer's first paint", geminiTrustFixture(t, "gemini_0_51_0_composer_first_paint.ansi")},
		{"the unauthenticated waiting box", geminiTrustFixture(t, "gemini_0_51_0_waiting_for_auth.ansi")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := tt.content
			session, keys := geminiTrustSession(t, nil, &content)
			require.False(t, pollGeminiPane(t, session, 4), "this pane is not showing the dialog")
			require.Empty(t, *keys, "no key may be injected into a pane that is not showing the dialog; got %v", *keys)
		})
	}
}

// A dialog af cannot read is refused, not guessed at: no key, the dialog still
// reported in the way, and one log line naming what af was aiming at.
func TestCheckAndHandleTrustPrompt_UnreadableGeminiFolderTrustDialogIsRefused(t *testing.T) {
	const fixture = "gemini_0_51_0_folder_trust.ansi"
	for _, tt := range []struct{ name, content string }{
		{"no row carries the cursor", geminiTrustFrameWithCursor(t, fixture)},
		{"two rows carry the cursor", geminiTrustFrameWithCursor(t, fixture, 0, 1)},
		{"an option the dialog does not paint", strings.Replace(geminiTrustFrameWithCursor(t, fixture, 0),
			"3. "+geminiTestDontTrust+" ", "3. Trust everything", 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			content := tt.content
			session, keys := geminiTrustSession(t, nil, &content)
			require.True(t, pollGeminiPane(t, session, 4), "an unreadable dialog is still in the way (#3302)")
			require.Empty(t, *keys, "af must not press a key it cannot aim at Trust folder; got %v", *keys)
			require.Contains(t, errLog.String(), "Trust folder", "the refusal must name what af was trying to select")
			require.Equal(t, 1, strings.Count(errLog.String(), "refusing to answer"),
				"the refusal is reported once per dialog, not once per poll")
		})
	}
}

// A frame that is a prefix of the dialog — its box not closed yet — holds
// without a key, rather than reporting the pane clear.
func TestCheckAndHandleTrustPrompt_PartiallyPaintedGeminiFolderTrustDialogHolds(t *testing.T) {
	full := strings.Split(geminiTrustFrameWithCursor(t, "gemini_0_51_0_folder_trust.ansi", 0), "\n")
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
		{"the question only", rowOf("Do you trust the files in this folder?")},
		{"through the body", rowOf("behavior of the CLI.")},
		{"every option, box not closed", rowOf(geminiTestDontTrust)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Join(full[:tt.last+1], "\n") + "\n"
			session, keys := geminiTrustSession(t, nil, &content)
			require.True(t, pollGeminiPane(t, session, 4), "a partially painted dialog must hold, not report the pane clear")
			require.Empty(t, *keys, "a partially painted dialog receives no key; got %v", *keys)
		})
	}
}
