package ui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

func numberedScrollHistory(lines int) string {
	out := make([]string, lines)
	for i := range out {
		out[i] = fmt.Sprintf("history-%03d", i+1)
	}
	return strings.Join(out, "\n")
}

// TestFirstScrollIntentRevealsAnOlderTerminalRow pins the user-visible contract,
// not just the controller's internal offset. tmux terminates capture-pane output
// with a newline; that record separator and AF's own scroll-mode chrome must not
// become rows in the terminal's history coordinate system. With a ten-row pane,
// the normal bottom view is history-031..040, so one upward gesture must reveal
// history-030 immediately.
func TestFirstScrollIntentRevealsAnOlderTerminalRow(t *testing.T) {
	inst := makeShellInstance(t, "visible-first-intent", "visible-line")
	defer func() { _ = inst.Kill() }()

	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if !full {
			return hostPreview("visible-line"), nil
		}
		return hostPreview(numberedScrollHistory(40) + "\n"), nil
	}
	p := NewTabPane(src)
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
	p.SetSize(80, 10)
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()
	require.NoError(t, p.UpdateContent(inst, 1))

	rendered := p.String()
	require.Contains(t, rendered, "history-030",
		"the first gesture must reveal a row older than the normal bottom view")
	require.NotContains(t, rendered, "history-040",
		"scrolling up one row must move the newest row out of the viewport")
	require.NotContains(t, rendered, "Scroll",
		"AF scroll-mode chrome must not be part of terminal history")
}

func TestCapturePaneHistoryRowsRemovesOnlyTheRecordSeparator(t *testing.T) {
	require.Equal(t, "history", capturePaneHistoryRows("history\n"))
	require.Equal(t, "history\n", capturePaneHistoryRows("history\n\n"),
		"an intentional blank terminal row must survive")
	require.Equal(t, "history", capturePaneHistoryRows("history"))
}

// TestFirstScrollIntentSurvivesPendingFill is the fail-first half of #2192:
// the gesture that ENTERS scroll mode is still a scroll request. The history
// capture remains off the event loop (#1637), but when it lands the viewport
// must be one row above the bottom rather than discarding that first intent.
func TestFirstScrollIntentSurvivesPendingFill(t *testing.T) {
	inst := makeShellInstance(t, "first-intent", "visible-line")
	defer func() { _ = inst.Kill() }()

	const height = 10
	history := numberedScrollHistory(40)
	started := make(chan struct{})
	release := make(chan struct{})
	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if !full {
			return hostPreview("visible-line"), nil
		}
		close(started)
		<-release
		return hostPreview(history), nil
	}

	p := NewTabPane(src)
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
	p.SetSize(80, height)
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()

	filled := make(chan error, 1)
	go func() { filled <- p.UpdateContent(inst, 1) }()
	<-started
	close(release)
	require.NoError(t, <-filled)

	// Forty terminal rows produce a bottom offset of 30. The initiating up
	// intent must therefore land at offset 29.
	require.Equal(t, 29, p.viewport.YOffset,
		"the first scroll-up must be applied after the async history fill")
}

// TestQueuedScrollIntentsSurvivePendingFill is the latency-sensitive half of
// #2192. Input can continue while the off-loop capture is pending; every
// gesture must accumulate against the eventual history instead of operating on
// the temporarily empty viewport and disappearing when the fill publishes.
func TestQueuedScrollIntentsSurvivePendingFill(t *testing.T) {
	inst := makeShellInstance(t, "queued-intents", "visible-line")
	defer func() { _ = inst.Kill() }()

	const height = 10
	history := numberedScrollHistory(40)
	started := make(chan struct{})
	release := make(chan struct{})
	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if !full {
			return hostPreview("visible-line"), nil
		}
		close(started)
		<-release
		return hostPreview(history), nil
	}

	p := NewTabPane(src)
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
	p.SetSize(80, height)
	require.NoError(t, p.ScrollUp(inst, 1)) // initiating intent
	p.BeginScrollFill()

	filled := make(chan error, 1)
	go func() { filled <- p.UpdateContent(inst, 1) }()
	<-started
	require.NoError(t, p.ScrollUp(inst, 1)) // queued while capture is blocked
	require.NoError(t, p.ScrollUp(inst, 1)) // queued while capture is blocked
	close(release)
	require.NoError(t, <-filled)

	// Bottom is 30; all three up intents must survive, yielding 27.
	require.Equal(t, 27, p.viewport.YOffset,
		"scroll intents queued during the pending fill must all be applied")
}

