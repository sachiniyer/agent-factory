package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The post-attach receipt watcher scans CODEX_HOME with filepath.WalkDir and
// os.ReadFile. Neither takes a context, so cancellation is only observed BETWEEN
// scans: while a scan is in flight against a stalled filesystem the watcher is
// uninterruptible for as long as that I/O blocks. An unbounded join in Stop or
// reap therefore parks daemon teardown in front of the very tmux sessions it
// exists to kill, and the config agent outlives the daemon.
//
// stallingReceiptWatch is that watcher: it ignores its context and blocks until
// the test ends. Because watchReceipt only closes `done` after watch returns,
// the wedge holds regardless of goroutine scheduling.
func stallingReceiptWatch(t *testing.T) func(context.Context) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(context.Context) { <-release }
}

// startTrackedConfigAgentSession registers a real (isolated) tmux session with a
// fresh supervisor plus a receipt watcher that will never unwind. The session is
// real so the assertions can prove teardown actually got PAST the join — a Stop
// that returns without killing what it owns would be a different bug wearing
// this one's clothes.
func startTrackedConfigAgentSession(t *testing.T, name string) (*configAgentSupervisor, *tmux.TmuxSession) {
	t.Helper()
	testguard.IsolateTmux(t)

	ts := tmux.NewTmuxSession(name, "sh")
	require.NoError(t, ts.Start(t.TempDir()))
	t.Cleanup(func() { _, _ = ts.Close() })

	c := newConfigAgentSupervisor()
	require.True(t, c.track(name, ts))
	c.watchReceipt(name, stallingReceiptWatch(t))
	return c, ts
}

func shortenReceiptJoinTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := configAgentReceiptJoinTimeout
	configAgentReceiptJoinTimeout = d
	t.Cleanup(func() { configAgentReceiptJoinTimeout = prev })
}

func TestConfigAgentStopDoesNotWedgeOnStalledReceiptScan(t *testing.T) {
	_, warnings := captureConfigAgentReceiptLogs(t)
	shortenReceiptJoinTimeout(t, 150*time.Millisecond)
	c, ts := startTrackedConfigAgentSession(t, "af-2758-stop-wedge")

	done := make(chan struct{})
	go func() { defer close(done); c.Stop() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("configAgentSupervisor.Stop hung joining a receipt watcher blocked in filesystem " +
			"I/O: daemon shutdown never reaches the bare tmux sessions it owns, so the config " +
			"agent survives the daemon")
	}

	require.Contains(t, warnings.String(), "did not unwind within 150ms",
		"abandoning a wedged watcher leaves a live goroutine behind; that must be reported, not silent")
	exists, known := ts.ProbeSession()
	require.True(t, known, "tmux must have answered the post-shutdown existence probe")
	require.False(t, exists,
		"shutdown must still kill the config-agent session it owns; the bounded join exists to let it")
}

func TestConfigAgentReapDoesNotWedgeOnStalledReceiptScan(t *testing.T) {
	shortenReceiptJoinTimeout(t, 150*time.Millisecond)
	c, ts := startTrackedConfigAgentSession(t, "af-2758-reap-wedge")

	done := make(chan error, 1)
	go func() { done <- c.reap("af-2758-reap-wedge") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("configAgentSupervisor.reap hung joining a receipt watcher blocked in filesystem " +
			"I/O: the TUI's return from the config-agent takeover never completes")
	}

	exists, known := ts.ProbeSession()
	require.True(t, known, "tmux must have answered the post-reap existence probe")
	require.False(t, exists, "reap must still close the config-agent session")
}
