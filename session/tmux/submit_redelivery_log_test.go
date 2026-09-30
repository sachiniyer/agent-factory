package tmux

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

func captureWarningLog(t *testing.T) *logtest.Buffer {
	t.Helper()
	var buf logtest.Buffer
	prev := aflog.WarningLog.Writer()
	aflog.WarningLog.SetOutput(&buf)
	t.Cleanup(func() { aflog.WarningLog.SetOutput(prev) })
	return &buf
}

// TestWithheldRedeliveryIsNotLoggedAsRedelivered pins #4934: the daemon log
// must describe what the redelivery path did, not what it was about to try.
// The first attempt reads as absent through the Enter boundary, but the prompt
// was submitted, so the pre-retry check withholds the second paste. A
// "redelivering" line for that attempt reports a double submit that never
// happened to anyone auditing the log for one.
func TestWithheldRedeliveryIsNotLoggedAsRedelivered(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	warnings := captureWarningLog(t)

	pane := newHeartbeatPane(t)
	truncated := wrapClaude(heartbeatPrompt)[:4]
	pane.onPaste = func(int, string) []string { return truncated }
	pane.onEnter = func(p *slidingClaudePane, _ int) {
		p.submitComposerAs(heartbeatPrompt, "", "✻ Crunching… (esc to interrupt)")
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())

	status, err := session.SendKeysCommandObserved(heartbeatPrompt)
	require.NoError(t, err)

	pastes, _ := pane.counts()
	require.Equal(t, 1, pastes, "fixture: the pre-retry check must withhold the second paste")
	require.Equal(t, PromptSentUnverified, status)
	require.NotContains(t, warnings.String(), "redelivering prompt",
		"a withheld retry must not be logged as a redelivery")
	require.Equal(t, 1, strings.Count(warnings.String(), "withholding redelivery"),
		"the withheld retry must be logged as withheld, once")
}

// TestRedeliveryIsLoggedOnceWhenItHappens is the other half of #4934: moving the
// line after the check must not lose it on the path that does redeliver.
func TestRedeliveryIsLoggedOnceWhenItHappens(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	warnings := captureWarningLog(t)

	pane := newHeartbeatPane(t)
	pane.onPaste = func(n int, payload string) []string {
		if n == 1 {
			return wrapClaude(payload)[:4]
		}
		return wrapClaude(payload)
	}
	pane.onEnter = func(p *slidingClaudePane, n int) {
		if n == 1 {
			p.composer = append(p.composer, "  ") // absorbed as a newline
			return
		}
		p.submitComposerAs(strings.Join(p.composer, " "), "", "✻ Crunching… (esc to interrupt)")
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())

	status, err := session.SendKeysCommandObserved(heartbeatPrompt)
	require.NoError(t, err)

	pastes, _ := pane.counts()
	require.Equal(t, 2, pastes, "fixture: a proven-absent strand is redelivered once")
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 1, strings.Count(warnings.String(), "redelivering prompt"),
		"the one redelivery must be logged, once")
	require.NotContains(t, warnings.String(), "withholding redelivery")
}
