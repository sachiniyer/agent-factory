package commands

import (
	"bytes"
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

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
// AGENT_FACTORY_HOME, so whatever `af config set` dials reaches the stub.
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

// TestConfigSetRefusedWhileDaemonQuiescing pins the CLI half of #3231: while
// the daemon refuses mutations for an upgrade hand-off, `af config set` must
// get the same admission answer the web form gets — a refusal that lands BEFORE
// the file write — instead of writing config.toml and live-applying it through
// the ungated path.
func TestConfigSetRefusedWhileDaemonQuiescing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	leaveAmbientRepo(t)
	path := filepath.Join(home, "config.toml")
	orig := "# hi\ndefault_program = 'claude'\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	serveQuiescingControl(t)

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := configSetCmd.RunE(cmd, []string{"default_program", "codex"})
	if err == nil {
		t.Fatal("config set must fail while the daemon refuses mutations for an upgrade hand-off")
	}
	if !daemon.IsDaemonQuiescingErr(err) {
		t.Errorf("the CLI must surface the daemon's own admission refusal, got: %v", err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != orig {
		t.Errorf("the refusal must land BEFORE the file write — config.toml changed:\n got: %q\nwant: %q", got, orig)
	}
}
