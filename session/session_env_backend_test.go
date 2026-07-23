package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestDockerEnvironmentDoesNotTrustRepoSelectedImageWithResolvedCredentials(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-value")
	t.Setenv("ANTHROPIC_API_KEY", "test-value")
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "docker",
		"docker":  map[string]any{"image": "example.invalid/agent:latest"},
		"program_overrides": map[string]any{
			tmux.ProgramClaude: tmux.ProgramCodex,
		},
	})
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	defer SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af"))()

	var runArgs []string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
		}
		return nil, fmt.Errorf("stop after capturing docker run")
	})()

	_, _ = (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "override-auth",
		Program:  tmux.ProgramClaude,
		CloneURL: "file:///fixture.git",
	})
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if dockerHasEnvName(runArgs, name) {
			t.Fatalf("docker forwarded built-in credential %s to a repo-selected image", name)
		}
	}
}

func TestProvisionEnvironmentGrantReachesRuntimeWithoutBecomingDurable(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	runtime := &specCapturingRuntime{res: ProvisionResult{Backend: NewFakeBackend()}}
	restore := SetRuntimeForTest(BackendDocker, func() Runtime { return runtime })
	t.Cleanup(restore)

	instance, err := NewInstance(InstanceOptions{
		Title:                          "provision-only-env",
		Path:                           repoRoot,
		Backend:                        BackendDocker,
		SessionEnvPassthrough:          []string{"DURABLE_OUTER_TOKEN"},
		ProvisionSessionEnvPassthrough: []string{"CURRENT_GLOBAL_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CURRENT_GLOBAL_TOKEN", "DURABLE_OUTER_TOKEN"} {
		if !slices.Contains(runtime.spec.SessionEnvPassthrough, name) {
			t.Fatalf("runtime provisioning omitted environment name %s", name)
		}
	}
	instance.mu.RLock()
	durable := append([]string(nil), instance.sessionEnvPassthrough...)
	instance.mu.RUnlock()
	if slices.Contains(durable, "CURRENT_GLOBAL_TOKEN") {
		t.Fatal("one-create global environment grant became a durable per-instance grant")
	}
	if !slices.Contains(durable, "DURABLE_OUTER_TOKEN") {
		t.Fatal("explicit outer-runtime environment grant was not retained")
	}
}

func TestHookEnvironmentUsesResolvedProgramOverride(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-value")
	t.Setenv("ANTHROPIC_API_KEY", "test-value")
	repoRoot := initTempGitRepo(t)
	scriptDir := t.TempDir()
	launch := writeScript(t, scriptDir, "launch.sh", `
env | cut -d= -f1 | grep -qx OPENAI_API_KEY || exit 9
env | cut -d= -f1 | grep -qx ANTHROPIC_API_KEY && exit 9
resolved=
resolved_marker=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--program" ]; then
    shift
    [ "$1" = "codex" ] || exit 9
    resolved=yes
  elif [ "$1" = "--program-resolved" ]; then
    resolved_marker=yes
  fi
  shift
done
[ "$resolved" = yes ] || exit 9
[ "$resolved_marker" = yes ] || exit 9
echo '{"url":"http://127.0.0.1:9","token":"test-token"}'
`)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "hook",
		"remote_hooks": map[string]any{
			"launch_cmd": launch,
			"delete_cmd": "true",
		},
		"program_overrides": map[string]any{
			tmux.ProgramClaude: tmux.ProgramCodex,
		},
	})

	result, err := (hookRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "override-auth",
		Program:  tmux.ProgramClaude,
	})
	if err != nil {
		t.Fatalf("hook launch did not receive authentication for the resolved Codex command: %v", err)
	}
	if result.Teardown != nil {
		defer func() { _ = result.Teardown() }()
	}
	backend, ok := result.Backend.(*HookBackend)
	if !ok || backend.cleanup == nil || backend.cleanup.Agent != tmux.ProgramCodex || !backend.cleanup.AgentResolved {
		t.Fatalf("hook cleanup did not persist the resolved Codex environment identity: %#v", result.Backend)
	}
}

func TestHookEnvironmentHonorsInlineClaudeCloudMode(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AZURE_CLIENT_SECRET", "fixture")
	repoRoot := initTempGitRepo(t)
	scriptDir := t.TempDir()
	launch := writeScript(t, scriptDir, "launch.sh", `
names=$(env | cut -d= -f1)
printf '%s\n' "$names" | grep -qx AWS_ACCESS_KEY_ID || exit 9
printf '%s\n' "$names" | grep -qx AWS_SECRET_ACCESS_KEY || exit 9
printf '%s\n' "$names" | grep -qx AZURE_CLIENT_SECRET && exit 9
echo '{"url":"http://127.0.0.1:9","token":"test-token"}'
`)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "hook",
		"remote_hooks": map[string]any{
			"launch_cmd": launch,
			"delete_cmd": "true",
		},
		"program_overrides": map[string]any{
			tmux.ProgramClaude: "CLAUDE_CODE_USE_BEDROCK=1 claude",
		},
	})

	result, err := (hookRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "inline-cloud-mode",
		Program:  tmux.ProgramClaude,
	})
	if err != nil {
		t.Fatalf("hook launch did not receive credentials selected by the resolved Claude command: %v", err)
	}
	if result.Teardown != nil {
		defer func() { _ = result.Teardown() }()
	}
}