// TestDuplicateRefreshCannotStarveCurrentScrollFill pins the regular-refresh
// ordering Codex Review found on #2257. BeginScrollFill masks the urgent refresh,
// but a normal 100ms refresh may still call UpdateContent while the first fill is
// pending. The controller must reject that duplicate before it launches another
// full capture, leaving the initiating fill authoritative.
func TestDuplicateRefreshCannotStarveCurrentScrollFill(t *testing.T) {
	inst := makeShellInstance(t, "duplicate-scroll-fill", "visible-line")
	defer func() { _ = inst.Kill() }()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var fullCalls int32
	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if !full {
			return hostPreview("visible-line"), nil
		}
		switch atomic.AddInt32(&fullCalls, 1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return hostPreview(strings.ReplaceAll(
				numberedScrollHistory(40), "history-", "first-history-")), nil
		case 2:
			return hostPreview(strings.ReplaceAll(
				numberedScrollHistory(40), "history-", "second-history-")), nil
		default:
			return PreviewSnapshot{}, fmt.Errorf("unexpected full-history capture")
		}
	}

	p := NewTabPane(src)
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
	p.SetSize(80, 10)
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()

	firstDone := make(chan error, 1)
	go func() { firstDone <- p.UpdateContent(inst, 1) }()
	<-firstStarted
	require.NoError(t, p.UpdateContent(inst, 1),
		"a periodic duplicate must return without waiting for the active fill")

	close(releaseFirst)
	require.NoError(t, <-firstDone)
	require.Equal(t, int32(1), atomic.LoadInt32(&fullCalls),
		"one fill lifecycle must launch exactly one full-history capture")
	require.Contains(t, p.viewport.View(), "first-history-",
		"the initiating fill must remain authoritative after a duplicate refresh")
}

// TestScrollEntryInvalidatesOlderNormalSnapshotWithoutLosingIntent pins the
// other ordering of the asynchronous race: a normal capture may already be in
// flight when the user scrolls. Scroll entry makes that older normal snapshot
// stale, while the controller retains both intents for its full snapshot.
func TestScrollEntryInvalidatesOlderNormalSnapshotWithoutLosingIntent(t *testing.T) {
	inst := makeShellInstance(t, "ownership-snapshot", "visible-line")
	defer func() { _ = inst.Kill() }()

	const height = 10
	normalStarted := make(chan struct{})
	releaseNormal := make(chan struct{})
	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if full {
			return hostPreview(numberedScrollHistory(40)), nil
		}
		close(normalStarted)
		<-releaseNormal
		return hostPreview("visible-line"), nil
	}

	p := NewTabPane(src)
	p.SetSize(80, height)
	normalDone := make(chan error, 1)
	go func() { normalDone <- p.UpdateContent(inst, 1) }()
	<-normalStarted

	// Both requests land while ownership is still unknown. The older normal
	// snapshot must not publish after scroll entry; the full capture resolves the
	// same controller to HostHistory and replays both requests.
	require.NoError(t, p.ScrollUp(inst, 1))
	require.NoError(t, p.ScrollUp(inst, 1))
	close(releaseNormal)
	require.NoError(t, <-normalDone)
	require.Equal(t, ScrollOwnerNone, p.ScrollOwner(),
		"scroll entry must invalidate the older normal snapshot")
	require.True(t, p.NeedsScrollFill())

	p.BeginScrollFill()
	require.NoError(t, p.UpdateContent(inst, 1))
	require.Equal(t, 28, p.viewport.YOffset,
		"an ownership snapshot must not erase input queued by its capture probe")
}

func TestOwnerSwitchInvalidatesPendingHostHistoryFill(t *testing.T) {
	inst := makeShellInstance(t, "owner-switch", "visible-line")
	defer func() { _ = inst.Kill() }()

	started := make(chan struct{})
	release := make(chan struct{})
	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if !full {
			return hostPreview("visible-line"), nil
		}
		close(started)
		<-release
		return hostPreview(numberedScrollHistory(40)), nil
	}
	p := NewTabPane(src)
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
	p.SetSize(80, 10)
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()

	filled := make(chan error, 1)
	go func() { filled <- p.UpdateContent(inst, 1) }()
	<-started
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerChildApplication)
	close(release)
	require.NoError(t, <-filled)

	require.Equal(t, ScrollOwnerChildApplication, p.ScrollOwner())
	require.False(t, p.IsScrolling(),
		"a late host capture must not paint over a child-owned terminal")
	require.Equal(t, 0, p.viewport.YOffset)
}

