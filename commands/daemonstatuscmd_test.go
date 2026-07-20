package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestCollectDaemonStatusNoDaemon runs the read-only probe against a fresh
// temp home where no daemon is running: it must report not-running and resolve
// both socket paths under that home without dialing or spawning anything.
func TestCollectDaemonStatusNoDaemon(t *testing.T) {
	// SocketTempDir, not t.TempDir: this resolves the daemon socket paths, and on
	// macOS a t.TempDir() home is ~107 bytes — past sun_path, so resolution now
	// fails with the #1940 guard. The real home (~/.agent-factory) is short.
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	info := collectDaemonStatus()
	if info.Running {
		t.Fatal("expected Running=false with no daemon in a fresh home")
	}
	if !strings.HasPrefix(info.ControlSocket, home) {
		t.Fatalf("control socket %q not under temp home %q", info.ControlSocket, home)
	}
	if !strings.HasPrefix(info.HTTPSocket, home) {
		t.Fatalf("http socket %q not under temp home %q", info.HTTPSocket, home)
	}
	if info.ControlSocketFile || info.HTTPSocketFile {
		t.Fatal("no socket files should exist in a fresh home")
	}
}

func TestPrintDaemonStatusHumanRunning(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	printDaemonStatusHuman(cmd, daemonStatusInfo{
		Running:           true,
		ControlSocket:     "/h/daemon.sock",
		ControlSocketFile: true,
		HTTPSocket:        "/h/daemon-http.sock",
		HTTPSocketFile:    true,
		PID:               42,
		PIDVerified:       true,
		AutostartUnit:     true,
		BinaryStale:       true,
	})
	got := out.String()
	for _, want := range []string{
		"daemon: running",
		"control socket: /h/daemon.sock (present)",
		"http socket:    /h/daemon-http.sock (present)",
		"pid:            42 (verified)",
		"autostart:      installed",
		"warning:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("human output missing %q\n%s", want, got)
		}
	}
}

func TestPrintDaemonStatusHumanNotRunning(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	printDaemonStatusHuman(cmd, daemonStatusInfo{
		Running:           false,
		ControlSocket:     "/h/daemon.sock",
		ControlSocketFile: false,
	})
	got := out.String()
	if !strings.Contains(got, "not running") {
		t.Errorf("expected not-running line, got:\n%s", got)
	}
	if !strings.Contains(got, "(absent)") {
		t.Errorf("expected absent socket label, got:\n%s", got)
	}
	if !strings.Contains(got, "no daemon.pid on disk") {
		t.Errorf("expected empty-pid line, got:\n%s", got)
	}
	if !strings.Contains(got, "af daemon install") {
		t.Errorf("expected autostart install hint, got:\n%s", got)
	}
}

// TestCollectDaemonStatusReportsExposureWithoutClaimingItCannotStart is the
// #2168 Phase 0 status surface, and it flips what #2090 asserted here.
//
// #2090 printed "not running and cannot start" on this config and suppressed the
// on-demand line. Both are now false: the daemon starts and serves. Keeping them
// would be a status command lying about a live capability — the same fabricated
// negative in the other direction. What survives is the exposure, reported as a
// warning alongside the ordinary on-demand wording.
func TestCollectDaemonStatusReportsExposureWithoutClaimingItCannotStart(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = false\n"), 0600))

	info := collectDaemonStatus()
	require.False(t, info.Running)
	require.NotEmpty(t, info.ExposureWarning, "an exposed listener must be reported, not implied")
	require.Contains(t, info.ExposureWarning, "require_token")
	require.Contains(t, info.ExposureWarning, "0.0.0.0:8443")

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printDaemonStatusHuman(cmd, info)
	got := out.String()
	require.Contains(t, got, "starts on demand",
		"the on-demand promise is true again — this config starts fine")
	require.NotContains(t, got, "cannot start",
		"there is no config the daemon refuses to start under any more")
	require.Contains(t, got, "warning:", "the exposure still has to reach the operator")
	require.Contains(t, got, "DeliverPrompt")
}

// TestCollectDaemonStatusSafeConfigIsUnwarned is the other direction: a user who
// simply has no daemon yet, on the shipped loopback default, sees no warning.
func TestCollectDaemonStatusSafeConfigIsUnwarned(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '127.0.0.1:8443'\nrequire_token = false\n"), 0600))

	info := collectDaemonStatus()
	require.Empty(t, info.ExposureWarning)

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printDaemonStatusHuman(cmd, info)
	require.Contains(t, out.String(), "starts on demand")
	require.NotContains(t, out.String(), "warning:")
}

// TestCollectDaemonStatusAuthenticatedNetworkBindIsUnwarned pins that
// require_token = true is untouched: the recommended remote posture draws no
// warning, so the warning keeps meaning something when it does appear.
func TestCollectDaemonStatusAuthenticatedNetworkBindIsUnwarned(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName),
		[]byte("listen_addr = '0.0.0.0:8443'\nrequire_token = true\n"), 0600))

	require.Empty(t, collectDaemonStatus().ExposureWarning)
}
