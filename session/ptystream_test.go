package session

import (
	"testing"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPTYFileStreamResize verifies the PTYStream Resize primitive (#1592 Phase 1
// PR5) drives a real TIOCSWINSZ on the underlying PTY master: after Resize the
// kernel reports the new window size. This is the forward seam a client-driven
// resize (Phase 2) rides on; here it is exercised end to end on a real PTY.
func TestPTYFileStreamResize(t *testing.T) {
	ptmx, tty, err := pty.Open()
	require.NoError(t, err)
	defer ptmx.Close()
	defer tty.Close()

	var stream PTYStream = ptyFileStream{ptmx}
	require.NoError(t, stream.Resize(24, 80))

	rows, cols, err := pty.Getsize(ptmx)
	require.NoError(t, err)
	assert.Equal(t, 24, rows)
	assert.Equal(t, 80, cols)

	// A second resize takes effect too (the control is not one-shot).
	require.NoError(t, stream.Resize(40, 120))
	rows, cols, err = pty.Getsize(ptmx)
	require.NoError(t, err)
	assert.Equal(t, 40, rows)
	assert.Equal(t, 120, cols)
}
