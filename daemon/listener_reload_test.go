package daemon

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestWebListenerReadsConfigExactlyOncePerRequest is the STRUCTURAL pin for the
// single-snapshot-per-request rule (#2480 PR2): the live handler reads the auth +
// CORS posture EXACTLY ONCE per request, so a config swap landing mid-request can
// never split one authorization across two generations (require_token from one,
// require_loopback_token or cors from the next). A future edit that reintroduces a
// second live read makes this count 2 and trips.
func TestWebListenerReadsConfigExactlyOncePerRequest(t *testing.T) {
	var reads int32
	build := func() requestPosture {
		atomic.AddInt32(&reads, 1)
		return requestPosture{} // nil gate ⇒ authorized; empty cors
	}
	served := false
	h := withLivePosture(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}), build)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))

	require.True(t, served, "the request must reach the wrapped handler")
	require.Equal(t, int32(1), atomic.LoadInt32(&reads),
		"the live handler must read the posture exactly once per request (op-entry rule for the request)")
}

// boundWebListeners builds a manager with cfg and binds its web listener through
// webListeners, returning the manager, the listeners, and the resolved bound
// address. Cleanup closes the listeners.
func boundWebListeners(t *testing.T, cfg *config.Config) (*Manager, *webListeners, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	t.Cleanup(func() { _ = wl.close() })
	addr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, addr, "web listener must be bound")
	return m, wl, addr
}

// getStatus issues a plain GET to http://addr/path and returns the status code.
func getStatus(t *testing.T, addr, path string) int {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestWebListenerAuthAppliesLiveWithoutRebind proves the decisive property of the
// live-read design (#2480 PR2): a require_token / require_loopback_token tighten
// applies to the NEXT request with no socket rebind — decoupled from the listener,
// so a tightening can never fail in the permissive direction because a rebind failed.
func TestWebListenerAuthAppliesLiveWithoutRebind(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.RequireToken = false
	cfg.RequireLoopbackToken = false
	m, _, addr := boundWebListeners(t, cfg)

	// require_token=false ⇒ a tokenless request is authorized.
	require.Equal(t, http.StatusOK, getStatus(t, addr, "/v1/health"))

	// Tighten live: token mandatory for every peer, loopback included. The address
	// is unchanged, so NO rebind — the live handler reads the new posture per request.
	tightened := *m.Config()
	tightened.RequireToken = true
	tightened.RequireLoopbackToken = true
	m.live.Store(&tightened)

	require.Equal(t, http.StatusUnauthorized, getStatus(t, addr, "/v1/health"),
		"a require_token/require_loopback_token tighten must apply on the next request with no rebind")
	require.Equal(t, addr, m.lifecycle.snapshot().listeners.TCPBoundAddr,
		"the auth tighten must NOT rebind the socket — it is decoupled from the listener")
}

// TestWebListenerRebindsOnListenAddrChange: a listen_addr change rebinds the socket
// in place — the new address serves and the old stops.
func TestWebListenerRebindsOnListenAddrChange(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))

	freeAddr := grabFreeLoopbackAddr(t)
	next := *m.Config()
	next.ListenAddr = freeAddr
	m.live.Store(&next)

	failed, err := wl.reconcile(&next)
	require.NoError(t, err)
	require.Empty(t, failed)

	newAddr := m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.Equal(t, freeAddr, newAddr, "the listener must rebind to the new address")
	require.Equal(t, http.StatusOK, getStatus(t, newAddr, "/v1/health"), "the new listener serves")

	_, err = http.Get("http://" + oldAddr + "/v1/health")
	require.Error(t, err, "the old listener must stop after a successful rebind")
}

// TestWebListenerRebindFailureKeepsOldListenerServing is THE brick-prevention pin
// (#2480 PR2): a rebind to an unbindable address must keep the OLD listener serving
// and return an actionable error, never leave the daemon unreachable through the
// very API used to fix the address. Bind-new-before-close is the whole point.
func TestWebListenerRebindFailureKeepsOldListenerServing(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))

	// Occupy a port so a rebind onto it MUST fail with "address already in use".
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()
	occupied := blocker.Addr().String()

	next := *m.Config()
	next.ListenAddr = occupied
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "a rebind onto an occupied port must fail")
	require.Contains(t, rerr.Error(), occupied, "the error must name the address")
	require.Contains(t, failed, "listen_addr")

	// The property: the OLD listener is still serving — the daemon is not bricked.
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"),
		"bind-new-before-close: a failed rebind must keep the old listener serving")
	require.Equal(t, oldAddr, m.lifecycle.snapshot().listeners.TCPBoundAddr,
		"lifecycle must still report the old address after a failed rebind")
}

