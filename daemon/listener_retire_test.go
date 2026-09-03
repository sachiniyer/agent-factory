package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The retirement tests (#3722). A listener the daemon REPLACES while it keeps
// running cannot be closed the way one at shutdown is: the config write that
// moves network.listen_addr arrives on the listener it moves, so cutting that
// listener from inside the handler destroys the connection its own reply is
// about to be written to. The operator is then told a committed, applied write
// failed and retries against an address the daemon has already left.

// setConfigValueEnvelope is what a client sees on the wire, so these tests read
// the reply the way `af config set --daemon-url` and the web form do rather than
// calling the handler in-process — where the severed connection, which is the
// whole defect, does not exist.
type setConfigValueEnvelope struct {
	Data  *SetConfigValueResponse `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// postSetConfigValue writes one key over a listener at addr and returns the
// status, the decoded envelope, and the raw body for failure messages.
func postSetConfigValue(t *testing.T, addr, key, value string) (int, setConfigValueEnvelope, string) {
	t.Helper()
	body, err := json.Marshal(SetConfigValueRequest{Key: key, Value: value})
	require.NoError(t, err)
	resp, err := http.Post("http://"+addr+"/v1/SetConfigValue", "application/json", bytes.NewReader(body))
	require.NoError(t, err, "the reply to a write that moves this listener must survive the move")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "the reply must be readable in full, not truncated by a closed connection")
	var env setConfigValueEnvelope
	require.NoError(t, json.Unmarshal(raw, &env), "body: %s", raw)
	return resp.StatusCode, env, string(raw)
}

// TestSetListenAddrRepliesOnTheListenerItMoves is the reported defect (#3722),
// end to end over the wire: a remote `SetConfigValue` of network.listen_addr must
// come back 200 on the OLD connection AND leave the NEW address answering.
//
// RED on master: bindWebLocked called the old listener's closer — srv.Close(),
// which cuts connections mid-handler — synchronously, so the 200 was written into
// a socket that was already gone and the client saw EOF for a write that had
// committed and applied.
func TestSetListenAddrRepliesOnTheListenerItMoves(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	_, _, oldAddr := boundWebListeners(t, cfg)
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))

	newAddr := grabFreeLoopbackAddr(t)
	status, env, raw := postSetConfigValue(t, oldAddr, "network.listen_addr", newAddr)

	require.Equal(t, http.StatusOK, status, "body: %s", raw)
	require.Nil(t, env.Error, "body: %s", raw)
	require.NotNil(t, env.Data, "body: %s", raw)
	require.NotNil(t, env.Data.Result)
	require.Equal(t, newAddr, env.Data.Result.Value, "the reply must echo the value that was written")

	// The other half of the decision: the reply names where the daemon is now
	// serving, so the operator re-targets from the answer instead of rediscovering
	// the address by hand.
	require.Equal(t, newAddr, env.Data.ListenerAddr,
		"the reply for a listener key must name the address the daemon is now accepting on")

	require.Equal(t, http.StatusOK, getStatus(t, newAddr, "/v1/health"),
		"the daemon must be serving on the new address by the time the reply lands")
}

// TestSetListenAddrToEmptyRepliesBeforeTheListenerGoes: the network.listen_addr=""
// opt-out reaches the same teardown path and severs its own reply in exactly the
// same way, so it is retired too. There is no address to name afterwards — the
// listener is gone, and the reply says so by leaving the field empty rather than
// echoing an address nothing answers.
func TestSetListenAddrToEmptyRepliesBeforeTheListenerGoes(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	_, _, oldAddr := boundWebListeners(t, cfg)

	status, env, raw := postSetConfigValue(t, oldAddr, "network.listen_addr", "")

	require.Equal(t, http.StatusOK, status, "body: %s", raw)
	require.NotNil(t, env.Data, "body: %s", raw)
	require.Empty(t, env.Data.ListenerAddr, "nothing is accepting, so there is no address to name")

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", oldAddr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	}, 3*time.Second, 20*time.Millisecond, "the opt-out must still take the listener down")
}

// TestFailedRebindReportsTheAddressStillServing pins the field's contract at the
// point where echoing the request would be a lie: a rebind onto an occupied port
// leaves the OLD listener serving, so the reply must name THAT address, not the
// one the caller asked for.
func TestFailedRebindReportsTheAddressStillServing(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	_, _, oldAddr := boundWebListeners(t, cfg)

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()
	occupied := blocker.Addr().String()

	status, env, raw := postSetConfigValue(t, oldAddr, "network.listen_addr", occupied)
	require.Equal(t, http.StatusOK, status, "body: %s", raw)
	require.NotNil(t, env.Data, "body: %s", raw)

	require.Equal(t, oldAddr, env.Data.ListenerAddr,
		"a failed rebind keeps the old listener; the reply must name where the daemon IS, not where it was asked to go")
	require.NotEmpty(t, env.Data.Warnings, "a failed rebind must still warn")
	require.Equal(t, http.StatusOK, getStatus(t, oldAddr, "/v1/health"))
}

// hangingControlListener binds the control listener on a mux that blocks in
// /v1/hang until the returned release is called, so a test can hold a request in
// flight across a rebind. entered fires when the handler is reached.
func hangingControlListener(t *testing.T) (m *Manager, wl *webListeners, addr string, entered <-chan struct{}, release func()) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.PreviewListenAddr = ""
	m, err := NewManager(cfg)
	require.NoError(t, err)

	reached := make(chan struct{})
	releaseCh := make(chan struct{})
	var once bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/hang", func(w http.ResponseWriter, _ *http.Request) {
		if !once {
			once = true
			close(reached)
		}
		<-releaseCh
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "drained")
	})

	wl = newWebListeners(m, mux, newPreviewMux(&controlServer{manager: m}))
	m.webListeners = wl
	failed, err := wl.reconcile(m.Config())
	require.NoError(t, err)
	require.Empty(t, failed)
	t.Cleanup(func() { _ = wl.close() })

	addr = m.lifecycle.snapshot().listeners.TCPBoundAddr
	require.NotEmpty(t, addr, "the control listener must be bound")
	return m, wl, addr, reached, func() { close(releaseCh) }
}

// rebindControl moves the control listener to a fresh address.
func rebindControl(t *testing.T, m *Manager, wl *webListeners) {
	t.Helper()
	next := *m.Config()
	next.ListenAddr = grabFreeLoopbackAddr(t)
	m.live.Store(&next)
	failed, err := wl.reconcile(&next)
	require.NoError(t, err)
	require.Empty(t, failed)
}

// TestRetiredListenerDrainsAnInFlightRequest is the property under the fix,
// stated on the listener owner rather than on one config key: a request already
// in flight when its listener is retired still gets its COMPLETE response.
//
// RED on master, where the old listener's srv.Close() cut the connection and the
// client read nothing at all.
func TestRetiredListenerDrainsAnInFlightRequest(t *testing.T) {
	m, wl, addr, entered, release := hangingControlListener(t)

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

	// The listener moves out from under a live request…
	rebindControl(t, m, wl)

	// …which then finishes, and its reply reaches the client.
	release()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	raw, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Contains(t, string(raw), "200 OK", "the in-flight reply must reach the client")
	require.Contains(t, string(raw), "drained")
}

// TestRebindDoesNotCutAConnectionMidRequest covers BOTH listeners with the real
// muxes, using a connection the server has accepted and is still reading a
// request from — the state srv.Close() destroys and a retirement does not.
//
// The preview listener cannot reach a test handler (its per-tab credential gate
// answers first, by design — #1856), so the discriminator here is the connection
// itself rather than a reply body. It is the same one: on master both listeners
// cut this connection the instant reconcile ran.
//
// Both are covered because both take the same retirement. Only the control one
// can sever its own reply today — preview_listen_addr rebinds a DIFFERENT
// listener from the one the write arrives on — but a listener-lifetime rule that
// holds for one of a pair and not the other is how this file earned its scars
// (#3012): the exception is what the next change reasons from.
func TestRebindDoesNotCutAConnectionMidRequest(t *testing.T) {
	for _, kind := range []string{"control", "preview"} {
		t.Run(kind, func(t *testing.T) {
			// Long enough that "still connected" cannot be the deadline not having
			// arrived yet on a loaded box, and short enough to also watch it expire.
			withRetireGrace(t, 2*time.Second)
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
			cs := &controlServer{manager: m}
			wl := newWebListeners(m, newHTTPMux(cs), newPreviewMux(cs))
			m.webListeners = wl
			failed, err := wl.reconcile(m.Config())
			require.NoError(t, err)
			require.Empty(t, failed)
			t.Cleanup(func() { _ = wl.close() })

			state := m.lifecycle.snapshot().listeners
			addr := state.TCPBoundAddr
			if kind == "preview" {
				addr = state.PreviewBoundAddr
			}
			require.NotEmpty(t, addr)

			// A request the server has begun reading and cannot finish: no final
			// CRLF, so the connection is neither idle nor complete.
			conn, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			defer conn.Close()
			_, err = fmt.Fprintf(conn, "GET /v1/health HTTP/1.1\r\nHost: %s\r\n", addr)
			require.NoError(t, err)

			next := *m.Config()
			if kind == "control" {
				next.ListenAddr = grabFreeLoopbackAddr(t)
			} else {
				next.PreviewListenAddr = grabFreeLoopbackAddr(t)
			}
			m.live.Store(&next)
			failed, err = wl.reconcile(&next)
			require.NoError(t, err)
			require.Empty(t, failed)

			// Retired, not closed: the connection is still up right after the rebind.
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
			_, err = conn.Read(make([]byte, 1))
			var netErr net.Error
			require.True(t, errors.As(err, &netErr) && netErr.Timeout(),
				"a rebind must not cut a connection the old listener is mid-request on; got %v", err)

			// And the deadline still governs it — retirement is not a leak.
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(4*time.Second)))
			_, err = conn.Read(make([]byte, 1))
			require.Error(t, err)
			require.False(t, errors.As(err, &netErr) && netErr.Timeout(),
				"the retirement deadline must still close a connection that never completes")
		})
	}
}

// withRetireGrace shortens the drain deadline for one test and restores it.
func withRetireGrace(t *testing.T, d time.Duration) {
	t.Helper()
	previous := listenerRetireGrace
	listenerRetireGrace = d
	t.Cleanup(func() { listenerRetireGrace = previous })
}

// TestRetiredListenerClosesAClientThatNeverDrains is the deadline's reason for
// existing: retiring rather than closing must not hand a stalled client the power
// to keep a superseded server, its connections, and its memory alive for as long
// as it likes. Past the grace those connections are force-closed.
func TestRetiredListenerClosesAClientThatNeverDrains(t *testing.T) {
	withRetireGrace(t, 300*time.Millisecond)
	m, wl, addr, entered, release := hangingControlListener(t)
	defer release()

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

	// The handler is never released, so the drain cannot complete.
	rebindControl(t, m, wl)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)
	var netErr net.Error
	require.False(t, errors.As(err, &netErr) && netErr.Timeout(),
		"a client past the retirement deadline must be CLOSED, not left holding the retired server open")
}
