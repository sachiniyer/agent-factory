package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskPaneListRecoveryKeepsTitleAndEscHint(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "empty"},
		{name: "load failure", err: errors.New("task file is unreadable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := NewTaskPane()
			pane.SetSize(60, 12)
			pane.SetFocus(true)
			pane.SetUnavailable(tc.err)

			plain := stripANSI(pane.String())
			require.Contains(t, plain, "Tasks", "the overlay keeps its title")
			lines := strings.Split(plain, "\n")
			require.Equal(t, "n new · esc back", strings.TrimSpace(lines[len(lines)-1]),
				"the recovery footer advertises exactly the live actions")
			require.NotContains(t, plain, "enter", "an empty list must not advertise edit")
			require.NotContains(t, plain, "?", "an empty list must not advertise unavailable actions")

			pane.HandleKeyPress(keyRunes("?"))
			plain = stripANSI(pane.String())
			lines = strings.Split(plain, "\n")
			require.Equal(t, "n new · esc back", strings.TrimSpace(lines[len(lines)-1]),
				"the unavailable action menu must stay hidden in a recovery state")
			require.NotContains(t, plain, "enter")
			require.NotContains(t, plain, "?")
		})
	}
}
