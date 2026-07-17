package daemon

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestSnapshotNoSpawn_NoDaemonReturnsUnavailable verifies the CLI read path's
// core contract (#1029 PR 2): with no daemon serving the control socket,
// SnapshotNoSpawn returns ErrDaemonUnavailable and MUST NOT spawn a daemon. It
// only dials the (absent) socket, so it is safe to run outside the container —
// unlike the EnsureDaemon-backed paths, it never launches a process.
func TestSnapshotNoSpawn_NoDaemonReturnsUnavailable(t *testing.T) {
	// Fresh home => no daemon.sock exists, so the dial fails fast.
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	// Guard against an accidental spawn: if SnapshotNoSpawn ever reached the
	// EnsureDaemon path, launchDaemonProcessFn would fire. Make that a test
	// failure rather than a silently-launched daemon.
	prev := launchDaemonProcessFn
	launchDaemonProcessFn = func() error {
		t.Fatalf("SnapshotNoSpawn must never spawn a daemon")
		return nil
	}
	t.Cleanup(func() { launchDaemonProcessFn = prev })

	data, err := SnapshotNoSpawn(SnapshotRequest{})
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("SnapshotNoSpawn with no daemon = (%v, %v), want ErrDaemonUnavailable", data, err)
	}
	if data != nil {
		t.Fatalf("expected nil instances on the unavailable path, got %+v", data)
	}
}

// TestSnapshotNoSpawn_RepoScopedRequest is a light compile/behavior check that a
// repo-scoped request also degrades to ErrDaemonUnavailable with no daemon (the
// scoping is enforced server-side; the client just forwards RepoID).
func TestSnapshotNoSpawn_RepoScopedRequest(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	rid := config.RepoIDFromRoot("/tmp/nospawn-repo")
	if _, err := SnapshotNoSpawn(SnapshotRequest{RepoID: rid}); !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("repo-scoped SnapshotNoSpawn with no daemon: want ErrDaemonUnavailable, got %v", err)
	}
}