func TestDockerRunForwardsAllowedNamesWithoutAmbientEnvironment(t *testing.T) {
	const (
		customName = "CUSTOM_PROVIDER_TOKEN"
		deniedName = "AF_TEST_UNRELATED_SECRET"
	)
	t.Setenv("GH_TOKEN", "test-value")
	t.Setenv(customName, "test-value")
	t.Setenv(deniedName, "test-value")

	var (
		gotEnv  []string
		gotArgs []string
	)
	restore := SetDockerExecForTest(func(_ context.Context, environ []string, args ...string) ([]byte, error) {
		gotEnv = append([]string(nil), environ...)
		gotArgs = append([]string(nil), args...)
		return []byte(dockerCreatedID), nil
	})
	defer restore()

	p := &dockerProvisioner{
		image:   "example.invalid/agent:latest",
		program: tmux.ProgramCodex,
		spec: ProvisionSpec{
			Title:                 "filtered",
			SessionEnvPassthrough: []string{customName},
		},
	}
	if err := p.runContainer(); err != nil {
		t.Fatal(err)
	}

	if !dockerHasEnvName(gotArgs, customName) {
		t.Fatalf("docker run did not forward explicit variable name %s", customName)
	}
	for _, name := range []string{"GH_TOKEN", "OPENAI_API_KEY"} {
		if dockerHasEnvName(gotArgs, name) {
			t.Fatalf("docker run forwarded built-in credential %s without explicit trust", name)
		}
	}
	if dockerHasEnvName(gotArgs, deniedName) {
		t.Fatalf("docker run forwarded disallowed variable name %s", deniedName)
	}
	if !environmentHasName(gotEnv, customName) {
		t.Fatalf("docker CLI environment omitted configured variable %s", customName)
	}
	if environmentHasName(gotEnv, "GH_TOKEN") || environmentHasName(gotEnv, "OPENAI_API_KEY") {
		t.Fatal("docker CLI environment retained a built-in credential without explicit trust")
	}
	if environmentHasName(gotEnv, deniedName) {
		t.Fatalf("docker CLI inherited disallowed variable %s", deniedName)
	}
	for _, arg := range gotArgs {
		if strings.Contains(arg, "test-value") {
			t.Fatal("docker argv rendered an environment value")
		}
	}
}

func TestSandboxAgentServersCarryPassThroughNamesIntoFilteredExec(t *testing.T) {
	const customName = "CUSTOM_PROVIDER_TOKEN"
	spec := ProvisionSpec{
		Title:                 "remote",
		Program:               tmux.ProgramCodex,
		SessionEnvPassthrough: []string{customName},
	}

	dockerCommand, err := (&dockerProvisioner{spec: spec, program: spec.Program}).agentServerCommand()
	if err != nil {
		t.Fatal(err)
	}
	sshCommand, err := (&sshProvisioner{spec: spec, program: spec.Program, sessionDir: "/srv/af-session"}).agentServerCommand()
	if err != nil {
		t.Fatal(err)
	}
	for backend, command := range map[string]string{"docker": dockerCommand, "ssh": sshCommand} {
		for _, want := range []string{"__af-session-env-exec", "agent-server", "--session-env", customName} {
			if !strings.Contains(command, want) {
				t.Fatalf("%s agent-server command omitted %q", backend, want)
			}
		}
	}
}

func TestSandboxAgentServerUsesResolvedCommandForFilteringAndLaunch(t *testing.T) {
	spec := ProvisionSpec{Title: "override", Program: tmux.ProgramClaude}
	tests := map[string]struct {
		executable    string
		inner         string
		commandResult func() (string, error)
	}{
		"docker": {
			executable: dockerAfBinaryPath,
			inner: fmt.Sprintf("%s agent-server --listen :%s --repo %s --title %s --program %s --program-resolved",
				shellQuote(dockerAfBinaryPath), dockerAgentPort, shellQuote(dockerWorkspaceDir), shellQuote(spec.Title), shellQuote(tmux.ProgramCodex)),
			commandResult: func() (string, error) {
				return (&dockerProvisioner{spec: spec, program: tmux.ProgramCodex}).agentServerCommand()
			},
		},
		"ssh": {
			executable: "/srv/af-session/af",
			inner: fmt.Sprintf("exec %s agent-server --listen 127.0.0.1:0 --repo %s --title %s --program %s --program-resolved",
				shellQuote("/srv/af-session/af"), shellQuote("/srv/af-session/workspace"), shellQuote(spec.Title), shellQuote(tmux.ProgramCodex)),
			commandResult: func() (string, error) {
				return (&sshProvisioner{spec: spec, program: tmux.ProgramCodex, sessionDir: "/srv/af-session"}).agentServerCommand()
			},
		},
	}
	for backend, test := range tests {
		command, err := test.commandResult()
		if err != nil {
			t.Fatal(err)
		}
		want, err := sessionenv.WrapCommand(test.executable, tmux.ProgramCodex, nil, test.inner)
		if err != nil {
			t.Fatal(err)
		}
		if command != want {
			t.Fatalf("%s agent-server command = %q, want the resolved Codex command inside the Codex filter %q", backend, command, want)
		}
	}
}

