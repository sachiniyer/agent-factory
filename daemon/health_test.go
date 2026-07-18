package daemon

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// fillUnixAcceptBacklog binds a listening Unix socket at path with the smallest
// possible accept backlog and NEVER accepts, then holds connections open until
// the queue is saturated — after which every further dial blocks until its
// deadline and returns a genuine, kernel-produced connect timeout.
//
// This is the exact real-world shape #2014 is about: a listener that is present
// (the socket answers connect() attempts up to the backlog) but momentarily
// unable to service a new connection within the 250ms probe bound. A hand-forged
// "timeout" error would test nothing; this drives the real thing.
func fillUnixAcceptBacklog(t *testing.T, path string) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	require.NoError(t, unix.Bind(fd, &unix.SockaddrUnix{Name: path}))
	require.NoError(t, unix.Listen(fd, 0)) // smallest backlog; never accept

	// Fill until a dial actually TIMES OUT. Stopping on the first timeout PROVES
	// the backlog is saturated before the code under test runs, rather than
	// assuming a particular kernel's rounding of backlog 0. Every established
	// connection is held open for the life of the test so the queue stays full.
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; ; i++ {
		require.Less(t, i, 64, "could not saturate the accept backlog")
		require.True(t, time.Now().Before(deadline), "could not saturate the accept backlog in time")
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err != nil {
			require.True(t, os.IsTimeout(err) || errors.Is(err, os.ErrDeadlineExceeded),
				"the fill dial failed for a non-timeout reason, so this fixture is not exercising the timeout path: %v", err)
			return // saturated: the next dial (the code under test) will also time out
		}
		held := conn
		t.Cleanup(func() { _ = held.Close() })
	}
}

// A dial that TIMES OUT is not evidence that nothing is listening: a listener
// present with a saturated accept backlog times out too. Collapsing that into a
// definite No is the exact timeout-is-not-a-negative fabrication #1920 set out
// to kill, one field over — it drives `af doctor` to an actionable "run af
// daemon restart" over a live-but-busy listener. The timeout must be
// Undetermined ("unknown"), never a made-up No (#2014).
//
// This is the fail-first lock: against the pre-fix code probeHTTPSocket returned
// AnswerNo() here, so this asserted "unknown" and FAILED.
func TestProbeHTTPSocket_DialTimeoutIsUndeterminedNotNo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path, err := DaemonHTTPSocketPath()
	require.NoError(t, err)
	fillUnixAcceptBacklog(t, path)

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
// a definite No; the fix must not blur it into "unknown".
func TestProbeHTTPSocket_DialRefusedIsNo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path, err := DaemonHTTPSocketPath()
	require.NoError(t, err)

	// Bind but never listen: the socket file exists, so os.Stat sees it, but a
	// dial is refused because the socket is not in LISTEN state.
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	require.NoError(t, unix.Bind(fd, &unix.SockaddrUnix{Name: path}))

	gotPath, exists, listening := probeHTTPSocket()

	require.Equal(t, path, gotPath)
	require.True(t, exists)
	requireAnswer(t, "no", listening, "a refused dial is a completed answer that nothing is listening")
}

// A real listener that actually accepts answers Yes — the happy path, proving
// the fixture and the fix leave the good case untouched.
func TestProbeHTTPSocket_LiveListenerIsYes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	path, err := DaemonHTTPSocketPath()
	require.NoError(t, err)
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	_, exists, listening := probeHTTPSocket()

	require.True(t, exists)
	requireAnswer(t, "yes", listening)
}

// No socket file at all is a definite answer (nothing to listen on), not a
// failure to look — it stays No.
func TestProbeHTTPSocket_MissingSocketIsNo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	_, exists, listening := probeHTTPSocket()

	require.False(t, exists)
	requireAnswer(t, "no", listening, "nothing to listen on is a definite answer, not unknown")
}
