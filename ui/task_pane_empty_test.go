package ui

import (
	"errors"
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
			require.Contains(t, plain, "esc", "the overlay advertises how to leave")
		})
	}
}