func TestSandboxAgentServerMarksResolvedProgram(t *testing.T) {
	spec := ProvisionSpec{Title: "override", Program: tmux.ProgramClaude}
	tests := map[string]func() (string, error){
		"docker": func() (string, error) {
			return (&dockerProvisioner{spec: spec, program: tmux.ProgramCodex}).agentServerCommand()
		},
		"ssh": func() (string, error) {
			return (&sshProvisioner{spec: spec, program: tmux.ProgramCodex, sessionDir: "/srv/af-session"}).agentServerCommand()
		},
	}
	for backend, commandResult := range tests {
		command, err := commandResult()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(command, "--program-resolved") {
			t.Fatalf("%s agent-server command can offer the resolved program for a second config lookup: %q", backend, command)
		}
	}
}

func TestSSHAgentServerCommandExecsAtRecordedPID(t *testing.T) {
	spec := ProvisionSpec{Title: "pid-identity", Program: tmux.ProgramCodex}
	p := &sshProvisioner{spec: spec, program: spec.Program, sessionDir: "/srv/af-session"}
	command, err := p.agentServerCommand()
	if err != nil {
		t.Fatal(err)
	}
	inner := fmt.Sprintf("exec %s agent-server --listen 127.0.0.1:0 --repo %s --title %s --program %s --program-resolved",
		shellQuote(p.afPath()), shellQuote(p.workspacePath()), shellQuote(spec.Title), shellQuote(spec.Program))
	want, err := sessionenv.WrapCommand(p.afPath(), tmux.ProgramCodex, nil, inner)
	if err != nil {
		t.Fatal(err)
	}
	if command != want {
		t.Fatalf("SSH agent-server launch does not exec af at the PID recorded for teardown: %q", command)
	}
}

func TestPreResolvedSandboxProgramBypassesSecondOverrideLookup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"program_overrides": map[string]any{
			tmux.ProgramCodex: "codex --model second-lookup",
		},
	})

	resolved := &Instance{
		Title:              "resolved",
		Path:               repoRoot,
		Program:            tmux.ProgramCodex,
		preResolvedProgram: tmux.ProgramCodex,
	}
	if got := resolveProgramForInstance(resolved); got != tmux.ProgramCodex {
		t.Fatalf("pre-resolved program = %q, want %q without a second override lookup", got, tmux.ProgramCodex)
	}

	ordinary := &Instance{Title: "ordinary", Path: repoRoot, Program: tmux.ProgramCodex}
	if got := resolveProgramForInstance(ordinary); got != "codex --model second-lookup" {
		t.Fatalf("ordinary program = %q, want one override lookup", got)
	}
}

func TestHookScriptRejectsAgentNameUsedAsDataAndGetsConfiguredEnvironment(t *testing.T) {
	const (
		customName = "CUSTOM_PROVIDER_TOKEN"
		deniedName = "AF_TEST_UNRELATED_SECRET"
	)
	t.Setenv(customName, "test-value")
	t.Setenv(deniedName, "test-value")
	t.Setenv("OPENAI_API_KEY", "test-value")

	script := filepath.Join(t.TempDir(), "print-env-names")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv | cut -d= -f1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, _, err := runHookScriptWithEnvironment(
		hookDeleteTimeout, script, "./collect codex", []string{customName},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Fields(string(out))
	if !slices.Contains(names, customName) {
		t.Fatalf("hook environment omitted configured variable %s", customName)
	}
	if slices.Contains(names, deniedName) {
		t.Fatalf("hook environment retained disallowed variable %s", deniedName)
	}
	if slices.Contains(names, "OPENAI_API_KEY") {
		t.Fatal("hook environment selected Codex credentials from an agent-looking data argument")
	}
}

func TestSandboxCredentialSelectionRejectsAgentNameUsedAsData(t *testing.T) {
	program := "./collect codex"
	tests := map[string]func() string{
		"docker": func() string { return (&dockerProvisioner{program: program}).agentName() },
		"ssh":    func() string { return (&sshProvisioner{program: program}).agentName() },
		"hook":   func() string { return (&hookProvisioner{program: program}).environmentAgent() },
	}
	for backend, selectedAgent := range tests {
		if got := selectedAgent(); got != "" {
			t.Fatalf("%s credential boundary selected %q from an agent-looking data argument", backend, got)
		}
	}
}

func dockerHasEnvName(args []string, name string) bool {
	for idx := 0; idx+1 < len(args); idx++ {
		if args[idx] == "-e" && args[idx+1] == name {
			return true
		}
	}
	return false
}

func environmentHasName(environ []string, name string) bool {
	prefix := name + "="
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
