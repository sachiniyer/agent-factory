package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// startWedgedControlSocket binds a unix listener at DaemonSocketPath() that
// Accept()s every connection and parks it open without ever reading or
// writing — a control socket whose listener is bound but whose RPC layer
// never services a request. This is exactly the net.Listen→Accept stall
// window startControlServer opens: the kernel accepts a client's dial into
// the backlog before the server's accept goroutine runs, so the client's
// request write is buffered and the client blocks reading a response that
// never comes. It is the one socket shape against which an unbounded
// control-socket ping is observable — a healthy server answers in
// microseconds and an absent socket refuses the dial in microseconds, so
// neither exposes a missing client-side timeout.
func startWedgedControlSocket(t *testing.T) {
	t.Helper()
	socketPath, err := DaemonSocketPath()
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
			// Hold the accepted connection open with no I/O until
			// cleanup, mirroring a bound-but-stalled accept loop.
			go func(c net.Conn) {
				<-ctx.Done()
				_ = c.Close()
			}(conn)
		}
	}()
}

// writeRuntimeFile is the small fixture both wedged-socket regressions share:
// a daemon.pid naming the PID the stop accounted for, on the temp home.
func writeRuntimeFile(t *testing.T, home string, pid int) string {
	t.Helper()
	pidFile := filepath.Join(home, "daemon.pid")
	require.NoError(t, os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0600))
	return pidFile
}

// TestCleanupDaemonRuntimeFiles_ZeroDeadlineDoesNotHangOnWedgedSocket is the
// regression for the unbounded FIRST control-socket probe in
// cleanupDaemonRuntimeFiles. The public StopDaemon path (af daemon stop, af
// reset) passes time.Time{} here; pre-fix that zero deadline flowed straight
// into pingDaemonUntil → callDaemonNoEnsureAttemptBefore, which skipped
// conn.SetDeadline (the RPC deadline is gated on !deadline.IsZero()), so
// against a wedged listener the dial succeeded and client.Call blocked
// forever — hanging StopDaemon. Post-fix the first probe is floored at
// socketRecheckBudget, so SetDeadline always runs, the timed-out read is
// classified indeterminate ("could not look"), and the runtime files are
// left in place with cleanup returning within the budget instead of hanging.
//
// Pre-fix this hangs and the time.After branch fatals; post-fix <-done wins.
func TestCleanupDaemonRuntimeFiles_ZeroDeadlineDoesNotHangOnWedgedSocket(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	startWedgedControlSocket(t)

	pidFile := writeRuntimeFile(t, home, 12345)
	socketPath, err := DaemonSocketPath()
	require.NoError(t, err)

	// Bound well above socketRecheckBudget (2s) + dial + slack but well
	// below "forever": pre-fix the goroutine never returns and this select
	// fatals; post-fix the bounded probe returns in ~socketRecheckBudget.
	const bound = 6 * time.Second
	done := make(chan struct{})
	start := time.Now()
	go func() {
		cleanupDaemonRuntimeFiles(pidFile, 12345, time.Time{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(bound):
		t.Fatalf("cleanupDaemonRuntimeFiles(time.Time{}) hung for %s on a wedged socket; the first probe is unbounded (no SetDeadline on the RPC conn)", bound)
	}
	elapsed := time.Since(start)

	// The probe must actually wait for its read deadline against the
	// wedged socket — the dial succeeds, so a near-instant return would
	// mean the deadline was either never applied or far too short.
	if elapsed < socketRecheckBudget/2 {
		t.Fatalf("the first probe returned in %s on a wedged socket; it should wait for its read deadline (~%s)", elapsed, socketRecheckBudget)
	}
	if elapsed >= bound {
		t.Fatalf("the first probe is not bounded by socketRecheckBudget; cleanup returned in %s", elapsed)
	}

	// A wedged socket is "could not look", not "nobody home": unreachable
	// is not gone, so both runtime files must be left in place — the same
	// stance the second probe takes. Unlinking here would delete a live
	// successor's socket (#767).
	require.FileExists(t, pidFile, "cleanup removed the PID file after a wedged-socket probe (indeterminate, not absent)")
	require.FileExists(t, socketPath, "cleanup removed the socket file after a wedged-socket probe (indeterminate, not absent)")
}

// TestCleanupDaemonRuntimeFiles_NonZeroDeadlineCappedAtRecheckBudget is the
// companion regression for the non-zero-deadline (EnsureDaemon reclaim) path.
// Pre-fix the first probe forwarded the raw caller deadline, which WAS
// applied as SetDeadline but NOT capped at socketRecheckBudget, so a wedged
// socket could consume the entire remaining admission budget before the
// safety-critical second probe (adjacent to the unlink) ever ran — a long
// first-probe stall left the second probe with no budget, tripping its
// probe<=0 short-circuit and leaving the socket untouched after a stop that
// took the full admission budget instead of the intended ~2s. Post-fix the
// first probe is capped at min(socketRecheckBudget, remaining).
//
// The deadline (4s) sits above socketRecheckBudget (2s); the cutoff (3s) sits
// between them, so a capped probe is already done while an uncapped one is
// still blocked on its 4s read.
func TestCleanupDaemonRuntimeFiles_NonZeroDeadlineCappedAtRecheckBudget(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	startWedgedControlSocket(t)

	pidFile := writeRuntimeFile(t, home, 12345)

	const (
		callerDeadline = 4 * time.Second // > socketRecheckBudget
		cutoff         = 3 * time.Second // between socketRecheckBudget and callerDeadline
	)
	done := make(chan struct{})
	start := time.Now()
	go func() {
		cleanupDaemonRuntimeFiles(pidFile, 12345, time.Now().Add(callerDeadline))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cutoff):
		t.Fatalf("cleanupDaemonRuntimeFiles took >%s on a wedged socket with a %s deadline; the first probe is not capped at socketRecheckBudget", cutoff, callerDeadline)
	}
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, socketRecheckBudget/2,
		"the capped probe should still wait for its read deadline, not return near-instantly (elapsed=%s)", elapsed)
	assert.Less(t, elapsed, cutoff,
		"the first probe must be capped at socketRecheckBudget (~%s), not the full %s caller deadline (elapsed=%s)",
		socketRecheckBudget, callerDeadline, elapsed)

	require.FileExists(t, pidFile, "a wedged-socket probe is indeterminate; the PID file must be left in place")
}

