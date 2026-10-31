package tmux

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

type captureLaunchEnvPty struct {
	cmd *exec.Cmd
}

func (p *captureLaunchEnvPty) Start(command *exec.Cmd) (*os.File, error) {
	p.cmd = command
	return nil, errors.New("stop after capturing launch command")
}

// forceAgentOnSearchPath plants executable stubs for the named agents in a
// private directory and makes it the whole PATH: the launcher's grant
// predicate models execvp's walk, so a bare agent name earns its credential
// grant only when a real executable file answers it (#5265).
func forceAgentOnSearchPath(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

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
		_, environ, imports, _, _, err := session.prepareLaunchEnvironment("")
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

	_, _, _, _, _, err := session.prepareLaunchEnvironment("")
	if err == nil {
		t.Fatal("a Claude account was reinterpreted as a same-named Codex account after the program changed")
	}
	if !strings.Contains(err.Error(), "selected for claude") || !strings.Contains(err.Error(), "resolves to codex") {
		t.Fatalf("cross-agent refusal did not name both namespaces: %v", err)
	}
}

// A scoped session's account material may only ride an executable af can
// attribute to the operator — the same grant question the ambient filter
// answers. A bare program that resolves through a repository-controlled PATH
// entry must refuse the launch rather than inject the selected account's
// environment into repository content (#5265 review).
func TestLaunchEnvironmentRefusesScopedLaunchWhenGrantFails(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	if _, err := agentaccount.Register(home, ProgramCodex, "work"); err != nil {
		t.Fatal(err)
	}
	// A PATH that resolves bare codex through the pane's working directory.
	t.Setenv("PATH", ".")

	session := NewTmuxSession("scoped-grant-fails", "codex")
	session.SetAccountForAgent("codex", "work")
	if _, _, _, _, _, err := session.prepareLaunchEnvironment(""); err == nil {
		t.Fatal("account-scoped launch on a repository-controlled PATH hit was not refused")
	} else if !strings.Contains(err.Error(), "cannot attribute") {
		t.Fatalf("scoped grant refusal lost its reason: %v", err)
	}
}

