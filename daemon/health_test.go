package daemon

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

// stubDialUnix replaces the package dial seam for one test, making every dial
// return (conn, err), and restores the real dialer on cleanup.
//
// It lets a test inject a DETERMINISTIC dial outcome instead of manufacturing
// one from the OS. An earlier version saturated a listener's accept backlog to
// force a real connect timeout; that works on Linux but never fires on Darwin,
// whose kernel completes handshakes past the nominal backlog — the timeout test
// then failed to set up and went red on macOS CI (#2039). The classification is
// pure error-shape logic, so injecting the exact error a real dial produces
// tests the real thing without depending on kernel backlog behavior.
func stubDialUnix(t *testing.T, conn net.Conn, err error) {
	t.Helper()
	prev := dialUnix
	t.Cleanup(func() { dialUnix = prev })
	dialUnix = func(string, string, time.Duration) (net.Conn, error) { return conn, err }
}

// listenUnix creates a real Unix listener at path so probeHTTPSocket's os.Stat
// gate passes and it proceeds to the dial. Closed on cleanup.
func listenUnix(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
}

// A dial that TIMES OUT is not evidence that nothing is listening: a live
// listener with a saturated accept backlog times out too. Collapsing that into a
// definite No is the exact timeout-is-not-a-negative fabrication #1920 set out
// to kill, one field over — it drives `af doctor` to an actionable "run af
// daemon restart" over a live-but-busy listener. The timeout must be
// Undetermined ("unknown"), never a made-up No (#2014).
//
// The timeout is INJECTED, mirroring the exact error net.DialTimeout returns on
// a deadline expiry (an *net.OpError whose Timeout() is true and which
// Is(os.ErrDeadlineExceeded)) — not manufactured from the OS, which is not
// portable (#2039).
//
// Fail-first: against the pre-fix classification this returned AnswerNo(), so
// this asserted "unknown" and FAILED.
func TestProbeHTTPSocket_DialTimeoutIsUndeterminedNotNo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path, err := DaemonHTTPSocketPath()
	require.NoError(t, err)
	listenUnix(t, path) // the socket file must exist to reach the dial

	timeoutErr := &net.OpError{Op: "dial", Net: "unix", Addr: &net.UnixAddr{Name: path, Net: "unix"}, Err: os.ErrDeadlineExceeded}
	require.True(t, os.IsTimeout(timeoutErr) || errors.Is(timeoutErr, os.ErrDeadlineExceeded),
		"the injected error must be exactly what the production classification keys on")
	stubDialUnix(t, nil, timeoutErr)

	gotPath, exists, listening := probeHTTPSocket()

	require.Equal(t, path, gotPath)
	require.True(t, exists, "the socket file is present on disk")
	requireAnswer(t, "unknown", listening,
		"a dial that timed out is not evidence that nothing is listening (#2014)")
	require.Error(t, listening.Cause(), "an undetermined answer must carry its cause")
	require.Contains(t, listening.Cause().Error(), path, "the cause names the socket it could not reach")
}

// The dominant real case — a socket file with no listener behind it — is a
// REFUSAL (ECONNREFUSED): a completed answer that nobody is home. That must stay
// a definite No; the fix must not blur it into "unknown". Injected so it is
// deterministic on every platform, mirroring a real refused dial's error shape.
func TestProbeHTTPSocket_DialRefusedIsNo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path, err := DaemonHTTPSocketPath()
	require.NoError(t, err)
	listenUnix(t, path)

	refusedErr := &net.OpError{Op: "dial", Net: "unix", Addr: &net.UnixAddr{Name: path, Net: "unix"},
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	require.False(t, os.IsTimeout(refusedErr) || errors.Is(refusedErr, os.ErrDeadlineExceeded),
		"a refusal must not look like a timeout to the classification")
	stubDialUnix(t, nil, refusedErr)

	gotPath, exists, listening := probeHTTPSocket()

	require.Equal(t, path, gotPath)
	require.True(t, exists)
	requireAnswer(t, "no", listening, "a refused dial is a completed answer that nothing is listening")
}

// The control-socket ping's timeout must be Undetermined, not a definite No —
// the same fix as probeHTTPSocket, one path over (#2040). A momentarily
// backlogged control socket times out exactly like a dead one, so collapsing
// the timeout into a Fail would send a user to `af daemon restart` over a live
// daemon. The error injected here is the exact shape net.DialTimeout returns on
// a deadline expiry.
func TestClassifyPingFailure_TimeoutIsUndetermined(t *testing.T) {
	timeoutErr := &net.OpError{Op: "dial", Net: "unix", Err: os.ErrDeadlineExceeded}
	require.True(t, os.IsTimeout(timeoutErr) || errors.Is(timeoutErr, os.ErrDeadlineExceeded),
		"the injected error must be exactly what the production classification keys on")

	answer := ClassifyPingFailure(timeoutErr)
	requireAnswer(t, "unknown", answer,
		"a dial timeout is not evidence the daemon is dead (#2040)")
	require.Error(t, answer.Cause(), "an undetermined answer must carry its cause")
}

// A refusal (ECONNREFUSED) is a completed answer — nobody is home — and must
// stay a definite No that drives the Fail. The fix must not blur it into
// "unknown".
func TestClassifyPingFailure_RefusalIsNo(t *testing.T) {
	refusedErr := &net.OpError{Op: "dial", Net: "unix",
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	require.False(t, os.IsTimeout(refusedErr) || errors.Is(refusedErr, os.ErrDeadlineExceeded),
		"a refusal must not look like a timeout to the classification")

	requireAnswer(t, "no", ClassifyPingFailure(refusedErr),
		"a refused ping is a completed answer that nothing is responding")
}

// A nil error is Yes: a daemon answered.
func TestClassifyPingFailure_NilIsYes(t *testing.T) {
	requireAnswer(t, "yes", ClassifyPingFailure(nil), "a nil ping error means a daemon answered")
}

// A real listener that actually accepts answers Yes — the happy path, run
// through the REAL dial (no stub) so the seam's production wiring is covered.
func TestProbeHTTPSocket_LiveListenerIsYes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path, err := DaemonHTTPSocketPath()
	require.NoError(t, err)
	listenUnix(t, path)

	_, exists, listening := probeHTTPSocket()

	require.True(t, exists)
	requireAnswer(t, "yes", listening)
}

// No socket file at all is a definite answer (nothing to listen on), not a
// failure to look — it stays No, without ever reaching the dial.
func TestProbeHTTPSocket_MissingSocketIsNo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	_, exists, listening := probeHTTPSocket()

	require.False(t, exists)
	requireAnswer(t, "no", listening, "nothing to listen on is a definite answer, not unknown")
}
