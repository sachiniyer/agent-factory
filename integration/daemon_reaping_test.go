package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestHarnessReapsADaemonThePIDFileNoLongerNames pins the reaper this package's
// harnesses register (#3842).
//
// The residue on the maintainer's box was not "a test forgot to clean up": every
// harness here already killed the daemon named by daemon.pid, in the right
// cleanup order. It leaked because that file names ONE daemon, the last to write
// it, while these tests deliberately run several — and the daemon whose entry was
// overwritten is invisible to a PID-file reaper. Three of them were found still
// running eleven days after the run that spawned them, holding homes that had
// been deleted the moment the test ended.
//
// So this reproduces that shape directly: a live daemon the PID file does not
// name, and a cleanup that must find and stop it anyway.
func TestHarnessReapsADaemonThePIDFileNoLongerNames(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real daemon; skipped under -short — see #2052")
	}
	// pgrep is the mechanism under test (a PID-file reaper cannot see this
	// daemon by construction), so its absence is a skip, not a failure.
	requireTool(t, "pgrep")
	testguard.IsolateTmux(t)

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	bin := buildBinary(t)

	cmd := exec.Command(bin, "--daemon")
	cmd.Env = append(os.Environ(), "AGENT_FACTORY_HOME="+home, "TERM=xterm")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	// Reap it here so the assertion below sees a gone process rather than a
	// zombie this test itself is holding open.
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid

	waitUntil(t, 30*time.Second, "the daemon writes its pid file", func() bool {
		got, ok := daemonPIDFromHome(home)
		return ok && got == pid
	})

	// The #3842 shape: nothing points at this daemon any more.
	if err := os.Remove(filepath.Join(home, "daemon.pid")); err != nil {
		t.Fatalf("remove daemon.pid: %v", err)
	}

	stopDaemons(t, bin, home)

	if pidAlive(pid) {
		t.Fatalf("daemon pid %d survived the harness cleanup; the home %s is about to be deleted "+
			"out from under a live daemon (#3842)", pid, home)
	}
}
