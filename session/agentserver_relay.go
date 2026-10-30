package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coder/websocket"
	"github.com/sachiniyer/agent-factory/agentproto"
)

// LoopbackRelayer is implemented by the agent-servers that can reach a loopback
// port INSIDE their workspace (W5.1, #5571). It is a separate interface rather
// than an AgentServer method because only the off-box runtime has a workspace
// the daemon cannot dial directly; the local runtime's loopback is the daemon's.
type LoopbackRelayer interface {
	// DialLoopback opens a byte stream to 127.0.0.1:port in the workspace. The
	// returned conn is closed when ctx is done only for the dial; its own Close
	// ends the relay. Every open relay is also closed when the session is killed.
	DialLoopback(ctx context.Context, port int) (net.Conn, error)
}

var _ LoopbackRelayer = (*remoteAgentServer)(nil)

// dialRelay opens the relay websocket for port and adapts it to a net.Conn.
func (c *remoteAgentClient) dialRelay(ctx context.Context, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("remote agent-server: relay port %d out of range", port)
	}
	return c.dialRelayQuery(ctx, agentproto.RelayPortQueryParam, strconv.Itoa(port),
		fmt.Sprintf("nothing is listening on 127.0.0.1:%d in the workspace", port))
}

// dialRelayQuery opens the relay for one query selector (a port, or a
// server-owned target) and adapts it to a net.Conn. notServing is the message a
// 502 maps to.
func (c *remoteAgentClient) dialRelayQuery(ctx context.Context, key, value, notServing string) (net.Conn, error) {
	q := url.Values{}
	q.Set(key, value)
	q.Set(agentproto.AccessTokenQueryParam, c.token)
	u := c.wsBase + agentproto.RelayPath + "?" + q.Encode()

	wsTransport := c.transport.Clone()
	wsTransport.ResponseHeaderTimeout = remoteAgentWSHandshakeTimeout
	defer wsTransport.CloseIdleConnections()
	wsClient := *c.httpClient
	wsClient.Transport = wsTransport
	opts := &websocket.DialOptions{
		HTTPClient: &wsClient,
		HTTPHeader: http.Header{agentproto.AuthHeader: []string{agentproto.BearerScheme + c.token}},
	}
	dialCtx, cancel := context.WithTimeout(ctx, remoteAgentDialTimeout+remoteAgentWSHandshakeTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, u, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusBadGateway {
			return nil, errors.New(notServing)
		}
		return nil, fmt.Errorf("remote agent-server: dial port relay: %w",
			agentproto.RedactAccessTokenError(err, c.token))
	}
	conn.SetReadLimit(4 << 20)
	// The conn outlives the dial: bind it to Background, not ctx, so a request
	// context ending after the handshake does not tear down a long-lived relay.
	return websocket.NetConn(context.Background(), conn, websocket.MessageBinary), nil
}

// DialLoopback implements LoopbackRelayer. Relays are tracked so Kill closes
// them with the session, as the PTY brokers are.
func (s *remoteAgentServer) DialLoopback(ctx context.Context, port int) (net.Conn, error) {
	return s.trackedRelay(func() (net.Conn, error) { return s.rc.dialRelay(ctx, port) })
}

func (s *remoteAgentServer) trackedRelay(dial func() (net.Conn, error)) (net.Conn, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("session is shut down")
	}
	s.mu.Unlock()
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	tc := &trackedRelayConn{Conn: conn, owner: s}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return nil, fmt.Errorf("session is shut down")
	}
	if s.relays == nil {
		s.relays = map[*trackedRelayConn]struct{}{}
	}
	s.relays[tc] = struct{}{}
	s.mu.Unlock()
	return tc, nil
}

// closeRelays closes every open relay. Called by Kill after latching closed.
func (s *remoteAgentServer) closeRelays() {
	s.mu.Lock()
	relays := s.relays
	s.relays = nil
	s.mu.Unlock()
	for r := range relays {
		_ = r.Conn.Close()
	}
}

type trackedRelayConn struct {
	net.Conn
	owner *remoteAgentServer
}

func (t *trackedRelayConn) Close() error {
	t.owner.mu.Lock()
	delete(t.owner.relays, t)
	t.owner.mu.Unlock()
	return t.Conn.Close()
}
