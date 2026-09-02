package apiclient

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Client built for ONE round-trip and dropped does not release its connection.
// The completed keep-alive socket stays in its transport's idle pool with the
// read-loop goroutine that owns it, and that goroutine keeps the transport
// reachable — so nothing is collected and the descriptor is gone for the life of
// the process. Every TUI seam builds exactly that shape, and two of them run on
// the 750ms poll, so it compounds at ~4 descriptors per second (#3626 review).
//
// This measures the real thing rather than asserting a call was made: a real
// unix-socket server, real round-trips, descriptors counted off /proc.
func fdCount(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	return len(fds)
}

func idleConnServer(t *testing.T) string {
	t.Helper()
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/Snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.SnapshotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, apiproto.Envelope{Data: json.RawMessage(`{"instances":[]}`)})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sockPath
}

func TestCloseIdleConnectionsStopsTheOneShotClientLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor counting reads /proc/self/fd")
	}
	sockPath := idleConnServer(t)
	const calls = 100

	// Warm first: the first few round-trips allocate whatever the runtime needs
	// once, and counting that as a leak would make this test lie in the safe
	// direction (which is still lying).
	for i := 0; i < 5; i++ {
		c := NewWithSocket(sockPath)
		require.NoError(t, c.call("Snapshot", daemon.SnapshotRequest{}, &daemon.SnapshotResponse{}))
		c.CloseIdleConnections()
	}

	// First the DEFECT, so this test is known to be able to see one: the same loop
	// without the close. If this stops leaking, the assertion below has stopped
	// meaning anything and should fail here instead of passing silently.
	unclosed := fdCount(t)
	for i := 0; i < calls; i++ {
		c := NewWithSocket(sockPath)
		require.NoError(t, c.call("Snapshot", daemon.SnapshotRequest{}, &daemon.SnapshotResponse{}))
	}
	require.Greater(t, fdCount(t)-unclosed, calls,
		"the probe cannot see the leak it is guarding against, so its verdict is worthless")

	before := fdCount(t)
	for i := 0; i < calls; i++ {
		c := NewWithSocket(sockPath)
		require.NoError(t, c.call("Snapshot", daemon.SnapshotRequest{}, &daemon.SnapshotResponse{}))
		c.CloseIdleConnections()
	}
	// The close is asynchronous at the far end; give the server side a moment to
	// reap its half before counting, or this measures timing rather than leaking.
	time.Sleep(500 * time.Millisecond)
	leaked := fdCount(t) - before

	// A per-call leak would be ~200 here (both ends). The bound is deliberately
	// loose — this counts the WHOLE process, so a sibling goroutine opening a file
	// must not fail it — while still being an order of magnitude below the defect.
	assert.Less(t, leaked, 20,
		"one-shot clients that close their idle connections must not leak per call; leaked %d over %d calls", leaked, calls)
}
