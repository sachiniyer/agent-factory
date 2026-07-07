package daemon

import (
	"time"
)

const (
	controlServiceName   = "Control"
	daemonSocketFileName = "daemon.sock"
	// daemonHTTPSocketFileName is the Unix socket the daemon-hosted HTTP/JSON
	// server (#1029 PR 4) listens on, alongside — never multiplexed onto — the
	// gob net/rpc control socket above. One listener, one protocol.
	daemonHTTPSocketFileName = "daemon-http.sock"
	daemonReadyTimeout       = 5 * time.Second
	daemonDialTimeout        = 250 * time.Millisecond
	// shutdownAckGrace delays the daemon main-loop teardown after a Shutdown
	// RPC handler returns so the response can flush back to the caller before
	// the listener closes.
	shutdownAckGrace = 50 * time.Millisecond
)
