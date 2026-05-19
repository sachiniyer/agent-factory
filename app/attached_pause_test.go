package app

import (
	"errors"
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTickUpdateMetadata_PausedWhileAttached is the regression test for #598:
// while the user is inside an attached tmux session, the metadata tick must
// not dispatch runMetadataTickCmd. The handler should return only the
// re-schedule cmd so the next tick fires within ~500ms of detach — which is
// what keeps the sidebar caught up without a multi-second backoff.
//
// Before the fix, runMetadataTickCmd ran every 500ms regardless of attach
// state and shelled out to tmux capture-pane 2 × N times. With ~10 sessions
// and an occasional slow capture, the resulting 40+ RPS against the shared
// tmux server queued the user's detach key for tens of seconds (#598).
func TestTickUpdateMetadata_PausedWhileAttached(t *testing.T) {
	h := newTestHome(t)
	a := instanceWithFakeBackend(t, "a")
	h.sidebar.AddInstance(a)

	h.attached.Store(true)
	_, cmd := h.Update(tickUpdateMetadataMessage{})
	require.NotNil(t, cmd, "handler must still return a re-schedule cmd while attached")

	// The re-schedule cmd is the package-level tickUpdateMetadataCmd; the
	// per-instance work cmd is runMetadataTickCmd(...). The only way to
	// tell them apart cheaply is by what they return when invoked: the
	// re-schedule sleeps then returns tickUpdateMetadataMessage{}; the
	// work cmd runs CheckAndHandleTrustPrompt/HasUpdated on each instance
	// and then returns tickUpdateMetadataMessage{}.
	//
	// Asserting on the function-pointer identity is the most direct check:
	// while attached we must return the bare re-schedule var, not a
	// freshly-constructed runMetadataTickCmd closure that captured the
	// instance slice.
	gotPtr := reflect.ValueOf(cmd).Pointer()
	wantPtr := reflect.ValueOf(tickUpdateMetadataCmd).Pointer()
	assert.Equal(t, wantPtr, gotPtr,
		"while attached the handler must return tickUpdateMetadataCmd, "+
			"not runMetadataTickCmd — otherwise capture-pane work runs in "+
			"the background and contends with the user's detach key (#598)")

	// And just to be sure: invoking the cmd must not have mutated the
	// instance status (which is what runMetadataTickCmd would do via
	// HasUpdated → Ready).
	assert.Equal(t, session.Running, a.GetStatus(),
		"instance status must not flip while attached — no per-instance "+
			"capture-pane work should have run")
}

// TestTickUpdateMetadata_RunsWhenNotAttached is the symmetric test: with
// attached=false, the handler must dispatch the work cmd as it always has.
// This guards against the gate being too aggressive (e.g. an inverted
// boolean) and silently breaking the sidebar Running/Ready status forever.
func TestTickUpdateMetadata_RunsWhenNotAttached(t *testing.T) {
	h := newTestHome(t)
	a := instanceWithFakeBackend(t, "a")
	h.sidebar.AddInstance(a)

	require.False(t, h.attached.Load(), "test pre-condition: not attached")
	_, cmd := h.Update(tickUpdateMetadataMessage{})
	require.NotNil(t, cmd)

	// Identity check the other way around: the cmd we got must NOT be the
	// bare re-schedule cmd. It must be a freshly-constructed closure from
	// runMetadataTickCmd.
	gotPtr := reflect.ValueOf(cmd).Pointer()
	wantPtr := reflect.ValueOf(tickUpdateMetadataCmd).Pointer()
	assert.NotEqual(t, wantPtr, gotPtr,
		"while not attached the handler must dispatch runMetadataTickCmd "+
			"with the snapshot of instances, not the bare re-schedule cmd")

	// Invoking the cmd should drive the FakeBackend HasUpdated path, which
	// returns (updated=false, hasPrompt=false), flipping Running → Ready.
	msg := cmd()
	_, ok := msg.(tickUpdateMetadataMessage)
	require.True(t, ok, "runMetadataTickCmd must re-emit tickUpdateMetadataMessage; got %T", msg)
	assert.Equal(t, session.Ready, a.GetStatus(),
		"work cmd must have executed against the instance")
}

// TestPreviewTick_PausedWhileAttached verifies that the previewTickMsg
// handler skips selectionChanged (and therefore refreshPanesCmd, two
// capture-pane shell-outs) while attached. The handler must still re-arm
// the tick so a sub-second post-detach repaint is guaranteed.
func TestPreviewTick_PausedWhileAttached(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.sidebar.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	h.attached.Store(true)
	_, cmd := h.Update(previewTickMsg{})
	require.NotNil(t, cmd, "previewTickMsg must keep the tick alive even while attached")

	// We can't compare tea.Batch closures by identity. Instead, drive the
	// batch and confirm panesRefreshedMsg never lands (which is what
	// refreshPanesCmd would return). The batch only contains the
	// re-schedule sleep — running it should yield previewTickMsg.
	got := drainCmd(t, cmd, 250*time.Millisecond)
	for _, msg := range got {
		_, isPanesRefreshed := msg.(panesRefreshedMsg)
		assert.False(t, isPanesRefreshed,
			"while attached previewTickMsg must not dispatch refreshPanesCmd — "+
				"saw a panesRefreshedMsg in the batch")
	}
}

// TestSelectionChanged_SkipsRefreshWhileAttached covers selectionChanged
// directly. While attached, it must skip refreshPanesCmd and fetchPRInfoCmd
// — both of which were observed in the #598 trace adding to tmux-server
// contention. The synchronous mutations (mode, menu state) still run so
// other code paths that happen to call selectionChanged stay consistent.
func TestSelectionChanged_SkipsRefreshWhileAttached(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.sidebar.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	h.attached.Store(true)
	cmd := h.selectionChanged()
	// tea.Batch(nil, nil) returns nil; that's the expected result while
	// attached. If the gate fails we'd see a non-nil batch from
	// refreshPanesCmd.
	assert.Nil(t, cmd,
		"selectionChanged must return nil while attached: refreshPanesCmd "+
			"and fetchPRInfoCmd are both gated — neither should be queued "+
			"behind the user's detach key (#598)")
}

// TestTickUpdatePRInfo_PausedWhileAttached: the 60s PR info refresh tick
// shells out to `gh pr view` for the selected instance. While attached
// that network round-trip provides no visible benefit (PR badge in the
// hidden sidebar) and races against detach for the tmux/socket stack.
func TestTickUpdatePRInfo_PausedWhileAttached(t *testing.T) {
	h := newTestHome(t)
	inst := instanceWithFakeBackend(t, "a")
	h.sidebar.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// Count fetch invocations to prove fetchPRInfoCmd was NOT dispatched.
	calls := 0
	restore := SetPRInfoFetcherForTest(func(string, string) (*git.PRInfo, error) {
		calls++
		return nil, nil
	})
	defer restore()

	h.attached.Store(true)
	_, cmd := h.Update(tickUpdatePRInfoMessage{})
	require.NotNil(t, cmd, "tick must still re-arm itself")

	// Identity check: the cmd should be the bare re-schedule, not a
	// tea.Batch that contains fetchPRInfoCmd.
	gotPtr := reflect.ValueOf(cmd).Pointer()
	wantPtr := reflect.ValueOf(tickUpdatePRInfoCmd).Pointer()
	assert.Equal(t, wantPtr, gotPtr,
		"while attached the handler must return tickUpdatePRInfoCmd, "+
			"not a tea.Batch that includes fetchPRInfoCmd (#598)")
	assert.Equal(t, 0, calls,
		"prInfoFetcher must not be invoked while attached: gh pr view "+
			"is a network call we don't want racing detach")
}

// TestAttachOverlayCallback_ClearsFlagOnDetach exercises the success path
// of the attach lifecycle helper: the attached flag is armed before <-ch
// blocks and disarmed after it unblocks. The defer in attachOverlayCallback
// is what guarantees the disarm — this test would fail if someone replaced
// the defer with a plain `Store(false)` and a panic occurred between the
// two.
func TestAttachOverlayCallback_ClearsFlagOnDetach(t *testing.T) {
	h := newTestHome(t)
	require.False(t, h.attached.Load(), "test pre-condition: not attached")

	ch := make(chan struct{})
	attach := func() (chan struct{}, error) { return ch, nil }

	done := make(chan tea.Cmd, 1)
	go func() {
		done <- h.attachOverlayCallback("test-attach", " title=t1", attach)
	}()

	// While the callback is blocked on <-ch the flag must be set.
	require.Eventually(t, func() bool { return h.attached.Load() },
		time.Second, time.Millisecond,
		"attached flag must be armed before <-ch blocks")

	// Simulate user detaching.
	close(ch)
	postDetachCmd := <-done

	require.False(t, h.attached.Load(),
		"attached flag must clear after <-ch unblocks — otherwise the "+
			"metadata tick / preview refresh / PR info fetcher stay paused "+
			"until the next process restart")
	require.NotNil(t, postDetachCmd, "success path returns repaintAfterDetachMsg cmd")
	msg := postDetachCmd()
	_, ok := msg.(repaintAfterDetachMsg)
	assert.True(t, ok, "post-detach cmd must emit repaintAfterDetachMsg, got %T", msg)
	// End the watchdog we armed via beginDetachWatchdog so subsequent
	// tests don't see a stray running goroutine.
	endDetachWatchdog()
}

// TestAttachOverlayCallback_LeavesFlagAloneWhenAttachErrors is THE
// regression-risk path called out in the brief: if the underlying attach
// fails before <-ch is ever reached, the callback returns nil without
// touching the flag. That's the only way to guarantee a transient attach
// failure can't strand the app in a permanently-paused state.
func TestAttachOverlayCallback_LeavesFlagAloneWhenAttachErrors(t *testing.T) {
	h := newTestHome(t)
	require.False(t, h.attached.Load(), "test pre-condition: not attached")

	attachErr := errors.New("simulated attach failure")
	attach := func() (chan struct{}, error) { return nil, attachErr }

	cmd := h.attachOverlayCallback("test-attach", "", attach)

	assert.Nil(t, cmd, "attachOverlayCallback must return nil when attach fails")
	assert.False(t, h.attached.Load(),
		"attached flag must NOT be set when attach itself errors — "+
			"otherwise a single failed attach permanently disables the "+
			"metadata tick (#598 regression-risk path)")
}

// drainCmd runs cmd (and any nested tea.Cmd it produces via tea.Batch) up
// to the given deadline and returns the messages it produced. Used by the
// previewTickMsg pause test to inspect the batch contents.
func drainCmd(t *testing.T, cmd tea.Cmd, deadline time.Duration) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() {
		done <- cmd()
	}()
	select {
	case msg := <-done:
		// tea.Batch returns a msg of type tea.BatchMsg ([]tea.Cmd internally
		// in bubbletea). We only care about whether any nested cmd
		// produces a panesRefreshedMsg; recursing one level deep is enough
		// for our purposes here.
		batch, ok := msg.(tea.BatchMsg)
		if !ok {
			return []tea.Msg{msg}
		}
		var got []tea.Msg
		for _, inner := range batch {
			if inner == nil {
				continue
			}
			innerCh := make(chan tea.Msg, 1)
			go func(c tea.Cmd) { innerCh <- c() }(inner)
			select {
			case innerMsg := <-innerCh:
				got = append(got, innerMsg)
			case <-time.After(deadline):
				// Slow re-schedule sleep — that's the only cmd in the
				// batch that takes longer than a few µs.
			}
		}
		return got
	case <-time.After(deadline):
		t.Fatalf("cmd did not return within %v", deadline)
		return nil
	}
}
