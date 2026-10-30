package tmux

import (
	"strings"
	"testing"
)

func TestSessionReadyTimeoutErrorNamesTheDeadlineAndBothCauses(t *testing.T) {
	msg := sessionReadyTimeoutError("af_x", "claude --flag").Error()
	for _, want := range []string{
		"timed out waiting for tmux session af_x",
		"within 2s",
		"too busy",
		"exited immediately",
		`"claude --flag"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}
