package ui

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestScrollEntryExitNeverCapturesOnEventLoop is the #1637 regression: entering
// and exiting scroll mode must NOT perform the full-scrollback capture inline.
// Before the fix, ScrollUp/ScrollDown/ResetToNormalMode called the preview
// source (a tmux capture-pane, later an unbounded daemon Preview RPC) directly
// on the bubbletea event loop while holding p.mu, so a slow or hung capture froze
// the entire TUI. Now scroll entry/exit only flips state and marks a pending
// fill; the capture rides the off-loop refresh goroutine (UpdateContent).
//
// The preview source here blocks forever until released, standing in for a hung
// capture. Each scroll method must return promptly regardless — a method that
// blocks on it is doing I/O on the event loop, the exact freeze this fixes.
func TestScrollEntryExitNeverCapturesOnEventLoop(t *testing.T) {
	inst := makeShellInstance(t, "offloop", "scrollback-line")
	defer func() { _ = inst.Kill() }()

	release := make(chan struct{})
	var calls int32
	blockingSrc := func(_ *session.Instance, _ int, _ bool) (string, error) {
		atomic.AddInt32(&calls, 1)
		<-release // stand in for a slow/hung tmux capture or daemon Preview RPC
		return "scrollback-line", nil
	}

	p := NewTabPane(blockingSrc)
	p.SetSize(80, 30)

	// mustReturnPromptly runs fn on another goroutine and fails if it does not
	// return quickly — i.e. if it blocked on the (never-released) capture.
	mustReturnPromptly := func(what string, fn func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- fn() }()
		select {
		case err := <-done:
			require.NoError(t, err, what)
		case <-time.After(3 * time.Second):
			t.Fatalf("%s blocked on the capture — scroll entry/exit must not do I/O on the event loop (#1637)", what)
		}
	}

	// Enter scroll mode on the shell tab. Must return at once and NOT capture.
	mustReturnPromptly("ScrollUp", func() error { return p.ScrollUp(inst, 1) })
	require.True(t, p.IsScrolling(), "ScrollUp enters scroll mode")
	require.True(t, p.NeedsScrollFill(), "scroll entry marks a pending off-loop fill")
	require.Equal(t, int32(0), atomic.LoadInt32(&calls),
		"scroll entry must not call the preview source on the event loop")

	// Exit scroll mode. Must also return at once and NOT capture.
	mustReturnPromptly("ResetToNormalMode", func() error { return p.ResetToNormalMode(inst, 1) })
	require.False(t, p.IsScrolling(), "ResetToNormalMode exits scroll mode")
	require.Equal(t, int32(0), atomic.LoadInt32(&calls),
		"scroll exit must not call the preview source on the event loop")

	// The capture is the off-loop refresh's job. Re-enter scroll mode, then run
	// UpdateContent (the refresh goroutine's work) — it IS allowed to block on the
	// capture because it never runs on the event loop. Release the capture and it
	// completes, filling the viewport and clearing the pending flag.
	require.NoError(t, p.ScrollUp(inst, 1))
	require.True(t, p.NeedsScrollFill())

	fillDone := make(chan error, 1)
	go func() { fillDone <- p.UpdateContent(inst, 1) }()
	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) >= 1 }, 3*time.Second, 5*time.Millisecond,
		"the off-loop refresh must be the path that performs the scroll capture")
	close(release)
	require.NoError(t, <-fillDone)

	require.False(t, p.NeedsScrollFill(), "a completed fill clears the pending flag")
	require.Contains(t, p.viewport.View(), "scrollback-line",
		"the off-loop fill populates the scroll viewport")
	require.True(t, p.IsScrolling(), "the pane stays in scroll mode after the fill")
}
