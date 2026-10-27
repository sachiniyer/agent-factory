package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
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
	require.Contains(t, failed, "network.listen_addr")

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
		getHandle func() *tcpListenerHandle
		isBound   func() bool
	}{
		{
			name: "control",
			getHandle: func() *tcpListenerHandle {
				wl.mu.Lock()
				defer wl.mu.Unlock()
				return wl.webHandle
			},
			isBound: func() bool { return m.lifecycle.snapshot().listeners.TCPBound },
		},
		{
			name: "preview",
			getHandle: func() *tcpListenerHandle {
				wl.mu.Lock()
				defer wl.mu.Unlock()
				return wl.previewHandle
			},
			isBound: func() bool { return m.lifecycle.snapshot().listeners.PreviewBound },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handle := tt.getHandle()
			require.NotNil(t, handle)
			require.NoError(t, handle.close())
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

			require.NoError(t, listener.Close(), "fail the listener without tearing the server down through its handle")
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
			withRetireGrace(t, 200*time.Millisecond)
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

			require.NoError(t, listener.Close(), "fail the listener without tearing the server down through its handle")
			require.Eventually(t, func() bool {
				wl.mu.Lock()
				defer wl.mu.Unlock()
				if kind == "control" {
					return wl.webConfigAddr == "" && wl.webHandle != nil
				}
				return wl.previewConfigAddr == "" && wl.previewHandle != nil
			}, time.Second, 10*time.Millisecond, "listener death must leave only the handle owning the accepted connections")

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

			// The connection now dies at the RETIREMENT DEADLINE rather than the
			// instant reconcile runs (#3722): a config-driven teardown retires its
			// listener so an in-flight reply can flush, and this connection is
			// holding a half-written request that will never finish, so it is
			// exactly the stalled client the deadline exists to evict. The property
			// under test is unchanged — the retained connection must not survive —
			// and shortening the grace keeps the assertion tight rather than
			// widening the read deadline until anything passes. The read window is
			// generous against that 200ms grace on purpose: a slow runner can only
			// make the close LATE, never early, so waiting longer costs nothing and
			// still fails outright if the connection is never closed at all.
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
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
// and immediately releasing it. Callers that require a later bind to succeed must
// retry the complete reserve-and-bind operation; retrying this reservation alone
// cannot close the release/rebind race.
func grabFreeLoopbackAddr(t *testing.T) string {
	t.Helper()
	addr, err := freeLoopbackAddr()
	require.NoError(t, err)
	return addr
}

func freeLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		return "", err
	}
	return addr, nil
}

// The same-port rebind tests (#5140). A listen_addr move that keeps the port and
// overlaps the old bind — wildcard ↔ specific — can never bind-new-before-close:
// a wildcard owns every address on its port, so the new socket meets the old one
// still holding it. Those pairs take the inverse order: release the old listener,
// bind the new, and re-bind the old on failure, so the daemon never reports a
// same-port move as deferred while a port its own socket could have freed is the
// only thing in the way.

// boundPort extracts the kernel-chosen port from a resolved bound address.
func boundPort(t *testing.T, boundAddr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(boundAddr)
	require.NoError(t, err)
	return port
}

