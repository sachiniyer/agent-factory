package apiclient

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #6072: the exit guard restores through the attach's own hand-back, exactly once
// however many callers race for it, and not at all once the attach is over.
func TestRestoreAttachedTerminalIsTheSingleIdempotentOwner(t *testing.T) {
	var out bytes.Buffer
	h := beginTerminalHandback(&out, nil)
	t.Cleanup(h.release)

	RestoreAttachedTerminal() // the exit guard, first
	first := out.String()
	require.Equal(t, tmux.NeutralTerminalRestore, first)

	RestoreAttachedTerminal() // a second caller: no-op, no stale re-apply
	h.restore()               // the attach's own watcher getting the same signal
	require.Equal(t, first, out.String(), "a second restore writes nothing")

	h.release() // the deferred hand-back
	require.Equal(t, first, out.String())

	RestoreAttachedTerminal() // attach over: nothing to restore
	require.Equal(t, first, out.String())
}
