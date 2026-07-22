package tmux

import (
	"errors"
	"os"
	"os/exec"
	"slices"
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
	wrapped, err := sessionenv.WrapCommand(executable, sessionenv.AgentForCommand(program), nil, program)
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

func TestAgentNameUsedAsDataDoesNotSelectCredentialAllowlist(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("ANTHROPIC_API_KEY", "fixture")

	for _, program := range []string{
		"./collect codex",
		"/srv/af agent-server --listen :43110 --repo /workspace --title codex",
	} {
		session := NewTmuxSession("agent-name-data", program)
		_, environ, imports, err := session.launchEnvironment(program)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
			if launchEnvironmentHasName(environ, name) || slices.Contains(imports, name) {
				t.Fatalf("program %q selected %s from an agent-looking data argument", program, name)
			}
		}
	}
}

func TestInlineClaudeCloudModeImportsProviderCredentials(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AZURE_CLIENT_SECRET", "fixture")

	session := NewTmuxSession("inline-cloud-mode", "CLAUDE_CODE_USE_BEDROCK=1 claude")
	_, environ, imports, err := session.launchEnvironment("CLAUDE_CODE_USE_BEDROCK=1 claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if !launchEnvironmentHasName(environ, name) {
			t.Fatalf("inline Claude Bedrock mode omitted %s from the launch environment", name)
		}
		if !slices.Contains(imports, name) {
			t.Fatalf("inline Claude Bedrock mode omitted %s from the tmux import list", name)
		}
	}
	if launchEnvironmentHasName(environ, "AZURE_CLIENT_SECRET") || slices.Contains(imports, "AZURE_CLIENT_SECRET") {
		t.Fatal("Claude Bedrock mode admitted an inactive Foundry credential")
	}
}

func launchEnvironmentHasName(environ []string, name string) bool {
	prefix := name + "="
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func TestStartImportsAllowedEnvironmentIntoExistingTmuxServer(t *testing.T) {
	const (
		allowedName = "CUSTOM_PROVIDER_TOKEN"
		deniedName  = "AF_TEST_UNRELATED_SECRET"
	)
	t.Setenv(allowedName, "test-value")
	t.Setenv(deniedName, "test-value")
	forceNewSessionEnvMarkers(t, false)

	pty := &captureLaunchEnvPty{}
	execu := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(command *exec.Cmd) ([]byte, error) {
			if len(command.Args) >= 2 && command.Args[1] == "show-options" {
				return []byte("DISPLAY SSH_AUTH_SOCK\n"), nil
			}
			return nil, nil
		},
	}
	session := NewTmuxSessionWithDeps("existing-server-env", "codex", pty, execu)
	if err := session.SetEnvPassthrough([]string{allowedName}); err != nil {
		t.Fatal(err)
	}
	_ = session.Start(t.TempDir())
	if pty.cmd == nil {
		t.Fatal("Start never reached the tmux launch command")
	}

	var updateEnvironment string
	for idx, arg := range pty.cmd.Args {
		if arg == "update-environment" && idx+1 < len(pty.cmd.Args) {
			updateEnvironment = pty.cmd.Args[idx+1]
			break
		}
	}
	if updateEnvironment == "" {
		t.Fatal("existing tmux server launch did not override update-environment for the new session")
	}
	if !strings.Contains(" "+updateEnvironment+" ", " "+allowedName+" ") {
		t.Fatalf("tmux update-environment omitted configured variable name %s", allowedName)
	}
	if strings.Contains(updateEnvironment, deniedName) {
		t.Fatalf("tmux update-environment admitted disallowed variable name %s", deniedName)
	}
	for _, arg := range pty.cmd.Args {
		if strings.Contains(arg, "test-value") {
			t.Fatal("tmux argv rendered an environment value")
		}
	}
	var updateValues []string
	for idx, arg := range pty.cmd.Args {
		if arg == "update-environment" && idx+1 < len(pty.cmd.Args) {
			updateValues = append(updateValues, pty.cmd.Args[idx+1])
		}
	}
	if len(updateValues) != 2 || updateValues[1] != "DISPLAY SSH_AUTH_SOCK" {
		t.Fatalf("tmux launch did not restore the prior update-environment option: %q", updateValues)
	}
}

func TestStartSurfacesUnexpectedEnvironmentImportFailure(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	pty := &captureLaunchEnvPty{}
	execu := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return errors.New("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, errors.New("permission denied")
		},
	}
	session := NewTmuxSessionWithDeps("environment-import-error", "codex", pty, execu)
	err := session.Start(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Start() error = %v, want the environment import failure", err)
	}
	if pty.cmd != nil {
		t.Fatal("Start launched a pane after it could not determine the existing server environment policy")
	}
}
