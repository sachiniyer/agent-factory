package tmux

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

type captureLaunchEnvPty struct {
	cmd *exec.Cmd
}

func (p *captureLaunchEnvPty) Start(command *exec.Cmd) (*os.File, error) {
	p.cmd = command
	return nil, errors.New("stop after capturing launch command")
}

func (*captureLaunchEnvPty) Close() {}

func forceSessionEnvExecutable(t *testing.T, path string) {
	t.Helper()
	previous := sessionEnvExecutable
	sessionEnvExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { sessionEnvExecutable = previous })
}

func wrappedProgramForTest(t *testing.T, executable, program string) string {
	t.Helper()
	wrapped, err := sessionenv.WrapCommand(executable, DetectAgentFromCommand(program), nil, program)
	if err != nil {
		t.Fatal(err)
	}
	return wrapped
}

// TestStartDoesNotGiveTmuxAmbientSecrets exercises Start's production spawn
// choke point. A nil Cmd.Env means os/exec inherits the daemon's entire
// environment, so inspect the effective environment rather than treating nil
// as empty.
func TestStartDoesNotGiveTmuxAmbientSecrets(t *testing.T) {
	const secretName = "AF_TEST_UNRELATED_SECRET"
	t.Setenv(secretName, "must-not-reach-session")
	forceNewSessionEnvMarkers(t, false)

	pty := &captureLaunchEnvPty{}
	execu := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	session := NewTmuxSessionWithDeps("env-boundary", "sh", pty, execu)
	if err := session.Start(t.TempDir()); err == nil {
		t.Fatal("Start unexpectedly succeeded after the capture factory stopped it")
	}
	if pty.cmd == nil {
		t.Fatal("Start never reached the tmux launch command")
	}

	effective := pty.cmd.Env
	if effective == nil {
		effective = os.Environ()
	}
	for _, entry := range effective {
		if strings.HasPrefix(entry, secretName+"=") {
			t.Fatalf("tmux launch inherited disallowed variable %s", secretName)
		}
	}
}

func TestStartAllowsConfiguredExactVariable(t *testing.T) {
	const allowedName = "CUSTOM_PROVIDER_TOKEN"
	t.Setenv(allowedName, "test-value")
	forceNewSessionEnvMarkers(t, false)

	pty := &captureLaunchEnvPty{}
	execu := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	session := NewTmuxSessionWithDeps("env-extension", "codex", pty, execu)
	if err := session.SetEnvPassthrough([]string{allowedName}); err != nil {
		t.Fatal(err)
	}
	_ = session.Start(t.TempDir())
	if pty.cmd == nil {
		t.Fatal("Start never reached the tmux launch command")
	}

	found := false
	for _, entry := range pty.cmd.Env {
		if strings.HasPrefix(entry, allowedName+"=") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("tmux launch omitted configured variable %s", allowedName)
	}
	if strings.Contains(pty.cmd.String(), "test-value") {
		t.Fatal("tmux argv rendered an environment value")
	}
}
