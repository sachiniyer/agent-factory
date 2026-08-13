package ui

import (
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/daemon"
)

// quiescingControlStub answers the daemon control socket the way a real daemon
// does during an upgrade hand-off: SetConfigValue is refused by the lifecycle
// admission gate before anything reaches disk. ApplyConfig deliberately answers
// OK — a client that writes the file first and asks for a live apply afterwards
// would see that sequence "succeed", so the assertions below fail against that
// ordering instead of accidentally passing because the apply errored.
type quiescingControlStub struct{}

func (s *quiescingControlStub) SetConfigValue(_ daemon.SetConfigValueRequest, _ *daemon.SetConfigValueResponse) error {
	// The stable wire prefix a quiescing daemon answers with (matched by
	// daemon.IsDaemonQuiescingErr); net/rpc flattens it to this text on the wire.
	return errors.New("agent-factory daemon is handing off to an upgrade; retry shortly")
}

func (s *quiescingControlStub) ApplyConfig(_ daemon.ApplyConfigRequest, _ *daemon.ApplyConfigResponse) error {
	return nil
}

// serveQuiescingControl binds the real control socket path inside the test's
// AGENT_FACTORY_HOME, so whatever the pane's save path dials reaches the stub.
func serveQuiescingControl(t *testing.T) {
	t.Helper()
	socketPath, err := daemon.DaemonSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := rpc.NewServer()
	if err := server.RegisterName("Control", &quiescingControlStub{}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()
}

// TestConfigPaneSaveRefusedByQuiescingDaemonWritesNothing pins the TUI half of
// #3231: a config save while the daemon is quiescing for an upgrade hand-off
// must get the same admission answer the web form gets — a refusal that lands
// BEFORE the file write — instead of writing config.toml and live-applying it
// through the ungated path.
func TestConfigPaneSaveRefusedByQuiescingDaemonWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, "config.toml")
	orig := "# hand-written\ndefault_program = 'claude'\n"
	if err := os.WriteFile(tomlPath, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	serveQuiescingControl(t)

	c := newTestConfigPane(t)
	selectKey(t, c, "default_program")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("")
	typeInto(c, "codex")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	if !c.statusIsError {
		t.Errorf("a save the daemon refuses must surface as an error, got status %q", c.status)
	}
	if !strings.Contains(c.status, "handing off to an upgrade") {
		t.Errorf("the pane must show the daemon's own refusal, got %q", c.status)
	}
	written, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != orig {
		t.Errorf("the refusal must land BEFORE the file write — config.toml changed:\n got: %q\nwant: %q", written, orig)
	}
}