// TestSetListenAddrSamePortNarrowingRepliesOnTheListenerItMoves is the reported
// defect (#5140) end to end over the wire: a remote SetConfigValue narrowing
// network.listen_addr on the SAME port — 0.0.0.0:P to a specific :P — must come
// back 200 on the old connection, name the new address as now-serving, and leave
// it answering. The old listener was released before the new bind, so this is
// also the pin that releasing first still drains the reply that caused it.
//
// RED on master: bind-new-before-close has the new specific bind meet the
// wildcard still owning every address on the port — EADDRINUSE, deferred, and
// the operator is told the daemon is "still serving" an address that nothing
// new will ever reach.
func TestSetListenAddrSamePortNarrowingRepliesOnTheListenerItMoves(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	m, _, oldBound := boundWebListeners(t, cfg)
	port := boundPort(t, oldBound)
	// Dial the wildcard listener over loopback.
	dialAddr := "127.0.0.1:" + port
	require.Equal(t, http.StatusOK, getStatus(t, dialAddr, "/v1/health"))

	newAddr := "127.0.0.1:" + port
	status, env, raw := postSetConfigValue(t, dialAddr, "network.listen_addr", newAddr)

	require.Equal(t, http.StatusOK, status, "body: %s", raw)
	require.Nil(t, env.Error, "body: %s", raw)
	require.NotNil(t, env.Data, "body: %s", raw)
	require.NotNil(t, env.Data.Result)
	require.Equal(t, newAddr, env.Data.Result.Value, "the reply must echo the value that was written")
	require.Equal(t, newAddr, env.Data.ListenerAddr,
		"the reply must name the address the daemon is now accepting on")

	require.Equal(t, http.StatusOK, getStatus(t, newAddr, "/v1/health"),
		"a same-port narrowing must apply live — the new address serves")
	require.Equal(t, newAddr, m.lifecycle.snapshot().listeners.TCPBoundAddr)
}

// TestWebListenerSamePortNarrowingAppliesLive pins the reconcile-level move:
// wildcard → specific on the same port rebinds in place rather than failing.
func TestWebListenerSamePortNarrowingAppliesLive(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	m, wl, oldBound := boundWebListeners(t, cfg)
	port := boundPort(t, oldBound)
	require.Equal(t, http.StatusOK, getStatus(t, "127.0.0.1:"+port, "/v1/health"))

	newAddr := "127.0.0.1:" + port
	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, err := wl.reconcile(&next)
	require.NoError(t, err, "a same-port narrowing must apply live (#5140)")
	require.Empty(t, failed)

	require.Equal(t, newAddr, m.lifecycle.snapshot().listeners.TCPBoundAddr)
	require.Equal(t, http.StatusOK, getStatus(t, newAddr, "/v1/health"))
}

// TestWebListenerSamePortWideningAppliesLive is the mirror image: specific →
// wildcard on the same port fails bind-first identically (the wildcard meets
// the old specific bind), so it takes the same release-then-bind path.
func TestWebListenerSamePortWideningAppliesLive(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))

	newAddr := "0.0.0.0:" + port
	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, err := wl.reconcile(&next)
	require.NoError(t, err, "a same-port widening must apply live (#5140)")
	require.Empty(t, failed)

	// The bound address is the wildcard — which the kernel may report as the
	// IPv4 "0.0.0.0" or the dual-stack "[::]" depending on platform — on the
	// SAME port.
	boundHost, boundP, err := net.SplitHostPort(m.lifecycle.snapshot().listeners.TCPBoundAddr)
	require.NoError(t, err)
	require.Equal(t, port, boundP)
	require.True(t, net.ParseIP(boundHost).IsUnspecified(),
		"the bound host must be a wildcard, got %q", boundHost)
	require.Equal(t, http.StatusOK, getStatus(t, "127.0.0.1:"+port, "/v1/health"),
		"the widened wildcard listener still answers on loopback")
}

// TestWebListenerSamePortRebindFailureRestoresOldListener: when the SAME-port
// new bind cannot succeed even with the port released — here a third party
// holds the new address, expressed through the listen seam — the old address is
// re-bound and the daemon keeps serving on it. The property under test is
// stronger than "still serving": the listener had to be released for the retry,
// so the handle that answers afterwards must be a NEW one — the rollback — not
// the socket that was never touched.
func TestWebListenerSamePortRebindFailureRestoresOldListener(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	newAddr := "0.0.0.0:" + port
	// Deny ONLY the requested address, so the release-then-bind retry fails
	// EADDRINUSE and the rollback re-bind of the old address must restore it.
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if address == newAddr {
			return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
		}
		return net.Listen(network, address)
	}

	wl.mu.Lock()
	released := wl.webHandle
	wl.mu.Unlock()

	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "the same-port rebind must report failure")
	require.Contains(t, rerr.Error(), "daemon still serving on the previous address")
	require.Contains(t, failed, "network.listen_addr")

	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"),
		"the rollback re-bind must leave the old address serving")
	require.Equal(t, oldAddr, m.lifecycle.snapshot().listeners.TCPBoundAddr,
		"lifecycle must still report the old bound address after a rollback")
	wl.mu.Lock()
	restored := wl.webHandle
	wl.mu.Unlock()
	require.NotSame(t, released, restored,
		"the listener answering after a rollback is a re-bound socket, not the one that was released")
}

