package tmux

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/require"
)

// The reworded folder-trust dialog as Claude Code paints it at 80x24, captured
// with af's capture flags (-p -e -J) from a throwaway HOME and
// CLAUDE_CONFIG_DIR, an untrusted scratch folder and a private tmux server
// (#7575 review, R3). 2.1.318 and 2.1.323 paint the same bytes; 2.1.327 adds an
// OSC 8 link to "Security guide". The cursor-on-yes capture is the frame
// 2.1.327 painted after one Down. No Enter was sent, so the folder was never
// trusted. The path is scrubbed to /home/u/proj.
const (
	rewordedDialogFixture       = "claude_2_1_327_folder_trust_dialog.ansi"
	rewordedDialogNoLinkFixture = "claude_2_1_323_folder_trust_dialog.ansi"
	rewordedDialogOnYesFixture  = "claude_2_1_327_folder_trust_dialog_cursor_on_yes.ansi"
)

// TestIssue7464RealRewordedDialogEndsWithItsFooterAndIsAnswered pins the
// property #7464's added hold rests on: every dialog the gate answers ends with
// its footer and nothing below it.
//
// #7464 holds, without a key, an answering clause's dialog with a column-0
// plain rule in the chrome below it, because that is a ruled draft cut above its
// prompt (claudeComposerRuleUnderDialog). If Claude Code drew a rule under the
// reworded dialog's footer, that hold would stall every create in a new folder.
// These are the dialog's real frames: its footer is its last row, and the gate
// still answers it, by moving the cursor from the preselected "No, exit" to
// "Yes, I trust this folder" and confirming that only on the real repaint.
func TestIssue7464RealRewordedDialogEndsWithItsFooterAndIsAnswered(t *testing.T) {
	for _, name := range []string{rewordedDialogFixture, rewordedDialogNoLinkFixture} {
		t.Run(name, func(t *testing.T) {
			content := readTrustFixture(t, name)
			screen := claudeScreenOf(content)
			lines := screen.lines()
			last := len(lines) - 1
			for last >= 0 && strings.TrimSpace(lines[last]) == "" {
				last--
			}
			require.GreaterOrEqual(t, last, 0, "the capture is empty")
			require.Equal(t, claudeFolderTrustFooter, strings.TrimSpace(lines[last]),
				"the dialog's footer must be its last row, with nothing painted below it")
			require.False(t, claudeComposerRuleUnderDialog(screen),
				"no rule may sit under the real dialog, or #7464's hold would stall the create")
			require.Equal(t, claudeFolderTrustComplete, claudeFolderTrustDialogOf(screen),
				"the real dialog must read whole")

			handled, cmds := pollStaticPane(t, content, 4)
			require.True(t, handled, "the real dialog is in the way until it is answered")
			require.Equal(t, []string{"Down"}, injectedKeyNames(sentKeystrokes(cmds)),
				"af must move the cursor from the preselected %q toward %q, and confirm nothing it has not seen land",
				claudeTrustNoLabel, claudeTrustYesLabel)
		})
	}

	t.Run("the real repaint after Down is confirmed with Enter", func(t *testing.T) {
		pane := &realRewordedDialogPane{
			before: readTrustFixture(t, rewordedDialogFixture),
			after:  readTrustFixture(t, rewordedDialogOnYesFixture),
		}
		after := claudeScreenOf(pane.after)
		require.False(t, claudeComposerRuleUnderDialog(after))
		require.Equal(t, claudeFolderTrustComplete, claudeFolderTrustDialogOf(after))

		advance := setClaudeTrustClock(t)
		session := newTmuxSession(toTmuxName("real-reworded", ""), ProgramClaude, NewMockPtyFactory(t), pane.executor())
		for i := 0; i < 6 && !pane.confirmed(); i++ {
			advance(time.Second)
			require.True(t, session.CheckAndHandleTrustPrompt(), "the dialog is in the way until it is answered")
		}
		require.Equal(t, []string{"Down", "Enter"}, pane.keyNames(),
			"af must confirm the affirmative row once the real repaint shows the cursor on it")
	})
}

// realRewordedDialogPane serves the real capture until af sends Down, then the
// frame Claude Code painted after one Down, and records every key it is sent.
type realRewordedDialogPane struct {
	before, after string

	mu   sync.Mutex
	keys []string
}

func (p *realRewordedDialogPane) executor() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "send-keys") {
				p.mu.Lock()
				p.keys = append(p.keys, injectedKeyNames([]string{joined})...)
				p.mu.Unlock()
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return []byte("0 0 0"), nil
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			for _, key := range p.keys {
				if key == "Down" {
					return []byte(p.after), nil
				}
			}
			return []byte(p.before), nil
		},
	}
}

func (p *realRewordedDialogPane) keyNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.keys...)
}

func (p *realRewordedDialogPane) confirmed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range p.keys {
		if key == "Enter" {
			return true
		}
	}
	return false
}
