package daemon

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// assertProbeFailed stands in for a liveness probe that cannot reach a verdict.
var assertProbeFailed = errors.New("probe failed")

// #2961: startup socket cleanup must PROVE a socket is dead before unlinking it.
//
// Editors are started with Setpgid, so they do not receive the daemon's SIGHUP and
// keep running across a restart or an upgrade. The sweep used to reason that a
// supervisor which has never spawned owns no editors, therefore every socket in the
// directory is abandoned — the second clause does not follow from the first. On a box
// that restarts its daemon on every upgrade, that cost a working editor per restart:
// unlinking a unix socket does not stop the listener, it only removes the name, so the
// editor lingers unreachable and every pane on it 502s.

// Every test here uses testguard.SocketTempDir rather than t.TempDir: on macOS the
// latter canonicalizes to a ~56-byte /private/var/folders/... path, and a vscode
// socket beneath it overruns sun_path's 104-byte limit, so net.Listen fails before
// the sweep is ever reached. Linux has 108 bytes and a short /tmp, which is why this
// passes there and fails only on Darwin.

// listeningSocket creates a real unix listener at path and returns it. This is the
// fixture the old sweep had no way to distinguish from litter.
func listeningSocket(t *testing.T, path string) net.Listener {
	t.Helper()
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestSweep_KeepsASocketAStillLiveEditorIsServing is the regression. A socket with a
// live listener must survive the sweep, and must still be connectable afterwards —
// asserting the file exists is not enough, since the bug's whole symptom is a
// listener that outlives its name.
func TestSweep_KeepsASocketAStillLiveEditorIsServing(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	dir, err := vscodeSocketDir()
	require.NoError(t, err)

	live := filepath.Join(dir, "93aa9bc2-a9009dfe.sock")
	listeningSocket(t, live)
	// The residue a SIGKILLed daemon leaves: a real socket FILE with no listener.
	dead := filepath.Join(dir, "93aa9bc2-00bf0126.sock")
	require.NoError(t, recreateOrphanSocket(dead))

	v := newVSCodeSupervisor()
	v.sweepAbandonedSockets()

	_, err = os.Stat(live)
	require.NoError(t, err, "a socket a live editor is serving must survive the sweep")
	conn, derr := net.Dial("unix", live)
	require.NoError(t, derr, "and must still be CONNECTABLE — the bug is a listener that outlives its name")
	require.NoError(t, conn.Close())

	_, err = os.Stat(dead)
	require.True(t, os.IsNotExist(err), "a socket with nothing listening is still swept — the feature still works")
}

// recreateOrphanSocket leaves a socket FILE at path with no listener behind it, the
// residue a SIGKILLed daemon leaves and the only thing the sweep should remove.
func recreateOrphanSocket(path string) error {
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	return l.Close()
}

// TestSweep_KeepsASocketWhoseOwnerIsAlive covers the second, independent signal: an
// owner record naming a live process group spares the socket even if the connect
// probe cannot reach a verdict. Either signal alone must be enough.
func TestSweep_KeepsASocketWhoseOwnerIsAlive(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	dir, err := vscodeSocketDir()
	require.NoError(t, err)
	sock := filepath.Join(dir, "93aa9bc2-a9009dfe.sock")
	require.NoError(t, recreateOrphanSocket(sock)) // nothing listening

	owner := vscodeOwnerRecord{
		Key: "repo/title", InstanceID: "inst", PID: 4242, StartID: 7,
		BootID: "boot", PIDNamespace: "ns", ProcessNonce: "nonce",
	}
	raw, err := json.Marshal(owner)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(vscodeOwnerPath(sock), raw, 0o600))

	v := newVSCodeSupervisor()
	v.groupAlive = func(int) (bool, error) { return true, nil } // the recorded editor lives
	v.sweepAbandonedSockets()

	_, err = os.Stat(sock)
	require.NoError(t, err, "a live owner record alone must spare the socket")
}

// TestSweep_UnknownLivenessKeepsTheSocket pins the BIAS, which is the opposite of the
// signalling path's. stopPersistedOwner needs strong proof before it signals, because
// signalling the wrong process kills a stranger's work. Deleting a name needs only a
// hint of life to abstain, because a stale file costs an inode and a deleted live one
// costs a running editor with no diagnostic beyond a 502.
func TestSweep_UnknownLivenessKeepsTheSocket(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	dir, err := vscodeSocketDir()
	require.NoError(t, err)
	sock := filepath.Join(dir, "93aa9bc2-a9009dfe.sock")
	require.NoError(t, recreateOrphanSocket(sock))

	owner := vscodeOwnerRecord{
		Key: "repo/title", InstanceID: "inst", PID: 4242, StartID: 7,
		BootID: "boot", PIDNamespace: "ns", ProcessNonce: "nonce",
	}
	raw, err := json.Marshal(owner)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(vscodeOwnerPath(sock), raw, 0o600))

	v := newVSCodeSupervisor()
	v.groupAlive = func(int) (bool, error) { return false, assertProbeFailed }
	v.sweepAbandonedSockets()

	_, err = os.Stat(sock)
	require.NoError(t, err, "an UNDECIDABLE liveness probe must keep the socket, never delete on doubt")
}

// TestRemoveRuntimeSockets_RefusesALiveEditorSocket pins the reset path's half of
// #2961. RemoveRuntimeSockets already refuses to unlink a live DAEMON's socket and
// explains why (#767: it keeps serving an inode with no name); editor sockets are the
// ones that most often outlive the daemon, since Setpgid spares them its SIGHUP, so
// "no daemon is answering" says nothing about them.
//
// The error is a SENTINEL because the caller must react differently: a live editor is
// a precondition failure for a destructive wipe, not litter to mention in passing.
func TestRemoveRuntimeSockets_RefusesALiveEditorSocket(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	dir, err := vscodeSocketDir()
	require.NoError(t, err)

	live := filepath.Join(dir, "93aa9bc2-a9009dfe.sock")
	listeningSocket(t, live)
	dead := filepath.Join(dir, "93aa9bc2-00bf0126.sock")
	require.NoError(t, recreateOrphanSocket(dead))

	removed, err := RemoveRuntimeSockets(home)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrLiveEditorSocket,
		"the caller has to distinguish this from ordinary socket litter, so it must be a sentinel")
	require.Contains(t, err.Error(), live, "and it must name the socket the operator has to deal with")

	_, statErr := os.Stat(live)
	require.NoError(t, statErr, "a live editor's socket must survive a reset attempt")
	conn, dialErr := net.Dial("unix", live)
	require.NoError(t, dialErr, "and stay connectable — the whole failure is a listener losing its name")
	require.NoError(t, conn.Close())

	// The genuinely stale one is still removed, so refusing is targeted rather than
	// giving up on the whole directory.
	require.NotContains(t, removed, live)
	_, deadErr := os.Stat(dead)
	require.True(t, os.IsNotExist(deadErr), "an abandoned socket is still cleared")
}