func TestUnknownOwnerObservationPreservesOnlyActiveHostHistory(t *testing.T) {
	inst := makeShellInstance(t, "unknown-active-host", "visible-line")
	defer func() { _ = inst.Kill() }()

	p := NewTabPane(func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if full {
			return hostPreview(numberedScrollHistory(40)), nil
		}
		return hostPreview("visible-line"), nil
	})
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
	p.SetSize(80, 10)
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()
	require.NoError(t, p.UpdateContent(inst, 1))
	beforeOffset, beforeView := p.viewport.YOffset, p.viewport.View()

	owner := p.ObserveScrollOwnerUnknownFor(inst, 1)
	require.Equal(t, ScrollOwnerHostHistory, owner)
	require.True(t, p.IsScrolling())
	require.Equal(t, beforeOffset, p.viewport.YOffset)
	require.Equal(t, beforeView, p.viewport.View(),
		"transient unknown authority must preserve the active history position")

	require.NoError(t, p.ResetToNormalMode(inst, 1))
	require.Equal(t, ScrollOwnerNone, p.ObserveScrollOwnerUnknownFor(inst, 1),
		"an idle host owner must fail closed so stale modes cannot start new history")
}

func TestLateNormalSnapshotCannotOverrideNewerChildOwnership(t *testing.T) {
	inst := makeShellInstance(t, "late-normal-owner", "visible-line")
	defer func() { _ = inst.Kill() }()

	normalStarted := make(chan struct{})
	releaseNormal := make(chan struct{})
	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if full {
			return PreviewSnapshot{
				Content: numberedScrollHistory(40),
				Owner:   ScrollOwnerChildApplication,
			}, nil
		}
		close(normalStarted)
		<-releaseNormal
		return hostPreview("stale-primary-grid"), nil
	}

	p := NewTabPane(src)
	p.SetSize(80, 10)
	normalDone := make(chan error, 1)
	go func() { normalDone <- p.UpdateContent(inst, 1) }()
	<-normalStarted

	// The gesture starts a newer full snapshot. It observes an alternate-screen
	// child and rejects tmux's background primary buffer.
	require.NoError(t, p.ScrollUp(inst, 1))
	p.BeginScrollFill()
	require.NoError(t, p.UpdateContent(inst, 1))
	require.Equal(t, ScrollOwnerChildApplication, p.ScrollOwner())

	// The older primary-screen snapshot finishes last. Completion order must not
	// let it reclassify the pane as HostHistory.
	close(releaseNormal)
	require.NoError(t, <-normalDone)
	require.Equal(t, ScrollOwnerChildApplication, p.ScrollOwner(),
		"a stale normal capture must not overwrite the newer full snapshot")
	require.False(t, p.IsScrolling())
}

func TestSameTargetPreviewPublishesByDispatchOrderNotCompletionOrder(t *testing.T) {
	inst := makeShellInstance(t, "same-target-preview-order", "visible-line")
	defer func() { _ = inst.Kill() }()

	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var calls int32
	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		require.False(t, full)
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return hostPreview("older-grid"), nil
		case 2:
			close(secondStarted)
			<-releaseSecond
			return hostPreview("newer-grid"), nil
		default:
			return PreviewSnapshot{}, fmt.Errorf("unexpected preview capture")
		}
	}

	p := NewTabPane(src)
	p.SetSize(80, 10)
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- p.UpdateContent(inst, 1) }()
	<-firstStarted
	go func() { secondDone <- p.UpdateContent(inst, 1) }()
	<-secondStarted

	close(releaseSecond)
	require.NoError(t, <-secondDone)
	require.Contains(t, p.String(), "newer-grid")

	close(releaseFirst)
	require.NoError(t, <-firstDone)
	require.Contains(t, p.String(), "newer-grid",
		"an older capture finishing last must not replace the newer snapshot")
	require.NotContains(t, p.String(), "older-grid")
}

