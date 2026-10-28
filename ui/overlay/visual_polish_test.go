package overlay

import (
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestConfirmationActionCopyIsCompact(t *testing.T) {
	c := NewConfirmationOverlay("Delete task?")
	require.Equal(t, "y/enter confirm · n/esc cancel", xansi.Strip(c.instruction(false)))
	c.ConfirmKey = "K"
	require.Equal(t, "K confirm · n/esc cancel", xansi.Strip(c.instruction(false)))
	require.Equal(t, "K confirm · n/esc cancel", xansi.Strip(c.instruction(true)))
}
