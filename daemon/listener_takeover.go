package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
)

// listener_takeover.go is the #6858 no-takeover gate for the restartable web
// listener: a daemon whose effective network.listen_addr is only the built-in
// default must never take the web port away from another AGENT_FACTORY_HOME's
// daemon — the failure mode of #6386's retry loop, where a second home's
// daemon retrying a held port wins it the moment the rightful owner's restart
// releases it, and the user's proxy, bookmarks and tokens then point at the
// wrong daemon.
//
// defaultWebListenAddr is what the gate measures "only the default" against:
// the built-in network.listen_addr value. It is a package var because a daemon
// test may never bind the shipped port itself (#6625) — a regression test
// needs the gate to fire on a real kernel socket, so it re-points this var at
// a free loopback port for the duration of the test.
var defaultWebListenAddr = config.DefaultListenAddr()