func TestFullCaptureCannotPublishAfterTargetBecomesChildOwned(t *testing.T) {
	inst := makeShellInstance(t, "owner-from-fill", "visible-line")
	defer func() { _ = inst.Kill() }()

	src := func(_ *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if !full {
			return hostPreview("visible-line"), nil
		}
		return PreviewSnapshot{
			Content: numberedScrollHistory(40),
			Owner:   ScrollOwnerChildApplication,
		}, nil
	}
	p := NewTabPane(src)
	p.SetSize(80, 10)
	enableHostHistory(p, inst, 1)
	require.NoError(t, p.ScrollUp(inst, 1))
	require.NoError(t, p.UpdateContent(inst, 1))

	require.Equal(t, ScrollOwnerChildApplication, p.ScrollOwner())
	require.False(t, p.IsScrolling())
	require.NotContains(t, p.viewport.View(), "history-",
		"a full capture from an alternate-screen target is the wrong buffer")
}

// TestHostHistoryScrollRetargetKeepsNewViewIntent pins the view-key boundary:
// moving from A to B must discard A's ready viewport, but the wheel/key intent
// that performs the retarget is B's first request and must survive B's fill.
func TestHostHistoryScrollRetargetKeepsNewViewIntent(t *testing.T) {
	instA := makeShellInstance(t, "retarget-a", "visible-a")
	instB := makeShellInstance(t, "retarget-b", "visible-b")
	defer func() { _ = instA.Kill() }()
	defer func() { _ = instB.Kill() }()

	historyA := strings.ReplaceAll(numberedScrollHistory(40), "history", "a-history")
	historyB := strings.ReplaceAll(numberedScrollHistory(40), "history", "b-history")
	aStarted := make(chan struct{})
	releaseA := make(chan struct{})
	src := func(inst *session.Instance, _ int, full bool) (PreviewSnapshot, error) {
		if !full {
			if inst == instA {
				return hostPreview("visible-a"), nil
			}
			return hostPreview("visible-b"), nil
		}
		if inst == instA {
			close(aStarted)
			<-releaseA
			return hostPreview(historyA), nil
		}
		return hostPreview(historyB), nil
	}

	p := NewTabPane(src)
	p.SetScrollOwnerFor(instA, 1, ScrollOwnerHostHistory)
	p.SetSize(80, 10)
	require.NoError(t, p.ScrollUp(instA, 1))
	p.BeginScrollFill()
	aFilled := make(chan error, 1)
	go func() { aFilled <- p.UpdateContent(instA, 1) }()
	<-aStarted

	// Retarget while A is still captured off-loop. This gesture belongs to B.
	require.NoError(t, p.ScrollUp(instB, 1))
	require.NotContains(t, p.viewport.View(), "a-history",
		"retarget must clear A before B's capture lands")
	p.BeginScrollFill()
	require.NoError(t, p.UpdateContent(instB, 1))
	require.Contains(t, p.viewport.View(), "b-history")
	require.NotContains(t, p.viewport.View(), "a-history")
	require.Equal(t, 29, p.viewport.YOffset,
		"the retargeting intent must be applied to B's host history")

	// A's stale capture may return afterward, but cannot overwrite B or consume
	// B's already-applied intent.
	close(releaseA)
	require.NoError(t, <-aFilled)
	require.Contains(t, p.viewport.View(), "b-history")
	require.NotContains(t, p.viewport.View(), "a-history")
	require.Equal(t, 29, p.viewport.YOffset)
}

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
	blockingSrc := func(_ *session.Instance, _ int, _ bool) (PreviewSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		<-release // stand in for a slow/hung tmux capture or daemon Preview RPC
		return hostPreview("scrollback-line"), nil
	}

	p := NewTabPane(blockingSrc)
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
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
	countingSrc := func(_ *session.Instance, _ int, _ bool) (PreviewSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		return hostPreview("scrollback-line"), nil
	}
	p := NewTabPane(countingSrc)
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
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
	blockingSrc := func(_ *session.Instance, _ int, _ bool) (PreviewSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		g := &gate{ch: make(chan struct{})}
		gates <- g
		<-g.ch
		return hostPreview("scrollback-line"), nil
	}
	p := NewTabPane(blockingSrc)
	p.SetScrollOwnerFor(inst, 1, ScrollOwnerHostHistory)
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
