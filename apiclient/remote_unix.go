package apiclient

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// A unix:///absolute/path target is how an in-sandbox af reaches the daemon
// (#5571 W3.4, #5799): the supervised ssh connection carries a 0600 socket inside
// the session's own 0700 directory to the daemon's sandbox-only endpoint. No TCP
// port exists on the sandbox host for another user to bind or connect to, and
// the bearer credential is only ever written to a socket in that directory.
//
// A missing or dead socket (the forward is mid-re-dial) is an ordinary dial
// error: the call fails and the caller retries. There is deliberately no
// fallback to any other address — a client that cannot reach the carrier must
// not send its credential anywhere else.

// parseUnixDaemonURL recognises a unix:// target. ok is false for any other
// scheme, which the caller parses as http/ws.
func parseUnixDaemonURL(daemonURL string) (socket string, ok bool, err error) {
	u, perr := url.Parse(strings.TrimSpace(daemonURL))
	if perr != nil || !strings.EqualFold(u.Scheme, "unix") {
		return "", false, nil
	}
	path := u.Path
	if u.Host != "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", true, fmt.Errorf("invalid --daemon-url %q: a unix target must be unix:///absolute/clean/path.sock", daemonURL)
	}
	return path, true, nil
}

// newRemoteUnix is NewRemote over a unix socket: bearer token on every call, the
// remote request deadline, and a transport that dials only socket.
func newRemoteUnix(socket, token string) *Client {
	dialer := &net.Dialer{Timeout: remoteDialTimeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{
		httpClient:      &http.Client{Transport: transport},
		remoteTransport: transport,
		token:           token,
		httpBase:        localHTTPBase,
		wsBase:          localWSBase,
		requestTimeout:  remoteRequestTimeout,
	}
}
