package apiclient

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/term"

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

func gateRestoreTermios(t *testing.T) (entered, unblock chan struct{}, calls *atomic.Int32) {
	t.Helper()
	entered, unblock, calls = make(chan struct{}, 4), make(chan struct{}), new(atomic.Int32)
	prev := restoreTermios
	restoreTermios = func(int, *term.State) error {
		calls.Add(1)
		entered <- struct{}{}
		<-unblock
		return nil
	}
	t.Cleanup(func() { restoreTermios = prev })
	return entered, unblock, calls
}

func blocked(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	case <-time.After(50 * time.Millisecond):
		return true
	}
}

// #6083: a signal at every step of the release sequence reaches an owner, and the
// termios is restored exactly once. The owner stays published until the restore
// it guards has completed.
func TestSignalDuringReleaseFindsTheOwner(t *testing.T) {
	entered, unblock, calls := gateRestoreTermios(t)
	var out bytes.Buffer
	h, err := beginRawTerminalHandback(&out, func() (*term.State, error) { return &term.State{}, nil })
	require.NoError(t, err)
	require.Same(t, h, activeHandback.Load(), "step 1: raw taken, owner published")

	released := make(chan struct{})
	go func() { h.release(); close(released) }()
	<-entered // step 2: release is inside the termios restore
	require.Same(t, h, activeHandback.Load(), "the owner is still published while the restore runs")

	signalled := make(chan struct{})
	go func() { RestoreAttachedTerminal(); close(signalled) }() // the exit guard's SIGHUP/SIGQUIT
	require.True(t, blocked(signalled), "the guard waits for the in-flight restore instead of returning")

	close(unblock)
	<-released
	<-signalled
	require.Equal(t, int32(1), calls.Load(), "termios restored exactly once however many callers raced")
	require.Nil(t, activeHandback.Load(), "step 3: unpublished only after the restore completed")

	RestoreAttachedTerminal() // step 4: after the release, nothing left to do
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, tmux.NeutralTerminalRestore, out.String(), "the escape restore is written once too")
}

// A signal while the terminal is still being taken reaches the owner and waits for
// the raw mode and its termios, then restores them.
func TestSignalDuringRawModeRestoresAfterItIsTaken(t *testing.T) {
	entered, unblock, calls := gateRestoreTermios(t)
	close(unblock) // restore itself is instant here
	_ = entered

	inMakeRaw, finishRaw := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	var h *terminalHandback
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		h, err = beginRawTerminalHandback(&bytes.Buffer{}, func() (*term.State, error) {
			close(inMakeRaw)
			<-finishRaw
			return &term.State{}, nil
		})
		require.NoError(t, err)
	}()
	<-inMakeRaw
	require.NotNil(t, activeHandback.Load(), "the owner is reachable before the terminal is borrowed")

	signalled := make(chan struct{})
	go func() { RestoreAttachedTerminal(); close(signalled) }()
	require.True(t, blocked(signalled), "the guard waits until the raw mode and its termios are recorded")
	require.Equal(t, int32(0), calls.Load())

	close(finishRaw)
	wg.Wait()
	<-signalled
	require.Equal(t, int32(1), calls.Load(), "the termios just taken is restored")
	h.release()
	require.Equal(t, int32(1), calls.Load(), "the release after it does not re-apply")
	require.Nil(t, activeHandback.Load())
}

// A raw-mode failure borrowed nothing: the owner is retired and nothing restores.
func TestFailedRawModeRetiresTheOwner(t *testing.T) {
	_, unblock, calls := gateRestoreTermios(t)
	close(unblock)
	_, err := beginRawTerminalHandback(&bytes.Buffer{}, func() (*term.State, error) { return nil, errTestRaw })
	require.Error(t, err)
	require.Nil(t, activeHandback.Load())
	RestoreAttachedTerminal()
	require.Equal(t, int32(0), calls.Load())
}

var errTestRaw = errors.New("raw mode refused")