// TestCleanupDaemonRuntimeFiles_ShortDeadlineNotExtendedOnWedgedSocket
// guards the min() arm of the cap: a non-zero caller deadline SHORTER than
// socketRecheckBudget must not be extended up to the budget. The first probe
// uses min(socketRecheckBudget, remaining), so a 500ms deadline against a
// wedged socket returns in ~500ms, not ~2s. This is the branch the cap test
// above does not exercise (its deadline exceeds the budget).
func TestCleanupDaemonRuntimeFiles_ShortDeadlineNotExtendedOnWedgedSocket(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	startWedgedControlSocket(t)

	pidFile := writeRuntimeFile(t, home, 12345)

	const (
		shortDeadline = 500 * time.Millisecond  // < socketRecheckBudget (2s)
		upperBound    = 1200 * time.Millisecond // well above shortDeadline, well below socketRecheckBudget
	)
	done := make(chan struct{})
	start := time.Now()
	go func() {
		cleanupDaemonRuntimeFiles(pidFile, 12345, time.Now().Add(shortDeadline))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(upperBound):
		t.Fatalf("cleanupDaemonRuntimeFiles took >%s with a %s deadline; the first probe was extended beyond the caller's remaining budget", upperBound, shortDeadline)
	}
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, shortDeadline/2,
		"the probe should still wait for its read deadline, not return near-instantly (elapsed=%s)", elapsed)
	assert.Less(t, elapsed, upperBound,
		"a %s caller deadline must not be extended to socketRecheckBudget; cleanup returned in %s", shortDeadline, elapsed)

	require.FileExists(t, pidFile, "a wedged-socket probe is indeterminate; the PID file must be left in place")
}