// TestWebListenerDifferentPortBindFailureKeepsSameHandle pins the UNCHANGED
// half of the contract: when the port itself moves, bind-new-before-close is
// the only order tried — a failure must not cost the old listener even a
// release-and-rollback bounce, so the SAME handle still owns it afterwards.
func TestWebListenerDifferentPortBindFailureKeepsSameHandle(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)

	// Occupy a DIFFERENT port so the rebind onto it fails EADDRINUSE.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()

	wl.mu.Lock()
	before := wl.webHandle
	wl.mu.Unlock()

	next := *m.Config()
	next.ListenAddr = blocker.Addr().String()
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "a rebind onto an occupied different port must fail")
	require.Contains(t, failed, "network.listen_addr")

	wl.mu.Lock()
	require.Same(t, before, wl.webHandle,
		"a different-port failure must leave the old listener untouched — no release-then-bind bounce")
	wl.mu.Unlock()
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))
	require.Equal(t, oldAddr, m.lifecycle.snapshot().listeners.TCPBoundAddr)
}

// TestWebListenerSamePortDistinctAddressKeepsSameHandle pins the OTHER half of
// the same-port gate: sharing a port does not make the held listener the cause.
// A move between two DISTINCT specific addresses (127.0.0.1:P → 127.0.0.2:P)
// could have coexisted, so its EADDRINUSE belongs to someone else's socket and
// the healthy listener must not be released on speculation — the SAME handle
// keeps serving, exactly like a different-port failure.
func TestWebListenerSamePortDistinctAddressKeepsSameHandle(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	// The conflict on the new address is a third party's, expressed through the
	// listen seam (127.0.0.2 need not be bindable in the test environment).
	newAddr := "127.0.0.2:" + port
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if address == newAddr {
			return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
		}
		return net.Listen(network, address)
	}

	wl.mu.Lock()
	before := wl.webHandle
	wl.mu.Unlock()

	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "a rebind onto an occupied distinct address must fail")
	require.Contains(t, failed, "network.listen_addr")

	wl.mu.Lock()
	require.Same(t, before, wl.webHandle,
		"a same-port failure our listener did not cause must leave it untouched — no release-then-bind bounce")
	wl.mu.Unlock()
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))
	require.Equal(t, oldAddr, m.lifecycle.snapshot().listeners.TCPBoundAddr)
}

// TestWebListenerSamePortRollbackFailureClearsBoundState: when BOTH the retry
// and the rollback re-bind fail, the daemon has no listener — and its state must
// say so synchronously. The retired listener's done-watcher cannot take wl.mu
// until reconcile returns, so bound state left standing would answer
// ListenerAddress with the very address the error declares dead.
func TestWebListenerSamePortRollbackFailureClearsBoundState(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	// Deny the requested address AND the rollback re-bind of the old one.
	newAddr := "0.0.0.0:" + port
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if address == newAddr || address == oldAddr {
			return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
		}
		return net.Listen(network, address)
	}

	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "the rollback failure must surface")
	require.Contains(t, rerr.Error(), "the daemon has no TCP listener")
	require.Contains(t, failed, "network.listen_addr")

	require.Empty(t, wl.listenerAddress("network.listen_addr"),
		"no listener is accepting, so no bound address may be reported")
	require.Empty(t, wl.webConfigAddress())
	require.False(t, m.lifecycle.snapshot().listeners.TCPBound,
		"lifecycle bound state must be cleared synchronously")
	require.Empty(t, m.lifecycle.snapshot().listeners.TCPBoundAddr)

	// The failed listener key is still retryable: a later apply of the same
	// address re-attempts the bind rather than deciding the state matches.
	next2 := *m.Config()
	m.live.Store(&next2)
	wl.listenTCP = net.Listen
	failed, err := wl.reconcile(&next2)
	require.NoError(t, err, "after the state cleared, re-applying retries the bind")
	require.Empty(t, failed)
	require.Equal(t, http.StatusOK, getStatus(t, "127.0.0.1:"+port, "/v1/health"),
		"the retried bind serves on the requested address")
}

