package daemon

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestSecondHomeDaemonNeverTakesDefaultPortFromForeignDaemon is the #6858
// regression: two agent-factory homes, one default web address. A daemon
// under a SECOND home (a non-default AGENT_FACTORY_HOME) with only the
// default network.listen_addr finds the port held by the first home's
// daemon — and must NEVER take it, including the moment the first daemon's
// restart releases it. Before the fix the #6386 retry loop kept re-attempting
// the bind and won the port in exactly that restart gap, leaving the real
// daemon the one stuck retrying while the user's proxy, bookmarks and tokens
// pointed at the wrong daemon.
//
// The simulation uses real kernel state end to end — no stubbed classifier:
// this test process stands in for the first daemon by holding (a) a real
// listening socket on the contested port and (b) a real, flock'd daemon.lock
// in a DIFFERENT af home — the exact fd evidence the holder classification
// keys on. The shipped default port is never bound: defaultWebListenAddr is
// re-pointed at a free loopback port for the duration (#6625), which is also
// what makes "the first daemon's restart" representable as a close().
func TestSecondHomeDaemonNeverTakesDefaultPortFromForeignDaemon(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("port-holder identification reads /proc on linux")
	}
	// Our home is a throwaway that is NOT the default ~/.agent-factory — the
	// second-home posture the gate protects.
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	// The contested address: a real loopback port standing in for the shipped
	// default. The first daemon holds it for now — a real LISTEN socket in
	// this process's fd table.
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	contested := holder.Addr().String()
	oldDefault := defaultWebListenAddr
	defaultWebListenAddr = contested
	t.Cleanup(func() { defaultWebListenAddr = oldDefault })

	// The first daemon's home marker: a live, flock'd daemon.lock in a home
	// that is not ours. A real af daemon holds exactly this fd for its whole
	// lifetime, and its path IS the home — the identity the classification
	// reads without trusting argv or environment spelling.
	foreignHome := testguard.SocketTempDir(t)
	lock, err := os.OpenFile(filepath.Join(foreignHome, daemonLockFileName), os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	defer func() { _ = lock.Close() }()

	// Shrink the retry backoff so the retry loop actually runs inside the
	// test window instead of parking on its 1s→30s schedule.
	oldInit, oldMax := listenRetryInitial, listenRetryMax
	listenRetryInitial, listenRetryMax = 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { listenRetryInitial, listenRetryMax = oldInit, oldMax })

	// The second daemon: default web address (per the re-pointed default),
	// preview disabled, under the non-default home set above.
	cfg := config.DefaultConfig()
	cfg.ListenAddr = contested
	cfg.PreviewListenAddr = ""
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	t.Cleanup(func() { _ = wl.close() })

	// The first bind attempt meets the foreign-held port. The retry loop is
	// what startHTTPServer arms on a failed startup bind (#6386).
	_, _ = wl.reconcile(m.Config())
	wl.retryFailedBinds()

	// The first daemon "restarts": it releases the contested port.
	require.NoError(t, holder.Close())

	snap := func() DaemonListenerStatus { return m.lifecycle.snapshot().listeners }
	// The property: for the whole settle window the second daemon must stay
	// unbound. On master the retry loop binds the freed port — the takeover —
	// within a backoff tick, so this fails fast there. With the fix the bind
	// is a terminal refusal and nothing is ever bound.
	require.Never(t, func() bool { return snap().TCPBound },
		400*time.Millisecond, 10*time.Millisecond,
		"the second-home daemon bound %s — a port another AF home's daemon held when it first tried — the #6858 takeover",
		contested)
	// The give-up must explain itself: the refusal is recorded on the
	// lifecycle status `af daemon status` renders and `af doctor` reads, and
	// it names the remedy — set [network] listen_addr to another port.
	require.NotEmpty(t, snap().TCPRefusalReason,
		"a refused takeover must say why: the lifecycle's TCPRefusalReason should name the foreign-home holder")
	require.Contains(t, snap().TCPRefusalReason, "network.listen_addr",
		"the refusal must name the fix — set [network] listen_addr to another port")
}
