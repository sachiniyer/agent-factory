package daemon

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The daemon's AF home is created before either socket binds — log.Initialize
// and config.LoadConfig write into it, and acquireHomeLock MkdirAll's it a few
// lines before the binds. So a home that is MISSING at bind time is never a
// first-start ordering; it is the abandoned-daemon signal #1094's
// watchDaemonHome exists to act on, and re-creating it is what kept 108 dead
// /tmp/af-* homes on the maintainer's box holding nothing but a daemon-http.sock
// (#3842). watchDaemonHome resets its miss counter on any successful stat, so a
// daemon that re-creates its own home never observes the deletion.
//
// Both tests below assert the two halves that matter: the bind FAILS with an
// error naming the home, and the home is still gone afterwards.

func TestStartHTTPServer_RefusesAVanishedHomeInsteadOfRecreatingIt(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(home))

	closeHTTP, err := startHTTPServer(m, newTaskScheduler(), nil)
	if err == nil {
		_ = closeHTTP()
	}
	require.Error(t, err, "binding the HTTP socket into a deleted home must fail")
	require.Contains(t, err.Error(), home, "the error must name the home that vanished")
	require.Contains(t, err.Error(), "removed after")

	_, statErr := os.Stat(home)
	require.True(t, errors.Is(statErr, os.ErrNotExist),
		"the HTTP bind must not re-create the agent-factory home: watchDaemonHome resets on any successful stat, so this keeps an abandoned daemon alive forever (#1094/#3842)")
}

func TestStartControlServer_RefusesAVanishedHomeInsteadOfRecreatingIt(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	require.NoError(t, os.RemoveAll(home))

	closeServer, err := startControlServer(nil, nil, nil, nil)
	if err == nil {
		_ = closeServer()
	}
	require.Error(t, err, "binding the control socket into a deleted home must fail")
	require.Contains(t, err.Error(), home, "the error must name the home that vanished")
	require.Contains(t, err.Error(), "removed after")

	_, statErr := os.Stat(home)
	require.True(t, errors.Is(statErr, os.ErrNotExist),
		"the control-socket bind must not re-create the agent-factory home (#1094/#3842)")
}
