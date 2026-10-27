package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestStopDaemon_DoesNotUnlinkSymlinkedPIDFile_LiveDaemonFifthSite is the
// end-to-end exercise of the fifth os.Remove site (cleanupDaemonRuntimeFiles)
// under the stacked-misconfiguration the bug report's "Reachability of the
// fifth site" section describes: the user plants a symlink whose target holds
// a REAL running af daemon's PID, so isAgentFactoryDaemon passes, StopDaemon
// SIGTERMs the live daemon, the daemon's own removeDaemonPIDFile refuses the
// link, and then cleanupDaemonRuntimeFiles runs. Pre-fix this os.Remove'd the
// link; post-fix RemoveFileRefusingLink refuses it.
//
// This is the live-stop counterpart to TestStopDaemon_DoesNotUnlinkASymlinkedStalePIDFile
// (which exercises the four stale branches). Together they cover both shapes
// of the asymmetry.
func TestStopDaemon_DoesNotUnlinkSymlinkedPIDFile_LiveDaemonFifthSite(t *testing.T) {
	tmpHome := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmpHome)

	// Spawn a process that responds to SIGTERM and presents an "af --daemon"
	// argv so isAgentFactoryDaemon classifies it as ours (same recipe as
	// TestStopDaemon_SIGTERMFirst).
	cmd := spawnFakeDaemonProc(t, "af", "sleep 60; :", "--daemon", "af-test")
	pid := cmd.Process.Pid
	waitForReady(t, fmt.Sprintf("fake daemon pid=%d cmdline exposes --daemon", pid), func() bool {
		return isAgentFactoryDaemon(pid)
	})

	// Reap in a goroutine so /proc/<pid>/cmdline clears once the process
	// exits, mirroring TestStopDaemon_SIGTERMFirst — otherwise pidLooksAlive
	// keeps the zombie looking alive and StopDaemon waits the full grace.
	go func() { _, _ = cmd.Process.Wait() }()

	// Plant the stacked-misconfiguration: a symlinked daemon.pid whose TARGET
	// holds the live daemon's PID. writeDaemonPIDFile would refuse this link;
	// the user planted it by hand.
	target := filepath.Join(t.TempDir(), "daemon-pid-target")
	require.NoError(t, os.WriteFile(target, []byte(fmt.Sprintf("%d\n", pid)), 0600))
	link := filepath.Join(tmpHome, "daemon.pid")
	require.NoError(t, os.Symlink(target, link))

	stopped, err := StopDaemon()
	require.NoError(t, err, "StopDaemon must not error on a live af daemon behind a symlinked PID file")
	assert.True(t, stopped, "a live af daemon was signaled")

	// Give the reaper a moment to clear the zombie so pidLooksAlive agrees.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidLooksAlive(pid) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// THE FIFTH SITE: cleanupDaemonRuntimeFiles ran after the SIGTERM-success
	// path. Pre-fix it os.Remove'd the link. Post-fix RemoveFileRefusingLink
	// refuses it.
	info, lerr := os.Lstat(link)
	require.NoError(t, lerr, "the symlink af could not have written through must survive the stop-side cleanup (#3672)")
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"cleanupDaemonRuntimeFiles must not unlink a symlinked PID file af never wrote through (#3672)")
	assert.FileExists(t, target, "the target keeps the daemon's PID bytes")
}
