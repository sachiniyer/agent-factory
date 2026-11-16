package tmux

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// reflowProbeTmux is the slice of tmux a viewer-resize test needs:
// display-message answers the pane's current size, resize-window applies a new
// one (and models the server: only a size-CHANGING apply reflows anything),
// and capture-pane answers whatever content the test last put in pane. The
// pane bytes themselves are the test's to script — the size bookkeeping and
// the stamp decision under test are real.
type reflowProbeTmux struct {
	size    string // display-message's pane_width x pane_height answer
	probe   string // overrides size for display-message when set (a malformed answer)
	pane    string // capture-pane's answer
	resizes int    // resize-window calls applied
	// onResizeApply runs inside the resize-window apply itself — after the
	// size has changed but before the call returns — so a test can take the
	// capture a poll would land mid-apply.
	onResizeApply func()
}

func (m *reflowProbeTmux) exec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			args := strings.Join(c.Args, " ")
			if strings.Contains(args, "resize-window") {
				m.resizes++
				// Apply the requested size the way a real server does.
				var x, y string
				for i, a := range c.Args {
					switch a {
					case "-x":
						if i+1 < len(c.Args) {
							x = c.Args[i+1]
						}
					case "-y":
						if i+1 < len(c.Args) {
							y = c.Args[i+1]
						}
					}
				}
				m.size = x + "x" + y
				if m.onResizeApply != nil {
					m.onResizeApply()
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			args := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(args, "display-message"):
				if m.probe != "" {
					return []byte(m.probe), nil
				}
				return []byte(m.size + "\n"), nil
			case strings.Contains(args, "capture-pane"):
				return []byte(m.pane), nil
			}
			return []byte("output"), nil
		},
	}
}

func newReflowSession(t *testing.T, tm *reflowProbeTmux) *TmuxSession {
	t.Helper()
	session := newTmuxSession(toTmuxName("reflow", ""), ProgramCodex, NewMockPtyFactory(t), tm.exec())
	session.monitor = newStatusMonitor()
	return session
}

// ageReflowStamp moves the monitor's last resize stamp past reflowGrace so the
// next capture is classified by the epoch alone, the state the poll is in a
// few seconds after the apply.
func ageReflowStamp(t *testing.T, session *TmuxSession) {
	t.Helper()
	session.monitorMu.Lock()
	session.monitor.lastAppliedResizeAt = time.Now().Add(-time.Hour)
	session.monitorMu.Unlock()
}

// TestResizeReflowMarksChangedCapture is the #7434 core: an attach/refit
// resize reflows the pane, and the changed capture it produces must carry the
// reflow mark so the daemon can hold it out of the work evidence — the repaint
// is churn, not the agent's output.
func TestResizeReflowMarksChangedCapture(t *testing.T) {
	tm := &reflowProbeTmux{size: "80x24", pane: "short line"}
	session := newReflowSession(t, tm)

	updated, _, _, _, captured, reflow := session.HasUpdatedObserved(nil)
	require.True(t, updated, "the first capture establishes the baseline frame")
	require.True(t, captured)
	require.False(t, reflow)

	require.NoError(t, session.ResizeWindow(120, 40))
	tm.pane = "short line rewrapped at 120"
	updated, _, _, _, captured, reflow = session.HasUpdatedObserved(nil)
	require.True(t, captured)
	require.True(t, updated, "the reflowed frame IS still an update — status and readiness read it")
	require.True(t, reflow, "a changed capture after a size-changing resize is the viewer's repaint, not work")
}

// TestReflowGraceBoundsTheRepaintTail covers both edges of the grace: a
// SIGWINCH repaint landing a capture after the one that consumed the epoch
// mismatch is still viewing's, while real output past the grace is work again.
func TestReflowGraceBoundsTheRepaintTail(t *testing.T) {
	tm := &reflowProbeTmux{size: "80x24", pane: "short line"}
	session := newReflowSession(t, tm)
	session.HasUpdatedObserved(nil)

	require.NoError(t, session.ResizeWindow(120, 40))
	tm.pane = "reflowed once"
	_, _, _, _, _, reflow := session.HasUpdatedObserved(nil)
	require.True(t, reflow, "the epoch mismatch marks the first reflowed capture")

	tm.pane = "the app's repaint answering SIGWINCH a poll late"
	updated, _, _, _, _, reflow := session.HasUpdatedObserved(nil)
	require.True(t, updated)
	require.True(t, reflow, "the repaint tail inside reflowGrace is still the viewer's")

	ageReflowStamp(t, session)
	tm.pane = "real agent output"
	updated, _, _, _, _, reflow = session.HasUpdatedObserved(nil)
	require.True(t, updated)
	require.False(t, reflow, "past the grace a changed capture is the agent's work again")
}

