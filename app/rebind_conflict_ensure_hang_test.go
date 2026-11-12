package app

import (
	"context"
	"net"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// productionAllReposSnapshotFetcher captures the production default at package
// load, before any test swaps the allReposSnapshotFetcher seam. The conflict
// ensure-hang reproduction must run the REAL fetch chain — withDaemonHTTPCtx →
// daemonHTTPClientCtx → daemon.EnsureDaemonUntil — rather than a stubbed
// fetcher, so the ensure phase's deadline coverage is what's under test.
var productionAllReposSnapshotFetcher = allReposSnapshotFetcher

// startWedgedControlSocketInHome binds a unix listener at the home's
// daemon.sock that Accept()s every connection and parks it without any I/O — a
// control socket whose listener is bound but whose RPC layer never services a
// request. This is the precise "accepts the connection and never answers"
// shape the rebindConflictRefreshTimeout doc comment claims to be safe against,
// and the same shape daemon/runtime_files_bugconfirm_test.go uses to expose the
// unbounded ping. The kernel accepts a client's dial into the backlog before the
// server's accept goroutine runs, so the client's request write is buffered and
// the client blocks reading a response that never comes — the one socket shape
// against which an unbounded control-socket ping is observable.
func startWedgedControlSocketInHome(t *testing.T) {
	t.Helper()
	socketPath, err := daemon.DaemonSocketPath()
	require.NoError(t, err)
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				<-ctx.Done()
				_ = c.Close()
			}(conn)
		}
	}()
}

// TestRebindConflictRefreshBoundedEnsureReArmsTheForm is the end-to-end
// regression for the rebind conflict refresh hang (#4888 follow-up). The
// off-loop snapshot fetch the conflict re-arm waits on goes through
// daemonHTTPClientCtx → daemon.EnsureDaemonUntil BEFORE the ctx-bounded HTTP
// read, and a wedged control socket (accepts the dial, never services the RPC)
// makes that ensure phase block forever when it runs under a zero deadline.
// Pre-fix the fetch's rebindConflictRefreshTimeout bounded only the HTTP read
// downstream of the ensure; the ensure's ping parked on an unanswered RPC read
// with no conn.SetDeadline, the reply never landed, and the rebind form stayed
// inert (rebindPending) — with the wedged holder also pinning ensureDaemonMu
// against every later daemon operation in the process.
//
// Post-fix the ensure phase inherits the fetch's deadline via
// daemon.EnsureDaemonUntil, so the wedged ping's conn.SetDeadline fires at the
// deadline, the ensure returns, the fetch reports a failed read, and the form
// re-arms on the daemon's refusal within the bound — not after Ctrl+C. The
// existing TestRebindConflictRefreshTimeoutReArmsTheForm passes the same harness
// only because it replaces the fetcher seam wholesale, bypassing
// daemonHTTPClientCtx/EnsureDaemon; this test runs the production chain.
func TestRebindConflictRefreshBoundedEnsureReArmsTheForm(t *testing.T) {
	h, id := activeRebindHome(t)
	displayed := h.projectPickerOverlay

	// Reinstall the PRODUCTION fetch chain. newTestHome stubs
	// allReposSnapshotFetcher wholesale, which bypasses withDaemonHTTPCtx →
	// daemonHTTPClientCtx → EnsureDaemonUntil — exactly the ensure step the
	// hang lives in. The production default is captured at package load, before
	// newTestHome's stub.
	prevFetcher := allReposSnapshotFetcher
	allReposSnapshotFetcher = productionAllReposSnapshotFetcher
	t.Cleanup(func() { allReposSnapshotFetcher = prevFetcher })

	// Shrink the bound so the regression is fast: the wedged ping must return
	// at this deadline, not at a real 10s. Well above the ensure's dial+RPC
	// floor so the wedged read actually parks before the deadline fires, and
	// well below the test-level cutoff so a hang fatals quickly.
	const timeout = 300 * time.Millisecond
	oldTimeout := rebindConflictRefreshTimeout
	rebindConflictRefreshTimeout = timeout
	t.Cleanup(func() { rebindConflictRefreshTimeout = oldTimeout })

	// The "rebound elsewhere" refusal: another client rebinds the project, then
	// the picker's rebind carries the (root, checkout) pair it displayed and the
	// daemon refuses it.
	moved := initTestGitRepo(t)
	_, err := config.RebindProject(id, moved)
	require.NoError(t, err)
	stubRebindCAS(t)

	// Wedge the control socket the ensure phase pings. The daemon just answered
	// the rebind refusal; this is the window where it accepts and never answers.
	startWedgedControlSocketInHome(t)

	_, cmd := h.Update(submitPickerRebind(t, h, initTestGitRepo(t))())
	require.NotNil(t, cmd, "the conflict refresh is dispatched off the event loop")
	assert.True(t, displayed.RebindPending(), "the form stays inert while the refresh is in flight")

	// Run the refresh beside a test-level bound. Pre-fix the goroutine never
	// returns (the ensure's ping parks on the wedged RPC read with no
	// SetDeadline), and the time.After branch fatals. Post-fix the ensure's
	// deadline fires at ~timeout, the fetch reports a failed read, and the
	// reply re-arms the form well inside the bound.
	replied := make(chan tea.Msg, 1)
	go func() { replied <- cmd() }()
	select {
	case snapshotMsg := <-replied:
		h.Update(snapshotMsg)
	case <-time.After(timeout + 5*time.Second):
		t.Fatalf("the conflict refresh must be bounded across the ensure phase — the wedged control socket left the fetch pending for >%s", timeout+5*time.Second)
	}

	assert.False(t, displayed.RebindPending(),
		"a refresh whose ensure ping never answers must still re-arm the form at the deadline")
	displayed.SetMaxSize(400, 60)
	displayed.SetWidth(200)
	assert.Contains(t, displayed.Render(), "Rebound elsewhere",
		"the re-armed form names the daemon's refusal")
}