// TestWebListenerSamePortLeadingZeroPortAppliesLive: port SPELLINGS the
// validator accepts (config/listen_addr.go lets every net.Listen port form
// through — service names, leading zeros) must compare equal after resolution.
// A "043123" port is the same port as the resolved "43123"; a raw string
// compare would miss the overlap and leave a valid same-port move deferred.
func TestWebListenerSamePortLeadingZeroPortAppliesLive(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0"
	m, wl, oldBound := boundWebListeners(t, cfg)
	port := boundPort(t, oldBound)

	newAddr := "127.0.0.1:0" + port
	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, err := wl.reconcile(&next)
	require.NoError(t, err, "a leading-zero spelling of the same port must still take the release path (#5140)")
	require.Empty(t, failed)

	boundHost, boundP, err := net.SplitHostPort(m.lifecycle.snapshot().listeners.TCPBoundAddr)
	require.NoError(t, err)
	require.Equal(t, port, boundP)
	require.Equal(t, "127.0.0.1", boundHost)
	require.Equal(t, http.StatusOK, getStatus(t, "127.0.0.1:"+port, "/v1/health"))
}

// TestWebListenerSamePortWideningBlockedBySiblingKeepsHandle pins the sibling
// half of the gate: when the OTHER managed listener also overlaps the requested
// wildcard — control on 127.0.0.1:P, preview on 127.0.0.2:P, request 0.0.0.0:P —
// releasing the control listener cannot make the retry succeed (the sibling
// still owns part of the port), so the gate must decline release-then-bind and
// leave the SAME handle serving.
func TestWebListenerSamePortWideningBlockedBySiblingKeepsHandle(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = ""
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	// Record a sibling bound address on the same port — two distinct specific
	// addresses on one port coexist, so this is a real shape. Only the bound
	// string is read by the gate; no socket has to exist on 127.0.0.2.
	wl.mu.Lock()
	wl.previewBoundAddr = "127.0.0.2:" + port
	before := wl.webHandle
	wl.mu.Unlock()

	// Deny the wildcard through the seam rather than relying on the kernel:
	// BSD/macOS SO_REUSEADDR semantics let 0.0.0.0:P bind over a held
	// 127.0.0.1:P, so only Linux produces the EADDRINUSE this test needs.
	newAddr := "0.0.0.0:" + port
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if address == newAddr {
			return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
		}
		return net.Listen(network, address)
	}

	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "widening onto a port the sibling shares must fail")
	require.Contains(t, failed, "network.listen_addr")

	wl.mu.Lock()
	require.Same(t, before, wl.webHandle,
		"when the sibling also blocks the wildcard, releasing ours cannot help — the handle must not bounce")
	wl.mu.Unlock()
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))
	require.Equal(t, oldAddr, m.lifecycle.snapshot().listeners.TCPBoundAddr)
}

