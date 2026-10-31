package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// relayDialTimeout bounds the in-workspace connect. The target is loopback, so
// anything slower than this is a port nobody is listening on.
const relayDialTimeout = 5 * time.Second

// relayReadLimit raises the websocket read limit off the 32 KiB default; the
// relay carries raw TCP chunks of up to io.Copy's buffer size per message.
const relayReadLimit = 1 << 20

// parseRelayPort validates the ?port= query. It accepts only a plain decimal
// port in 1..65535: the relay's whole safety property is that it can reach
// 127.0.0.1:<port> and nothing else, so the host is never caller-chosen.
func parseRelayPort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("relay requires ?port=<1-65535>, got %q", raw)
	}
	return port, nil
}

// relayHandler upgrades GET /v1/agent/relay?port=N to a websocket and pipes it,
// as raw bytes, to 127.0.0.1:N on this host. The target is dialed BEFORE the
// upgrade so a closed port is a plain 502 the caller can report, rather than an
// accepted socket that immediately closes. The token gate wrapping the mux has
// already authenticated the request.
func (hs *headlessServer) relayHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := net.Dialer{Timeout: relayDialTimeout}
	var target net.Conn
	if which := q.Get(agentproto.RelayTargetQueryParam); which != "" {
		// A server-owned target (W5.4): the workspace's editor socket, whose path
		// only this process knows. It is never caller-chosen and cannot be combined
		// with a port.
		if which != agentproto.RelayTargetEditor || q.Get(agentproto.RelayPortQueryParam) != "" {
			writeHTTPError(w, r, http.StatusBadRequest, fmt.Errorf("relay target must be %q and excludes ?port=", agentproto.RelayTargetEditor))
			return
		}
		ep, err := hs.ensureEditor()
		if err != nil {
			writeHTTPError(w, r, http.StatusBadGateway, fmt.Errorf("the workspace editor is not serving: %w", err))
			return
		}
		target, err = d.DialContext(r.Context(), "unix", ep.SocketPath)
		if err != nil {
			writeHTTPError(w, r, http.StatusBadGateway, fmt.Errorf("the workspace editor is not accepting connections: %w", err))
			return
		}
	} else {
		port, err := parseRelayPort(q.Get(agentproto.RelayPortQueryParam))
		if err != nil {
			writeHTTPError(w, r, http.StatusBadRequest, err)
			return
		}
		target, err = d.DialContext(r.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			writeHTTPError(w, r, http.StatusBadGateway, fmt.Errorf("nothing is listening on 127.0.0.1:%d in the workspace: %w", port, err))
			return
		}
	}
	defer target.Close()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(relayReadLimit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsc := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	defer wsc.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, wsc); done <- struct{}{} }()
	go func() { _, _ = io.Copy(wsc, target); done <- struct{}{} }()
	// Either side closing ends the relay; closing both unblocks the other copy.
	<-done
}
