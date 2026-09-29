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
	daemonDialTimeout        = 250 * time.Millisecond
	// shutdownAckGrace delays the daemon main-loop teardown after a Shutdown
	// RPC handler returns so the response can flush back to the caller before
	// the listener closes.
	shutdownAckGrace = 50 * time.Millisecond
)

// daemonReadyTimeout is a var so the handful of tests that deliberately spend
// a readiness window can shrink it rather than burn five seconds of suite
// wall-clock each (#4464); production never assigns it. Same seam as
// upgradeGateTimeout.
var daemonReadyTimeout = 5 * time.Second
