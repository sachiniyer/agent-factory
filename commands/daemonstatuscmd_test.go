package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestCollectDaemonStatusNoDaemon runs the read-only probe against a fresh
// temp home where no daemon is running: it must report not-running and resolve
// both socket paths under that home without dialing or spawning anything.
func TestCollectDaemonStatusNoDaemon(t *testing.T) {
	home := t.TempDir()
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
