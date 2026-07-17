package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sockpath"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These pin #1940: the daemon's OWN sockets get the same length guard the VS
// Code socket next door has had all along.
//
// The asymmetry was the whole bug. A secondary feature (the editor pane) failed
// with an error naming the path, the limit and AGENT_FACTORY_HOME, while the
// core control plane failed with "bind: invalid argument" — a bare kernel errno
// naming nothing at all, from which no user could work out that their AF home
// was the problem.

// TestDaemonSocketPathsRejectOverlongHome is the regression: both daemon
// sockets must refuse an over-long home at RESOLUTION, before net.Listen turns
// it into an opaque errno.
func TestDaemonSocketPathsRejectOverlongHome(t *testing.T) {
	deep := filepath.Join(testguard.SocketTempDir(t), strings.Repeat("d", 80), strings.Repeat("e", 80))
	t.Setenv("AGENT_FACTORY_HOME", deep)

	for _, tc := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"control socket", DaemonSocketPath},
		{"HTTP socket", DaemonHTTPSocketPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := tc.fn()
			if err == nil {
				t.Fatalf("resolved %q (%d bytes) with no error; net.Listen would fail as a bare "+
					"'bind: invalid argument' naming nothing (#1940)", path, len(path))
			}
			if !strings.Contains(err.Error(), "AGENT_FACTORY_HOME") {
				t.Errorf("error %q does not name AGENT_FACTORY_HOME — the one knob that fixes it", err)
			}
		})
	}
}

// TestDaemonSocketPathsAcceptDefaultHome is the guard on the guard. This runs
// on the daemon's startup path, so a spurious or off-by-one check would stop af
// starting for EVERYONE — strictly worse than the unactionable message it
// replaces. An ordinary home must resolve cleanly.
func TestDaemonSocketPathsAcceptDefaultHome(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	for _, tc := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"control socket", DaemonSocketPath},
		{"HTTP socket", DaemonHTTPSocketPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.fn(); err != nil {
				t.Errorf("%s rejected an ordinary home: %v", tc.name, err)
			}
		})
	}
}

// TestRealisticMacDefaultHomeResolves walks the arithmetic through the REAL
// resolution path rather than a recomputation of it: a stock macOS home must
// leave both sockets comfortably inside darwin's 103-byte ceiling.
//
// sockpath.Portable is darwin's ceiling, so this asserts the mac outcome on any
// runner. See internal/sockpath for the full table and the verdict: a default
// install needs a 65-character username to overrun, which is why #1940 is a
// real-but-narrow defect rather than "af does not work on my Mac".
func TestRealisticMacDefaultHomeResolves(t *testing.T) {
	for _, user := range []string{"pradeepiyer", "firstname.lastname"} {
		t.Run(user, func(t *testing.T) {
			home := filepath.Join("/Users", user, ".agent-factory")
			for _, name := range []string{daemonSocketFileName, daemonHTTPSocketFileName} {
				path := filepath.Join(home, name)
				if len(path) > sockpath.Portable {
					t.Errorf("%s is %d bytes, over darwin's %d — a DEFAULT mac install cannot be broken by this guard",
						path, len(path), sockpath.Portable)
				}
			}
		})
	}
}
