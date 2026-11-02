package daemon

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// retryTestListeners builds listeners whose bind boundary fails the first
// `failures` attempts with the error a daemon racing tailscaled for its tailnet
// IP sees, then binds for real on loopback. The backoff is shrunk so a retry
// lands inside the test.
func retryTestListeners(t *testing.T, failures int32) (*Manager, *webListeners, *atomic.Int32) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	prevInitial, prevMax := listenRetryInitial, listenRetryMax
	listenRetryInitial, listenRetryMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { listenRetryInitial, listenRetryMax = prevInitial, prevMax })

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = ""
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	var calls atomic.Int32
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if calls.Add(1) <= failures {
			return nil, errors.New("listen tcp: cannot assign requested address")
		}
		return net.Listen(network, address)
	}
	t.Cleanup(func() { _ = wl.close() })
	return m, wl, &calls
}

// A startup bind that fails is retried until it succeeds (W9.9, topo:T10), the
// failure is visible in the status snapshot meanwhile, and the status clears
// once the listener is up.
func TestWebListenerStartupBindFailureIsRetriedUntilItBinds(t *testing.T) {
	m, wl, calls := retryTestListeners(t, 3)

	failed, err := wl.reconcile(m.Config())
	require.Error(t, err)
	require.Equal(t, []string{"network.listen_addr"}, failed)
	wl.retryFailedBinds()

	st := m.lifecycle.snapshot().listeners
	require.False(t, st.TCPBound)
	require.Contains(t, st.TCPBindError, "cannot assign requested address",
		"a listener that lost the startup race must show why in the status snapshot")

	require.Eventually(t, func() bool {
		s := m.lifecycle.snapshot().listeners
		return s.TCPBound && s.TCPBindError == "" && !s.BindRetrying
	}, 5*time.Second, 5*time.Millisecond, "the retry must bind the listener and clear the pending status")
	require.GreaterOrEqual(t, calls.Load(), int32(4), "three failed attempts, then the successful one")
	require.Equal(t, 200, getStatus(t, m.lifecycle.snapshot().listeners.TCPBoundAddr, "/v1/health"))
}

// Shutdown ends the retry: no further bind attempts, and nothing is bound after
// close even if a retry was mid-flight.
func TestWebListenerRetryStopsAtClose(t *testing.T) {
	m, wl, calls := retryTestListeners(t, 1<<20)

	_, err := wl.reconcile(m.Config())
	require.Error(t, err)
	wl.retryFailedBinds()
	require.Eventually(t, func() bool { return calls.Load() >= 3 }, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, wl.close())
	require.Eventually(t, func() bool { return !m.lifecycle.snapshot().listeners.BindRetrying },
		5*time.Second, 5*time.Millisecond, "close must stop the retry goroutine")
	after := calls.Load()
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, after, calls.Load(), "no bind attempts after close")
	require.False(t, m.lifecycle.snapshot().listeners.TCPBound)
}

// Turning the listener off in config ends the retry without binding anything:
// the retry always reconciles against the CURRENT config.
func TestWebListenerRetryEndsWhenListenerIsConfiguredOff(t *testing.T) {
	m, wl, _ := retryTestListeners(t, 1<<20)

	_, err := wl.reconcile(m.Config())
	require.Error(t, err)
	wl.retryFailedBinds()

	off := *m.Config()
	off.ListenAddr = ""
	m.storeLivePosture(&off)

	require.Eventually(t, func() bool {
		s := m.lifecycle.snapshot().listeners
		return !s.BindRetrying && s.TCPBindError == ""
	}, 5*time.Second, 5*time.Millisecond)
	require.False(t, m.lifecycle.snapshot().listeners.TCPBound)
}

// A retry and a live config apply must never interleave: the retry's config
// snapshot and its reconcile are one step under configApplyMu, the lock
// ApplyConfig holds across its own swap + reconcile. Hold that lock as an
// in-flight apply would, swap the config to "listener off" under it, and check
// the retry neither binds while the apply is in flight (the old interleave let it
// reconcile a config captured before the apply) nor resurrects the old address
// afterwards.
func TestWebListenerRetryIsSerializedWithConfigApply(t *testing.T) {
	m, wl, calls := retryTestListeners(t, 0)
	// First bind fails so the retry starts; every later attempt would succeed,
	// which is what makes a stale-config bind observable.
	var failNext atomic.Bool
	failNext.Store(true)
	realListen := wl.listenTCP
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if failNext.Swap(false) {
			calls.Add(1)
			return nil, errors.New("listen tcp: cannot assign requested address")
		}
		calls.Add(1)
		return realListen(network, address)
	}
	_, err := wl.reconcile(m.Config())
	require.Error(t, err)

	m.configApplyMu.Lock() // an ApplyConfig is in flight
	attempts := calls.Load()
	wl.retryFailedBinds()
	time.Sleep(150 * time.Millisecond) // many backoff ticks (5-20ms each)
	callsDuringApply := calls.Load()

	off := *m.Config()
	off.ListenAddr = ""
	m.storeLivePosture(&off)
	m.configApplyMu.Unlock()

	require.Equal(t, attempts, callsDuringApply,
		"the retry must not bind while a config apply holds configApplyMu")

	require.Eventually(t, func() bool { return !m.lifecycle.snapshot().listeners.BindRetrying },
		5*time.Second, 5*time.Millisecond)
	require.False(t, m.lifecycle.snapshot().listeners.TCPBound,
		"the retry must apply the config the apply stored, not bind the one it captured earlier")
	require.Equal(t, attempts, calls.Load(), "no bind attempt for a listener configured off")
}
