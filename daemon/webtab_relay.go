package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// relayTransportIdleTimeout bounds how long an idle relayed connection (one
// websocket over the session's ssh connection) is kept for reuse.
const relayTransportIdleTimeout = 30 * time.Second

// relayTransport returns the http transport that reaches the loopback of an
// off-box session's workspace through its agent-server relay (W5.1). One is kept
// per session so keep-alive reuses relay connections instead of opening a
// websocket per request.
//
// The dialer ignores the host in the address it is handed: the relay can reach
// 127.0.0.1:<port> in the workspace and nothing else, and the port is the only
// part of the target that survives. It resolves the agent-server at dial time, so
// a session restored onto a new runtime is reached on its new connection rather
// than the torn-down one.
func (m *Manager) relayTransport(instance *session.Instance, repoID, title string) (*http.Transport, error) {
	if _, ok := instance.AgentServer().(session.LoopbackRelayer); !ok {
		return nil, fmt.Errorf("this session's workspace is off-box and its runtime has no port relay, so a loopback web tab cannot be served (a daemon-host port would be the wrong machine)")
	}
	key := daemonInstanceKey(repoID, title)
	// Keyed by the Instance too: a session rebuilt under the same key is a new
	// object whose agent-server the cached dialer does not reach.
	if v, ok := m.relayTransports.Load(key); ok {
		if e := v.(relayTransportEntry); e.instance == instance {
			return e.transport, nil
		}
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			relayer, ok := instance.AgentServer().(session.LoopbackRelayer)
			if !ok {
				return nil, fmt.Errorf("session has no port relay")
			}
			_, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				return nil, err
			}
			return relayer.DialLoopback(ctx, port)
		},
		// The environment proxy would route a relay dial through a daemon-host
		// proxy, which is the wrong machine for a workspace loopback.
		Proxy:           nil,
		IdleConnTimeout: relayTransportIdleTimeout,
	}
	if old, ok := m.relayTransports.Swap(key, relayTransportEntry{instance: instance, transport: tr}); ok {
		old.(relayTransportEntry).transport.CloseIdleConnections()
	}
	return tr, nil
}

type relayTransportEntry struct {
	instance  *session.Instance
	transport *http.Transport
}

// evictRelayTransport drops a session's cached relay transport and closes its
// idle relayed connections. A no-op for a session that never opened one.
func (m *Manager) evictRelayTransport(key string) {
	for _, k := range []string{key, editorRelayTransportKey(key)} {
		if old, ok := m.relayTransports.LoadAndDelete(k); ok {
			old.(relayTransportEntry).transport.CloseIdleConnections()
		}
	}
}