// TestWebListenerIPv4WildcardRequestSeesIPv6Sibling pins the request-side
// family rule: control on 127.0.0.1:P with the preview sibling bound on
// [::1]:P, control asked to widen to 0.0.0.0:P. Go binds a requested 0.0.0.0
// as a dual-stack [::] socket, so on Linux that bind still collides with the
// v6 sibling — classifying the request as IPv4-only misses the sibling and
// releases the healthy control listener for a retry that cannot succeed.
// The gate must see the sibling and decline, leaving the SAME handle serving.
func TestWebListenerIPv4WildcardRequestSeesIPv6Sibling(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "0.0.0.0:0" // configured, so the sibling is not torn down
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	// Record the sibling on an IPv6-specific address on the same port — only
	// the bound string feeds the gate; no socket on [::1] has to exist.
	wl.mu.Lock()
	wl.previewBoundAddr = "[::1]:" + port
	before := wl.webHandle
	wl.mu.Unlock()

	// Deny the wildcard through the seam, persistently — the retry after the
	// sibling step must meet the same denial so the shape stays deferred.
	newAddr := "0.0.0.0:" + port
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if address == newAddr {
			return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
		}
		return net.Listen(network, address)
	}

	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "widening onto a port a v6 sibling shares must fail")
	require.Contains(t, failed, "network.listen_addr")

	wl.mu.Lock()
	require.Same(t, before, wl.webHandle,
		"a requested 0.0.0.0 binds dual-stack and still collides with the [::1] sibling — releasing ours cannot help")
	wl.mu.Unlock()
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))
	require.Equal(t, oldAddr, m.lifecycle.snapshot().listeners.TCPBoundAddr)
}

// TestWebListenerSamePortSiblingMovedInSameApplyRetries: when ONE apply both
// widens the control listener onto the sibling's port AND tears the sibling
// down, the sibling-decline must not stick — after the preview step runs, the
// blocker is gone, so reconcile retries the declined bind and the widening
// applies live.
func TestWebListenerSamePortSiblingMovedInSameApplyRetries(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "0.0.0.0:0" // a real preview listener, so its teardown runs
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	// Record the preview sibling on the SAME port on a distinct specific
	// address — the shape that blocks the control widening. Only the bound
	// string feeds the gate; no socket on 127.0.0.2 is needed.
	wl.mu.Lock()
	wl.previewBoundAddr = "127.0.0.2:" + port
	wl.mu.Unlock()

	next := *m.Config()
	next.ListenAddr = "0.0.0.0:" + port
	next.PreviewListenAddr = "" // torn down in the same apply — the blocker leaves
	m.live.Store(&next)

	failed, err := wl.reconcile(&next)
	require.NoError(t, err, "once the sibling leaves the port the retried widening must apply (#5140)")
	require.Empty(t, failed)

	boundHost, boundP, err := net.SplitHostPort(m.lifecycle.snapshot().listeners.TCPBoundAddr)
	require.NoError(t, err)
	require.Equal(t, port, boundP)
	require.True(t, net.ParseIP(boundHost).IsUnspecified())
	require.Equal(t, http.StatusOK, getStatus(t, "127.0.0.1:"+port, "/v1/health"))
}

// TestWebListenerSamePortIPv6WideningAppliesLive covers the family the OpError
// can lose: net.Listen("tcp", "[::]:P") may report the Addr of its last
// internal bind attempt as 0.0.0.0:P, so the gate must take the requested
// spelling's family — a v6-only listener widening to the v6 wildcard is the
// same overlap shape as the IPv4 narrowing and must apply live.
func TestWebListenerSamePortIPv6WideningAppliesLive(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback in this environment: %v", err)
	}
	probe.Close()

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "[::1]:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	newAddr := "[::]:" + port
	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.NoError(t, rerr, "an IPv6 same-port widening must apply live (#5140)")
	require.Empty(t, failed)

	boundHost, boundP, err := net.SplitHostPort(m.lifecycle.snapshot().listeners.TCPBoundAddr)
	require.NoError(t, err)
	require.Equal(t, port, boundP)
	require.True(t, net.ParseIP(boundHost).IsUnspecified())
}

