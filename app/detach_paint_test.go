package app

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// TestSelectionChanged_DispatchesPaneRefreshOffEventLoop is the regression
// test for issue #579. The selectionChanged handler used to call
// tw.UpdatePreview + tw.UpdateTerminal synchronously on the bubbletea Update
// goroutine. Each call shells out to `tmux capture-pane` (~3–5ms locally),
// so the post-detach repaint paid ~7–10ms of event-loop blocking on top of
// waiting up to a full previewTickMsg cycle (~100ms) for the first paint.
//
// After the fix, selectionChanged returns a tea.Cmd that runs the captures in
// a goroutine and emits panesRefreshedMsg{}.
//
// The property is an ordering one — selectionChanged returns WITHOUT waiting for
// a capture — so it is asserted as one (#6375). The capture is held on a channel
// the test controls: if selectionChanged calls it synchronously it blocks inside
// the hold, the fetcher reports that it was entered before selectionChanged
// returned, and the test fails. No stopwatch: a 5ms budget failed a loaded runner
// that blocked on nothing (19.9ms on macOS, 5.9ms on 26.04) and would have passed
// a regression whose capture happened to be fast.
func TestSelectionChanged_DispatchesPaneRefreshOffEventLoop(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	resizeHome(h, 120, 40)

	// Every capture, once armed, announces itself and then waits for the test to
	// release it. Unarmed, it answers at once, so opening the pane captures nothing
	// held. The fetcher is read when a pane window is built, so it is installed
	// BEFORE the pane opens.
	var armed atomic.Bool
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	h.previewFetcher = func(daemon.PreviewRequest) (daemon.PreviewResponse, error) {
		if armed.Load() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		return testPreviewResponse("held"), nil
	}
	openTestPane(t, h, inst, 0)
	armed.Store(true)

	// Call selectionChanged off the test goroutine so a synchronous capture shows up
	// as "entered the hold before it returned" instead of a hang.
	returned := make(chan tea.Cmd, 1)
	go func() { returned <- h.selectionChanged() }()
	var cmd tea.Cmd
	select {
	case cmd = <-returned:
	case <-entered:
		unblock()
		t.Fatal("selectionChanged blocked on a pane capture: it must return a cmd and leave the capture to run off the event loop")
	}
	require.NotNil(t, cmd, "selectionChanged must return the off-loop pane refresh cmd")
	select {
	case <-entered:
		t.Fatal("selectionChanged ran a capture before returning")
	default:
	}

	// Run the returned cmds the way bubbletea does, off the loop. The capture must
	// now start (so the hold above is wired to the real path and the test is not
	// vacuous) and stay outstanding while the event loop remains free.
	done := make(chan tea.Msg, 4)
	runCmdTree(cmd, done)
	select {
	case <-entered:
	case <-time.After(30 * time.Second): // liveness only: nothing here measures a duration
		t.Fatal("the pane refresh cmd never reached the capture; the hold is not on the real path")
	}
	again := make(chan tea.Cmd, 1)
	go func() { again <- h.selectionChanged() }()
	select {
	case <-again:
	case <-time.After(30 * time.Second): // liveness only
		t.Fatal("selectionChanged did not return while a capture was outstanding")
	}
	select {
	case msg := <-done:
		t.Fatalf("the refresh finished (%T) while its capture was still held", msg)
	default:
	}

	unblock()
	select {
	case msg := <-done:
		require.IsType(t, panesRefreshedMsg{}, msg)
	case <-time.After(30 * time.Second): // liveness only
		t.Fatal("the pane refresh did not complete after its capture was released")
	}
}

// runCmdTree runs cmd the way bubbletea does, each cmd on its own goroutine,
// flattening tea.Batch, and sends every non-batch result to out.
func runCmdTree(cmd tea.Cmd, out chan<- tea.Msg) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				runCmdTree(c, out)
			}
			return
		}
		out <- msg
	}()
}

// TestRepaintAfterDetachMsg_KicksOffRefresh ensures the post-detach repaint
// path goes through selectionChanged (and therefore through the off-loop
// refresh cmd). Before the fix, the attach goroutine set m.state=stateDefault
// but emitted no tea.Msg, so the first paint waited for the next ticker
// (up to ~100ms). After the fix, the goroutine returns
// repaintAfterDetachMsg{}, which Update handles by calling selectionChanged.
func TestRepaintAfterDetachMsg_KicksOffRefresh(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	resizeHome(h, 120, 40)
	openTestPane(t, h, inst, 0)
	statePath, err := config.TUIStatePath()
	require.NoError(t, err)

	start := time.Now()
	_, cmd := h.Update(repaintAfterDetachMsg{})
	elapsed := time.Since(start)

	require.NotNil(t, cmd,
		"repaintAfterDetachMsg must return a refresh cmd so bubbletea repaints "+
			"with fresh content immediately after detach")
	assert.Less(t, elapsed, 5*time.Millisecond,
		"Update for repaintAfterDetachMsg must not block; saw %s", elapsed)
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr),
		"repaintAfterDetachMsg must not synchronously flush TUI view-state")
}

// TestRefreshPaneBindingCmd_ProducesPanesRefreshedMsg verifies the goroutine
// body itself: with a FakeBackend (no real tmux), the cmd must complete and
// return panesRefreshedMsg so bubbletea re-runs View() against the freshly
// captured content.
func TestRefreshPaneBindingCmd_ProducesPanesRefreshedMsg(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	resizeHome(h, 120, 40)
	p := openTestPane(t, h, inst, 0)
	tw := h.paneWindows[p.ID()]
	cmd := refreshPaneBindingCmd(tw, inst, tw.GetActiveTab(), tw.ContentSeq())
	require.NotNil(t, cmd)

	got := cmd()
	_, ok := got.(panesRefreshedMsg)
	require.True(t, ok, "refreshPaneBindingCmd must return panesRefreshedMsg, got %T", got)

	// And the handler for the msg must not itself schedule more work
	// (otherwise we'd loop forever on every repaint).
	_, followup := h.Update(got)
	assert.Nil(t, followup,
		"panesRefreshedMsg handler must return nil — bubbletea will re-render "+
			"after Update returns, no extra work needed")
}