func TestWebListenersRebindSameAddressAfterListenerDeath(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0"
	m, wl, _ := boundWebListeners(t, cfg)

	tests := []struct {
		name      string
		getCloser func() func() error
		isBound   func() bool
	}{
		{
			name: "control",
			getCloser: func() func() error {
				wl.mu.Lock()
				defer wl.mu.Unlock()
				closer := wl.webClose
				return closer
			},
			isBound: func() bool { return m.lifecycle.snapshot().listeners.TCPBound },
		},
		{
			name: "preview",
			getCloser: func() func() error {
				wl.mu.Lock()
				defer wl.mu.Unlock()
				closer := wl.previewClose
				return closer
			},
			isBound: func() bool { return m.lifecycle.snapshot().listeners.PreviewBound },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closer := tt.getCloser()
			require.NotNil(t, closer)
			require.NoError(t, closer())
			require.Eventually(t, func() bool { return !tt.isBound() }, time.Second, 10*time.Millisecond,
				"the done watcher must observe listener death")

			failed, err := wl.reconcile(m.Config())
			require.NoError(t, err)
			require.Empty(t, failed)
			require.Eventually(t, tt.isBound, time.Second, 10*time.Millisecond,
				"reconciling the unchanged configured address must replace the dead listener")
		})
	}
}

type readObservedListener struct {
	net.Listener
	readStarted chan struct{}
}

func (l *readObservedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &readObservedConn{Conn: conn, readStarted: l.readStarted}, nil
}

type readObservedConn struct {
	net.Conn
	readStarted chan struct{}
	started     atomic.Bool
}

func (c *readObservedConn) Read(p []byte) (int, error) {
	if c.started.CompareAndSwap(false, true) {
		close(c.readStarted)
	}
	return c.Conn.Read(p)
}

func TestWebListenersRetainCloserAfterUnexpectedListenerDeath(t *testing.T) {
	for _, kind := range []string{"control", "preview"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			cfg := config.DefaultConfig()
			cfg.ListenAddr = ""
			cfg.PreviewListenAddr = ""
			if kind == "control" {
				cfg.ListenAddr = "127.0.0.1:0"
			} else {
				cfg.PreviewListenAddr = "127.0.0.1:0"
			}

			m, err := NewManager(cfg)
			require.NoError(t, err)
			wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
			m.webListeners = wl

			var listener *readObservedListener
			wl.listenTCP = func(network, address string) (net.Listener, error) {
				bound, listenErr := net.Listen(network, address)
				if listenErr != nil {
					return nil, listenErr
				}
				listener = &readObservedListener{Listener: bound, readStarted: make(chan struct{})}
				return listener, nil
			}
			failed, err := wl.reconcile(m.Config())
			require.NoError(t, err)
			require.Empty(t, failed)
			t.Cleanup(func() { _ = wl.close() })

			state := m.lifecycle.snapshot().listeners
			addr := state.TCPBoundAddr
			if kind == "preview" {
				addr = state.PreviewBoundAddr
			}
			conn, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			defer conn.Close()
			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n")
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				select {
				case <-listener.readStarted:
					return true
				default:
					return false
				}
			}, time.Second, 10*time.Millisecond, "the server must accept and begin reading the connection")

			require.NoError(t, listener.Close(), "fail the listener without calling the server closer")
			require.Eventually(t, func() bool {
				wl.mu.Lock()
				defer wl.mu.Unlock()
				if kind == "control" {
					return wl.webConfigAddr == ""
				}
				return wl.previewConfigAddr == ""
			}, time.Second, 10*time.Millisecond, "the done watcher must observe listener death")

			require.NoError(t, conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
			_, err = conn.Read(make([]byte, 1))
			var netErr net.Error
			require.ErrorAs(t, err, &netErr)
			require.True(t, netErr.Timeout(), "the accepted connection must survive listener death before shutdown")

			require.NoError(t, wl.close())
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
			_, err = conn.Read(make([]byte, 1))
			require.Error(t, err)
			require.False(t, errors.As(err, &netErr) && netErr.Timeout(),
				"daemon shutdown must still reach and close the accepted connection")
		})
	}
}

