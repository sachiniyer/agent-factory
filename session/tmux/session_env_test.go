package tmux

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
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
		_, environ, imports, err := session.launchEnvironment()
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

// An account name belongs to the agent namespace selected when the user chose
// it. A later program rewrite must not reinterpret that same string in another
// agent's account registry.
func TestLaunchEnvironmentRefusesCrossAgentAccountRewrite(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)

	// The persisted instance still says Claude, while restore has re-resolved the
	// current override to Codex before refreshing the account selection.
	session := NewTmuxSession("cross-agent-account", "codex")
	session.SetAccountForAgent("claude", "work")

	_, _, _, err := session.launchEnvironment()
	if err == nil {
		t.Fatal("a Claude account was reinterpreted as a same-named Codex account after the program changed")
	}
	if !strings.Contains(err.Error(), "selected for claude") || !strings.Contains(err.Error(), "resolves to codex") {
		t.Fatalf("cross-agent refusal did not name both namespaces: %v", err)
	}
}

func TestSiblingSessionsInheritAccountEnvironmentMode(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	accountDir, err := agentaccount.Register(home, ProgramCodex, "work")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "/ambient/codex")
	t.Setenv("OPENAI_API_KEY", "ambient-secret")

	agent := NewTmuxSession("account-parent", "codex")
	agent.SetAccountForAgent("codex", "work")
	_, _, _, agentSessionEnv, agentDefault, err := agent.prepareLaunchEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := launchEnvironmentValue(agentSessionEnv, "CODEX_HOME"); !ok || got != accountDir {
		t.Fatalf("agent session CODEX_HOME = %q, %v; want selected root %q", got, ok, accountDir)
	}
	if !strings.Contains(agentDefault, sessionenv.AccountEnvironmentExecMarker) || !strings.Contains(agentDefault, "/bin/sh -i") {
		t.Fatalf("agent default window command is not a scoped startup-free shell: %q", agentDefault)
	}

	process := agent.NewSiblingSession("account-process", "make -j4")
	wrapped, environ, imports, err := process.launchEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped, sessionenv.AccountEnvironmentExecMarker) || !strings.Contains(wrapped, "work") {
		t.Fatalf("process sibling launch is not scoped to the parent account: %q", wrapped)
	}
	for _, name := range []string{"CODEX_HOME", "OPENAI_API_KEY"} {
		if !slices.Contains(imports, name) {
			t.Fatalf("process sibling did not explicitly unset stale tmux %s", name)
		}
	}
	if got, ok := launchEnvironmentValue(environ, "CODEX_HOME"); ok {
		t.Fatalf("process sibling exposed selected CODEX_HOME %q through the tmux client environment", got)
	}
	if launchEnvironmentHasName(environ, "OPENAI_API_KEY") {
		t.Fatal("process sibling exposed the competing ambient API key to the tmux session environment")
	}
	_, _, _, processSessionEnv, processDefault, err := process.prepareLaunchEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := launchEnvironmentValue(processSessionEnv, "CODEX_HOME"); !ok || got != accountDir {
		t.Fatalf("process sibling session CODEX_HOME = %q, %v; want selected root %q", got, ok, accountDir)
	}
	if !strings.Contains(processDefault, sessionenv.AccountEnvironmentExecMarker) || !strings.Contains(processDefault, "/bin/sh -i") {
		t.Fatalf("process sibling default window command is not a scoped startup-free shell: %q", processDefault)
	}

	shell, err := agent.NewShellSiblingSession("account-shell", "/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	wrapped, environ, imports, err = shell.launchEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped, sessionenv.AccountEnvironmentExecMarker) || !strings.Contains(wrapped, "work") {
		t.Fatalf("shell sibling launch is not scoped to the parent account: %q", wrapped)
	}
	for _, name := range []string{"CODEX_HOME", "OPENAI_API_KEY"} {
		if !slices.Contains(imports, name) {
			t.Fatalf("shell sibling did not explicitly unset stale tmux %s", name)
		}
	}
	if got, ok := launchEnvironmentValue(environ, "CODEX_HOME"); ok {
		t.Fatalf("shell sibling exposed selected CODEX_HOME %q through the tmux client environment", got)
	}
	if launchEnvironmentHasName(environ, "OPENAI_API_KEY") {
		t.Fatal("shell sibling exposed the competing ambient API key to the tmux session environment")
	}
	if shell.Program() != "/bin/sh -i" {
		t.Fatalf("account shell program = %q, want startup-file-free interactive shell", shell.Program())
	}
	_, _, _, shellSessionEnv, shellDefault, err := shell.prepareLaunchEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := launchEnvironmentValue(shellSessionEnv, "CODEX_HOME"); !ok || got != accountDir {
		t.Fatalf("shell sibling session CODEX_HOME = %q, %v; want selected root %q", got, ok, accountDir)
	}
	if shellDefault != wrapped {
		t.Fatalf("account shell default window command = %q, want initial scoped shell %q", shellDefault, wrapped)
	}
}

func TestInlineClaudeCloudModeImportsProviderCredentials(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AZURE_CLIENT_SECRET", "fixture")

	session := NewTmuxSession("inline-cloud-mode", "CLAUDE_CODE_USE_BEDROCK=1 claude")
	_, environ, imports, err := session.launchEnvironment()
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
	_, ok := launchEnvironmentValue(environ, name)
	return ok
}

func launchEnvironmentValue(environ []string, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
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