func TestSiblingSessionsInheritAccountEnvironmentMode(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceNewSessionEnvMarkers(t, true)
	// The scoped agent pane carries account material only on an executable af
	// can attribute to the operator, so the bare codex name needs a PATH hit.
	forceAgentOnSearchPath(t, "codex")
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
	if err := agent.SetEnvPassthrough([]string{"BASH_ENV", "PS1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", "/tmp/ambient-bashrc")
	t.Setenv("PS1", "$((CODEX_HOME=42))")
	_, agentLaunchEnv, agentImports, agentSessionEnv, agentDefault, err := agent.prepareLaunchEnvironment("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"BASH_ENV", "PS1"} {
		if launchEnvironmentHasName(agentLaunchEnv, name) {
			t.Fatalf("account tmux client environment retained shell startup hook %s", name)
		}
		if !slices.Contains(agentImports, name) {
			t.Fatalf("account tmux launch did not explicitly unset stale shell startup hook %s", name)
		}
	}
	if got, ok := launchEnvironmentValue(agentSessionEnv, "CODEX_HOME"); !ok || got != accountDir {
		t.Fatalf("agent session CODEX_HOME = %q, %v; want selected root %q", got, ok, accountDir)
	}
	if !strings.Contains(agentDefault, sessionenv.AccountEnvironmentExecMarker) || !strings.Contains(agentDefault, "/bin/sh -i") {
		t.Fatalf("agent default window command is not a scoped startup-free shell: %q", agentDefault)
	}

	process := agent.NewSiblingSession("account-process", "make -j4")
	wrapped, environ, imports, _, _, err := process.prepareLaunchEnvironment("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped, sessionenv.AccountEnvironmentExecMarker) {
		t.Fatalf("process sibling launch is not scoped through the account boundary: %q", wrapped)
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
	_, _, _, processSessionEnv, processDefault, err := process.prepareLaunchEnvironment("")
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
	wrapped, environ, imports, _, _, err = shell.prepareLaunchEnvironment("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped, sessionenv.AccountEnvironmentExecMarker) {
		t.Fatalf("shell sibling launch is not scoped through the account boundary: %q", wrapped)
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
	_, _, _, shellSessionEnv, shellDefault, err := shell.prepareLaunchEnvironment("")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := launchEnvironmentValue(shellSessionEnv, "CODEX_HOME"); !ok || got != accountDir {
		t.Fatalf("shell sibling session CODEX_HOME = %q, %v; want selected root %q", got, ok, accountDir)
	}
	// Same shell, but its own per-pane capability: the initial pane's is single-use.
	if shellDefault == wrapped || !strings.HasSuffix(shellDefault, " "+shellquote.Quote(shell.Program())) {
		t.Fatalf("account shell default window command = %q, want the scoped shell with its own capability (initial %q)", shellDefault, wrapped)
	}
}

func TestInlineClaudeCloudModeImportsProviderCredentials(t *testing.T) {
	forceSessionEnvExecutable(t, "/opt/af")
	forceAgentOnSearchPath(t, "claude")
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AZURE_CLIENT_SECRET", "fixture")

	session := NewTmuxSession("inline-cloud-mode", "CLAUDE_CODE_USE_BEDROCK=1 claude")
	_, environ, imports, _, _, err := session.prepareLaunchEnvironment("")
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

func TestLegacyLaunchClearsInheritedSessionMarkers(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)
	forceSessionEnvExecutable(t, "/opt/af")
	t.Setenv(EnvMarkerSession, "af_parent")
	t.Setenv(EnvMarkerGeneration, "parent-generation")
	child := NewTmuxSession("child", "claude")
	_, environ, imports, _, _, err := child.prepareLaunchEnvironment("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{EnvMarkerSession, EnvMarkerGeneration} {
		if launchEnvironmentHasName(environ, name) {
			t.Errorf("child inherited %s", name)
		}
		if !slices.Contains(imports, name) {
			t.Errorf("must unset stale server %s through update-environment", name)
		}
	}
}

// TestStartRestoresUpdateEnvironmentAfterFailedLaunch covers #5390's primary
// property: once the expansion is queued, EVERY Start exit path re-applies the
// baseline. The in-queue restore dies with the queue on a mid-queue failure —
// tmux abandons a ;-chained queue at the first failed command — so the restore
// must also run as its own bounded command after the launch attempt ends.
func TestStartRestoresUpdateEnvironmentAfterFailedLaunch(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)

	var restores []string
	pty := &captureLaunchEnvPty{}
	execu := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			for idx, arg := range c.Args {
				if arg == "update-environment" && idx+1 < len(c.Args) {
					restores = append(restores, c.Args[idx+1])
				}
			}
			if len(c.Args) >= 2 && c.Args[1] == "set-option" {
				return nil
			}
			return errors.New("session not found")
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) >= 2 && c.Args[1] == "show-options" {
				return []byte("DISPLAY SSH_AUTH_SOCK\n"), nil
			}
			return nil, nil
		},
	}
	session := NewTmuxSessionWithDeps("restore-on-failure", "codex", pty, execu)
	if err := session.Start(t.TempDir()); err == nil {
		t.Fatal("Start unexpectedly succeeded after the capture factory stopped it")
	}
	if !slices.Contains(restores, "DISPLAY SSH_AUTH_SOCK") {
		t.Fatalf("failed launch never re-applied the prior update-environment outside the queue: %q", restores)
	}
}

// TestStartHealsCredentialNamesLeftInUpdateEnvironment covers the other half
// of #5390: a server an older build already left expanded cannot recover its
// true pre-expansion value, but af never intends credential import names to
// persist — the restore baseline drops them instead of re-persisting the leak.
func TestStartHealsCredentialNamesLeftInUpdateEnvironment(t *testing.T) {
	forceNewSessionEnvMarkers(t, false)

	var restores []string
	pty := &captureLaunchEnvPty{}
	execu := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			for idx, arg := range c.Args {
				if arg == "update-environment" && idx+1 < len(c.Args) {
					restores = append(restores, c.Args[idx+1])
				}
			}
			if len(c.Args) >= 2 && c.Args[1] == "set-option" {
				return nil
			}
			return errors.New("session not found")
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if len(c.Args) >= 2 && c.Args[1] == "show-options" {
				// An older build left the option expanded to a claude
				// session's credential import list.
				return []byte("ANTHROPIC_API_KEY DISPLAY OPENAI_API_KEY SSH_AUTH_SOCK\n"), nil
			}
			return nil, nil
		},
	}
	session := NewTmuxSessionWithDeps("heal-expanded", "codex", pty, execu)
	_ = session.Start(t.TempDir())

	const want = "DISPLAY SSH_AUTH_SOCK"
	if len(restores) == 0 {
		t.Fatal("failed launch never re-applied update-environment")
	}
	for _, got := range restores {
		if got != want {
			t.Fatalf("restored update-environment %q kept credential names; want %q", got, want)
		}
	}
	// The queue's own trailing restore targets the same healed baseline.
	var tail string
	for idx, arg := range pty.cmd.Args {
		if arg == "update-environment" && idx+1 < len(pty.cmd.Args) {
			tail = pty.cmd.Args[idx+1]
		}
	}
	if tail != want {
		t.Fatalf("queue tail restore = %q, want %q", tail, want)
	}
}