// TestWebListenerSiblingOnlyConflictRetriesAfterSiblingLeaves pins the
// sibling-only shape: control moves A:P → B:P where B:P belongs to the sibling —
// our own listener does not overlap at all, so there is nothing to release —
// but the same apply tears the sibling down, so the declined bind must be
// retried after the sibling's step and apply live.
func TestWebListenerSiblingOnlyConflictRetriesAfterSiblingLeaves(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "0.0.0.0:0" // real preview listener so its teardown runs
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	target := "127.0.0.2:" + port
	// The sibling records the target as ITS bound address — distinct specifics
	// on one port coexist, so this is a real shape — and the seam denies only
	// the first attempt at it. Later binds get a real loopback socket, so
	// nothing here depends on 127.0.0.2 being assignable in the environment.
	wl.mu.Lock()
	wl.previewBoundAddr = target
	wl.mu.Unlock()
	denied := false
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if address == target && !denied {
			denied = true
			return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
		}
		return net.Listen(network, "127.0.0.1:0")
	}

	next := *m.Config()
	next.ListenAddr = target
	next.PreviewListenAddr = "" // the sibling leaves the port in the same apply
	m.live.Store(&next)

	failed, err := wl.reconcile(&next)
	require.NoError(t, err, "once the sibling vacates the address the retried move must apply (#5140)")
	require.Empty(t, failed)
	require.Equal(t, target, wl.webConfigAddress())
	require.Equal(t, http.StatusOK,
		getStatus(t, m.lifecycle.snapshot().listeners.TCPBoundAddr, "/v1/health"))
}

// TestWebListenerTwoWaySwapAppliesLive pins the mutual-block shape: control on
// 127.0.0.1:P moves to 127.0.0.2:P while the preview sibling moves from
// (recorded as) 127.0.0.2:P to 127.0.0.1:P — each request names the address
// the OTHER listener is holding, so both first binds come back
// sibling-declined and a single-listener retry can never free either port.
// Reconcile must retire BOTH listeners and rebind them in turn, applying the
// whole swap live instead of deferring two valid changes forever.
func TestWebListenerTwoWaySwapAppliesLive(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0" // a real preview listener so its release runs
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	// The preview listener's REAL bound address is a second loopback socket on
	// its own ephemeral port; the gate is fed 127.0.0.2:P — the address the
	// control request will name — while previewBoundAddr-vs-real drift does not
	// matter anywhere else in this test.
	wl.mu.Lock()
	previewRealAddr := wl.previewBoundAddr
	wl.previewBoundAddr = "127.0.0.2:" + port
	wl.mu.Unlock()

	// The control bind to 127.0.0.2:P is denied through the seam only WHILE the
	// preview sibling still holds its real socket — a probe bind on the real
	// preview port is the witness. That is exactly the release ordering under
	// test: a single-listener retry leaves the sibling bound (deny), the swap
	// path's coordinated release frees it first (allow, redirected to loopback
	// so nothing depends on 127.0.0.2 being assignable).
	webTarget := "127.0.0.2:" + port
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if address == webTarget {
			if probe, perr := net.Listen(network, previewRealAddr); perr == nil {
				probe.Close()
				return net.Listen(network, "127.0.0.1:0")
			}
			return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
		}
		return net.Listen(network, address)
	}

	next := *m.Config()
	next.ListenAddr = webTarget
	next.PreviewListenAddr = "127.0.0.1:" + port // preview takes the control listener's home
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.NoError(t, rerr, "a two-way sibling swap must converge, not defer both sides (#5140)")
	require.Empty(t, failed)
	require.Equal(t, webTarget, wl.webConfigAddress())

	wl.mu.Lock()
	require.Equal(t, "127.0.0.1:"+port, wl.previewConfigAddr)
	require.Equal(t, "127.0.0.1:"+port, wl.previewBoundAddr)
	wl.mu.Unlock()
	require.Equal(t, http.StatusOK,
		getStatus(t, m.lifecycle.snapshot().listeners.TCPBoundAddr, "/v1/health"))
}

