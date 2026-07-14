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

// TestBeginScrollFillMasksNeedsScrollFill is the #1709 regression at the TabPane
// level: panesRefresh claims a pending fill with BeginScrollFill the instant it
// dispatches the off-loop capture, and NeedsScrollFill must report false from
// that moment until the capture resolves — so a refresh cycle that fires in the
// dispatch→land window (rapid scroll input, or a slow daemon Preview RPC) does
// not dispatch a redundant capture. A claim that fails to publish (view changed
// mid-flight) must re-arm, so a genuinely-owed fill is never wedged.
func TestBeginScrollFillMasksNeedsScrollFill(t *testing.T) {
	inst := makeShellInstance(t, "claim", "scrollback-line")
	defer func() { _ = inst.Kill() }()

	var calls int32
	countingSrc := func(_ *session.Instance, _ int, _ bool) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "scrollback-line", nil
	}
	p := NewTabPane(countingSrc)
	p.SetSize(80, 30)

	// Enter scroll mode: a fill is owed, none dispatched yet.
	require.NoError(t, p.ScrollUp(inst, 1))
	require.True(t, p.NeedsScrollFill(), "scroll entry owes a fill")

	// Claim the fill for a dispatched capture: NeedsScrollFill goes false so the
	// next refresh cycle is a no-op for this pane even though the fill hasn't run.
	p.BeginScrollFill()
	require.False(t, p.NeedsScrollFill(),
		"a claimed (in-flight) fill must not re-dispatch (#1709)")
	require.True(t, p.IsScrolling(), "the claim must not exit scroll mode")

	// The claimed capture resolves but cannot publish — the render binding moved
	// on mid-flight (guard returns false on the post-capture re-check). The claim
	// releases and, since nothing was published, the fill stays owed and re-arms.
	guardCalls := 0
	staleGuard := func() bool {
		guardCalls++
		return guardCalls == 1 // pass the entry check, fail the post-capture check
	}
	require.NoError(t, p.UpdateContentGuarded(inst, 1, staleGuard))
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "the claimed fill ran exactly once")
	require.True(t, p.NeedsScrollFill(),
		"a claimed fill that could not publish must re-arm, not wedge the viewport blank (#1709)")

	// The re-armed fill dispatches once more and lands.
	require.NoError(t, p.UpdateContent(inst, 1))
	require.False(t, p.NeedsScrollFill(), "the re-dispatched fill lands and clears the owed flag")
	require.Contains(t, p.viewport.View(), "scrollback-line")
}

// TestStaleScrollFillDoesNotSatisfyNewEntry is the #1709-review regression: a
// scroll-fill capture dispatched for one scroll session must NOT satisfy or
// clear the in-flight claim of a LATER session on the same instance/tab. The
// in-flight state is a shared flag, so without a generation stamp a slow capture
// returning after the user exits and re-enters scroll mode would clear the newer
// entry's claim (or publish stale scrollback over it) and drop the fresh capture.
func TestStaleScrollFillDoesNotSatisfyNewEntry(t *testing.T) {
	inst := makeShellInstance(t, "gen", "scrollback-line")
	defer func() { _ = inst.Kill() }()

	// The capture blocks on a per-call gate so we can hold session #1's fill in
	// flight across the exit/re-entry, and count total captures.
	type gate struct{ ch chan struct{} }
	gates := make(chan *gate, 4)
	var calls int32
	blockingSrc := func(_ *session.Instance, _ int, _ bool) (string, error) {
		atomic.AddInt32(&calls, 1)
		g := &gate{ch: make(chan struct{})}
		gates <- g
		<-g.ch
		return "scrollback-line", nil
	}
	p := NewTabPane(blockingSrc)
	p.SetSize(80, 30)

	// Session #1: enter scroll mode and dispatch its fill (still in flight).
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()
	fill1 := make(chan error, 1)
	go func() { fill1 <- p.UpdateContent(inst, 1) }()
	var g1 *gate
	require.Eventually(t, func() bool {
		select {
		case g1 = <-gates:
			return true
		default:
			return false
		}
	}, 3*time.Second, 5*time.Millisecond, "session #1's fill must reach the capture")

	// Exit and immediately re-enter scroll mode: session #2, same instance/tab,
	// a brand-new fill generation — its claim is still open (in flight from #1
	// belongs to the old generation).
	require.NoError(t, p.ResetToNormalMode(inst, 1))
	require.NoError(t, p.ScrollUp(inst, 1))
	require.True(t, p.NeedsScrollFill(), "the new scroll entry owes its own fill")
	p.BeginScrollFill() // session #2 claims its fill
	require.False(t, p.NeedsScrollFill(), "session #2's claim masks the pane")

	// Session #1's stale capture now returns. It must be ignored: it may not
	// clear session #2's in-flight claim nor mark #2's fill satisfied.
	close(g1.ch)
	require.NoError(t, <-fill1)
	require.False(t, p.NeedsScrollFill(),
		"a stale capture must not re-open (clear the claim of) the newer entry's fill (#1709 review)")

	// Session #2's own fill lands and populates the viewport.
	fill2 := make(chan error, 1)
	go func() { fill2 <- p.UpdateContent(inst, 1) }()
	var g2 *gate
	require.Eventually(t, func() bool {
		select {
		case g2 = <-gates:
			return true
		default:
			return false
		}
	}, 3*time.Second, 5*time.Millisecond, "session #2 must dispatch its own fresh capture, not reuse the stale one")
	close(g2.ch)
	require.NoError(t, <-fill2)
	require.False(t, p.NeedsScrollFill(), "session #2's fill lands and clears the owed flag")
	require.Contains(t, p.viewport.View(), "scrollback-line")
	require.Equal(t, int32(2), atomic.LoadInt32(&calls),
		"exactly two captures ran: one per scroll session, none redundant within a generation")
}
