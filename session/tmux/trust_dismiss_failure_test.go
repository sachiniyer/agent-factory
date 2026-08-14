package tmux

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// Regression tests for #3302. task.DismissTrustPrompt treats a false return
// from CheckAndHandleTrustPrompt as "the pane shows no dialog" — its permission
// to let the create path type the user's prompt into the pane. A dialog that
// was positively identified but whose dismissal keystroke failed is still on
// screen, so it must report true ("one was there; it may still be in the way")
// and let the loop retry within its budget and fail loudly, exactly as the
// codex safety-check picker beside these branches already does.

// runTrustPromptCheckSendKeysFail drives CheckAndHandleTrustPrompt for a pane
// running `program` and showing `content` while every send-keys command fails,
// modelling a dismissal keystroke that does not land. Every other tmux command
// (has-session, the cursor query) stays healthy so the failure is attributed to
// the keystroke alone, not to a dead session.
func runTrustPromptCheckSendKeysFail(t *testing.T, program, content string) (handled bool, cmds []string) {
	t.Helper()
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			cmds = append(cmds, joined)
			if strings.Contains(joined, "send-keys") {
				return errors.New("send-keys refused")
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return []byte("0 0 0"), nil
			}
			return []byte(content), nil
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), program, NewMockPtyFactory(t), cmdExec)
	return session.CheckAndHandleTrustPrompt(), cmds
}

func TestCheckAndHandleTrustPrompt_FailedDismissalStillReportsDialog(t *testing.T) {
	tests := []struct {
		name    string
		program string
		content string
	}{
		{"claude folder trust", ProgramClaude, "Do you trust the files in this folder?\n❯ Yes  No"},
		{"codex directory trust", ProgramCodex, codexDirectoryTrustDialog},
		{"aider doc trust", ProgramAider, aiderDocTrustDialog},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, errLog := captureTrustPromptLogs(t)
			handled, cmds := runTrustPromptCheckSendKeysFail(t, tt.program, tt.content)
			require.NotEmpty(t, sentKeystrokes(cmds),
				"precondition: the dialog must have been identified and its dismissal attempted")
			require.True(t, handled,
				"a found-but-not-dismissed dialog is still in the way; reporting false lets task.DismissTrustPrompt type the user's prompt into it (#3302)")
			require.NotEmpty(t, errLog.String(), "the failed keystroke must be logged")
		})
	}
}

// An unreadable pane is not a pane with no dialog on it. Capture can fail
// transiently on the same wedged server that just failed a keystroke, and a
// false return there lets the caller type into a state nobody observed — the
// failed-read-as-empty-result class (#2870). DismissTrustPrompt's budget and
// its readiness re-wait (which fails fast on a genuinely dead session) bound
// the retries this true return buys.
func TestCheckAndHandleTrustPrompt_UnreadablePaneStillReportsDialog(t *testing.T) {
	var keystrokes []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			keystrokes = append(keystrokes, strings.Join(c.Args, " "))
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return nil, errors.New("capture-pane refused")
		},
	}
	session := newTmuxSession(toTmuxName("trust", ""), ProgramClaude, NewMockPtyFactory(t), cmdExec)
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"an unreadable pane must not be reported as dialog-free (#3302)")
	require.Empty(t, sentKeystrokes(keystrokes),
		"no dismissal keystroke may be sent blind into an unobserved pane")
}