// TestWebListenerTwoWaySwapRestoresPairWhenSecondHalfFails pins the failure
// half of the swap: control 127.0.0.1:P → 127.0.0.2:P and preview
// 127.0.0.2:P → 127.0.0.1:P is a realizable exchange, but the preview bind
// still fails (a squatter took the freshly released address in the gap). The
// control move then OWNS the preview rollback target — the apply must undo the
// control half too so both listeners come back, not strand the preview.
func TestWebListenerTwoWaySwapRestoresPairWhenSecondHalfFails(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0" // a real preview listener
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)
	oldPreviewConfig := wl.previewConfigAddr

	wl.mu.Lock()
	wl.previewBoundAddr = "127.0.0.2:" + port
	wl.mu.Unlock()

	// The seam models the squatter on wl state — safe, the listener mutex is
	// held for the whole reconcile: a bind aimed at a contested address is
	// denied while the OTHER claimant still holds a bound address, and allowed
	// (as a plain loopback socket) once that claimant has been released. On
	// the fixed path the preview rollback only reaches 127.0.0.2:P after the
	// control move was undone; on the un-fixed path it meets a still-moved
	// control claim and is denied, stranding the preview listener.
	webTarget := "127.0.0.2:" + port
	previewTarget := "127.0.0.1:" + port
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		switch address {
		case webTarget:
			if wl.webBoundAddr != "" {
				return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
			}
			// Stand in for the (possibly unassignable) 127.0.0.2 socket.
			return net.Listen(network, "127.0.0.1:0")
		case previewTarget:
			if wl.webBoundAddr != "" {
				return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
			}
			return net.Listen(network, address)
		}
		return net.Listen(network, address)
	}

	next := *m.Config()
	next.ListenAddr = webTarget
	next.PreviewListenAddr = previewTarget
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "a swap whose second half cannot land reports deferred")
	require.Contains(t, failed, "network.listen_addr")
	require.Contains(t, failed, "network.preview_listen_addr")

	wl.mu.Lock()
	require.NotNil(t, wl.previewHandle,
		"undoing the control move frees the preview listener's home — it must come back")
	require.Equal(t, oldPreviewConfig, wl.previewConfigAddr)
	require.Equal(t, oldAddr, wl.webBoundAddr,
		"the control listener must be restored to the address it was moved off")
	require.NotNil(t, wl.webHandle)
	wl.mu.Unlock()
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))
}

// TestWebListenerMutualDeclineIsNotASwap pins the preflight on the swap path:
// control on 127.0.0.1:P and the preview sibling on 127.0.0.2:P are BOTH asked
// for 0.0.0.0:P — the requests overlap each other, so the mutual
// sibling-decline is not a realizable exchange: releasing both would let the
// first bind claim the wildcard and strand the second listener's rollback.
// Both handles must survive and both changes must report deferred.
func TestWebListenerMutualDeclineIsNotASwap(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = "127.0.0.1:0" // a real preview listener
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	// Only the bound string feeds the gate; no socket on 127.0.0.2 is needed.
	wl.mu.Lock()
	wl.previewBoundAddr = "127.0.0.2:" + port
	beforeWeb := wl.webHandle
	beforePreview := wl.previewHandle
	wl.mu.Unlock()

	newAddr := "0.0.0.0:" + port
	wl.listenTCP = func(network, address string) (net.Listener, error) {
		if address == newAddr {
			return nil, fmt.Errorf("listen %s: %w", address, syscall.EADDRINUSE)
		}
		return net.Listen(network, address)
	}

	next := *m.Config()
	next.ListenAddr = newAddr
	next.PreviewListenAddr = newAddr
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.Error(t, rerr, "both sides asking for the same wildcard cannot both bind — defer")
	require.Contains(t, failed, "network.listen_addr")
	require.Contains(t, failed, "network.preview_listen_addr")

	wl.mu.Lock()
	require.Same(t, beforeWeb, wl.webHandle,
		"the requests overlap each other — releasing both can only strand a listener")
	require.Same(t, beforePreview, wl.previewHandle,
		"the requests overlap each other — releasing both can only strand a listener")
	wl.mu.Unlock()
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))
}