func TestWebListenersDisableClosesRetainedServerAfterUnexpectedListenerDeath(t *testing.T) {
	for _, kind := range []string{"control", "preview"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			cfg := config.DefaultConfig()
			cfg.ListenAddr = ""
			cfg.PreviewListenAddr = ""
			if kind == "control" {
				cfg.ListenAddr = "127.0.0.1:0"
			} else {
				cfg.PreviewListenAddr = "127.0.0.1:0"
			}

			m, err := NewManager(cfg)
			require.NoError(t, err)
			wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
			m.webListeners = wl

			var listener *readObservedListener
			wl.listenTCP = func(network, address string) (net.Listener, error) {
				bound, listenErr := net.Listen(network, address)
				if listenErr != nil {
					return nil, listenErr
				}
				listener = &readObservedListener{Listener: bound, readStarted: make(chan struct{})}
				return listener, nil
			}
			failed, err := wl.reconcile(m.Config())
			require.NoError(t, err)
			require.Empty(t, failed)
			t.Cleanup(func() { _ = wl.close() })

			state := m.lifecycle.snapshot().listeners
			addr := state.TCPBoundAddr
			if kind == "preview" {
				addr = state.PreviewBoundAddr
			}
			conn, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			defer conn.Close()
			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n")
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				select {
				case <-listener.readStarted:
					return true
				default:
					return false
				}
			}, time.Second, 10*time.Millisecond, "the server must accept and begin reading the connection")

			require.NoError(t, listener.Close(), "fail the listener without calling the server closer")
			require.Eventually(t, func() bool {
				wl.mu.Lock()
				defer wl.mu.Unlock()
				if kind == "control" {
					return wl.webConfigAddr == "" && wl.webClose != nil
				}
				return wl.previewConfigAddr == "" && wl.previewClose != nil
			}, time.Second, 10*time.Millisecond, "listener death must leave only the accepted-connection closer")

			disabled := *m.Config()
			if kind == "control" {
				disabled.ListenAddr = ""
			} else {
				disabled.PreviewListenAddr = ""
			}
			m.live.Store(&disabled)
			failed, err = wl.reconcile(&disabled)
			require.NoError(t, err)
			require.Empty(t, failed)

			require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
			_, err = conn.Read(make([]byte, 1))
			var netErr net.Error
			require.Error(t, err)
			require.False(t, errors.As(err, &netErr) && netErr.Timeout(),
				"disabling a dead listener must close its retained accepted connections")
		})
	}
}

// TestApplyConfigTokenlessNetworkWarnsAndBinds: a tokenless non-loopback address is
// WARNED about at save time and BINDS — never refused (#2168 Phase 0; the #2556
// correction the plan called out). ApplyConfig surfaces the exposure notice as a
// warning and the listener comes up on the network address.
func TestApplyConfigTokenlessNetworkWarnsAndBinds(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, _, _ := boundWebListeners(t, cfg)

	// Move to a tokenless network bind on an ephemeral port on all interfaces.
	_, err := config.SetGlobalConfigValue("listen_addr", "0.0.0.0:0")
	require.NoError(t, err)
	_, err = config.SetGlobalConfigValue("require_token", "false")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err, "a tokenless network bind must NOT be refused (#2168)")
	require.Empty(t, result.FailedListenerKeys, "the network bind must succeed, not fail")

	exposed := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "require_token is false") {
			exposed = true
		}
	}
	require.True(t, exposed, "the tokenless-network exposure notice must be surfaced at save time, got %v", result.Warnings)
	require.NotEmpty(t, m.lifecycle.snapshot().listeners.TCPBoundAddr, "the listener must have bound the network address")
}

// grabFreeLoopbackAddr returns a currently-free 127.0.0.1:port address by binding
// and immediately releasing it. A tiny race exists (the port could be taken before
// the caller rebinds it); it is acceptable for a test on an otherwise-idle box.
func grabFreeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}