// TestResizeSameSizeDoesNotStamp is the review amendment (#7434): every web
// subscribe and refit resends the CURRENT size with force=true. That apply
// changes nothing and must open no grace — a stamp there would mask real work
// for a client that refits often.
func TestResizeSameSizeDoesNotStamp(t *testing.T) {
	tm := &reflowProbeTmux{size: "120x40", pane: "content at rest"}
	session := newReflowSession(t, tm)
	session.HasUpdatedObserved(nil)

	// The resize-window apply still runs — the client's assertion is real —
	// but with no size change it must not stamp the monitor.
	require.NoError(t, session.ResizeWindow(120, 40))
	require.Equal(t, 1, tm.resizes)

	tm.pane = "real agent output inside the would-be grace"
	updated, _, _, _, _, reflow := session.HasUpdatedObserved(nil)
	require.True(t, updated)
	require.False(t, reflow, "a same-size apply opens no grace: real output still answers the request")
}

// TestResizeUnparseableProbeStamps pins the conservative direction: a size
// probe that answers something unreadable cannot prove the apply was a no-op,
// so the stamp lands and the next changed capture reads as reflow rather than
// gambling the request on a guess.
func TestResizeUnparseableProbeStamps(t *testing.T) {
	tm := &reflowProbeTmux{size: "80x24", pane: "content", probe: "not-a-size"}
	session := newReflowSession(t, tm)
	session.HasUpdatedObserved(nil)

	require.NoError(t, session.ResizeWindow(120, 40))
	tm.pane = "reflowed"
	updated, _, _, _, _, reflow := session.HasUpdatedObserved(nil)
	require.True(t, updated)
	require.True(t, reflow, "an unreadable pre-size stamps conservatively — preserving the request")
}

// TestResizeWithoutReflowLeavesNoStaleEpoch pins the every-capture epoch sync:
// a size-changing apply whose pane bytes did not move (short content, nothing
// to rewrap) must not leave a mismatch behind that marks the NEXT real output
// as a repaint.
func TestResizeWithoutReflowLeavesNoStaleEpoch(t *testing.T) {
	tm := &reflowProbeTmux{size: "80x24", pane: "a line too short to rewrap"}
	session := newReflowSession(t, tm)
	session.HasUpdatedObserved(nil)

	require.NoError(t, session.ResizeWindow(200, 50))
	updated, _, _, _, _, reflow := session.HasUpdatedObserved(nil)
	require.False(t, updated, "the pane's bytes did not move: nothing reflowed")
	require.False(t, reflow)

	ageReflowStamp(t, session)
	tm.pane = "real agent output"
	updated, _, _, _, _, reflow = session.HasUpdatedObserved(nil)
	require.True(t, updated)
	require.False(t, reflow, "the epoch already synced on the unchanged capture — real work is not a repaint")
}

// TestResizeStampIsEpochOrdered pins the ordering the mechanism relies on: a
// resize stamped AFTER a capture can never mark that capture — attribution
// only flows forward, so a poll that straddled the apply reads exactly one
// side of it.
func TestResizeStampIsEpochOrdered(t *testing.T) {
	tm := &reflowProbeTmux{size: "80x24", pane: "frame one"}
	session := newReflowSession(t, tm)
	session.HasUpdatedObserved(nil)

	tm.pane = "frame two — the agent's own output, before any resize"
	updated, _, _, _, _, reflow := session.HasUpdatedObserved(nil)
	require.True(t, updated)
	require.False(t, reflow, "output that predates the resize cannot be its repaint")

	require.NoError(t, session.ResizeWindow(120, 40))
	tm.pane = "frame three — the resize's reflow"
	_, _, _, _, _, reflow = session.HasUpdatedObserved(nil)
	require.True(t, reflow)
}

// TestResizeStampPrecedesTheApply is the #7437-review straddle: the status
// poll shares no lock with ResizeWindow, so a capture that lands while the
// server is mid-apply sees the rewrapped grid BEFORE the call returns. Only a
// stamp issued before resize-window runs classifies that frame as the
// viewer's repaint — a post-apply-only stamp leaves it reading as the agent's
// churn, which answers a live attention request with no work behind it.
func TestResizeStampPrecedesTheApply(t *testing.T) {
	tm := &reflowProbeTmux{size: "80x24", pane: "steady frame"}
	session := newReflowSession(t, tm)
	session.HasUpdatedObserved(nil)

	// The capture the poll takes while resize-window is still in flight: the
	// server has already applied the size (the mock's size moved above) and
	// the pane already shows the rewrap, but the apply has not returned —
	// the post-apply stamp has not run yet either.
	var midApplyUpdated, midApplyReflow bool
	tm.onResizeApply = func() {
		tm.pane = "the in-flight apply's rewrap"
		_, _, _, _, _, midApplyReflow = session.HasUpdatedObserved(nil)
		midApplyUpdated = true
	}
	require.NoError(t, session.ResizeWindow(120, 40))
	require.True(t, midApplyUpdated, "the mid-apply capture ran")
	require.True(t, midApplyReflow,
		"a capture that can see the resized grid must also see the stamp — the pre-apply stamp classifies the rewrap")

	// The post-apply stamp still rebased the grace: a changed capture now is
	// inside it, and one past it is the agent's work again.
	ageReflowStamp(t, session)
	tm.pane = "real agent output"
	updated, _, _, _, _, reflow := session.HasUpdatedObserved(nil)
	require.True(t, updated)
	require.False(t, reflow, "the grace aged out — real work answers")
}