// TestWebListenerSameEndpointRespellingKeepsSandboxCredentials: a change that
// resolves to the identical endpoint (127.0.0.1:43123 → 127.0.0.1:043123) still
// has to release and re-bind the socket — but nothing MOVED, so it must not run
// the move announcement that revokes every sandbox callback credential.
func TestWebListenerSameEndpointRespellingKeepsSandboxCredentials(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	m, wl, oldAddr := boundWebListeners(t, cfg)
	port := boundPort(t, oldAddr)

	_, err := m.sandboxTokens.mint("session-1")
	require.NoError(t, err)
	before := m.sandboxTokens.invalidationCount()

	newAddr := "127.0.0.1:0" + port
	next := *m.Config()
	next.ListenAddr = newAddr
	m.live.Store(&next)

	failed, rerr := wl.reconcile(&next)
	require.NoError(t, rerr, "a respelling of the same endpoint must apply live")
	require.Empty(t, failed)
	require.Equal(t, m.sandboxTokens.invalidationCount(), before,
		"the resolved endpoint never changed, so no credential may be revoked")
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))
	require.Equal(t, newAddr, wl.webConfigAddress())
}

// TestWebListenerSamePortSwapFlushesInflightRequest: releasing the old listener
// before the new bind must not drop a request already IN THE HANDLER on it.
// retire() stops accepting and frees the port synchronously, then drains active
// connections under the usual grace — the same drain a post-bind retire runs,
// just sequenced before the bind — so a request mid-handler across the swap
// still gets its reply. (A request whose headers are only partially read when
// Shutdown begins is dropped by net/http itself — readRequest returns after
// inShutdown is set and the serve loop never runs the handler — which is true
// of every retirement, not just this path.)
func TestWebListenerSamePortSwapFlushesInflightRequest(t *testing.T) {
	// A grace this test can never reach — it is about the DRAIN, not the
	// deadline (same rationale as TestRetiredListenerDrainsAnInFlightRequest).
	withRetireGrace(t, time.Minute)
	m, wl, addr, entered, release := hangingControlListener(t)
	port := boundPort(t, addr)

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET /v1/hang HTTP/1.1\r\nHost: %s\r\n\r\n", addr)
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the request never reached the handler")
	}

	// The listener widens on the SAME port — the release-then-bind path — while
	// the request is in the handler…
	next := *m.Config()
	next.ListenAddr = "0.0.0.0:" + port
	m.live.Store(&next)
	failed, err := wl.reconcile(&next)
	require.NoError(t, err, "the same-port widening must succeed while the request is in flight")
	require.Empty(t, failed)

	// …which then finishes, and its reply reaches the client.
	release()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	raw, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Contains(t, string(raw), "200 OK", "the in-flight reply must reach the client")
	require.Contains(t, string(raw), "drained")

	require.Equal(t, http.StatusOK, getStatus(t, "127.0.0.1:"+port, "/v1/hang"),
		"the widened listener serves on loopback (/v1/hang returns immediately once released)")
}

// TestPreviewListenerSamePortNarrowingAppliesLive keeps the pair honest (#3012):
// the preview listener shares the same-port overlap shape, so it takes the same
// release-then-bind path rather than failing live with EADDRINUSE.
func TestPreviewListenerSamePortNarrowingAppliesLive(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = ""
	cfg.PreviewListenAddr = "0.0.0.0:0"
	m, err := NewManager(cfg)
	require.NoError(t, err)
	wl := newWebListeners(m, newHTTPMux(&controlServer{manager: m}), newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	t.Cleanup(func() { _ = wl.close() })

	oldBound := m.lifecycle.snapshot().listeners.PreviewBoundAddr
	require.NotEmpty(t, oldBound, "the preview listener must be bound")
	port := boundPort(t, oldBound)

	newAddr := "127.0.0.1:" + port
	next := *m.Config()
	next.PreviewListenAddr = newAddr
	m.live.Store(&next)

	failed, err = wl.reconcile(&next)
	require.NoError(t, err, "a same-port preview narrowing must apply live (#5140)")
	require.Empty(t, failed)
	require.Equal(t, newAddr, m.lifecycle.snapshot().listeners.PreviewBoundAddr)
}
