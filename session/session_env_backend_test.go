package session

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

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

	for _, name := range []string{"GH_TOKEN", "OPENAI_API_KEY", customName} {
		if name == "OPENAI_API_KEY" && os.Getenv(name) == "" {
			continue
		}
		if !dockerHasEnvName(gotArgs, name) {
			t.Fatalf("docker run did not forward allowed variable name %s", name)
		}
	}
	if dockerHasEnvName(gotArgs, deniedName) {
		t.Fatalf("docker run forwarded disallowed variable name %s", deniedName)
	}
	if !environmentHasName(gotEnv, customName) {
		t.Fatalf("docker CLI environment omitted configured variable %s", customName)
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

func TestHookScriptGetsFilteredEnvironment(t *testing.T) {
	const (
		customName = "CUSTOM_PROVIDER_TOKEN"
		deniedName = "AF_TEST_UNRELATED_SECRET"
	)
	t.Setenv(customName, "test-value")
	t.Setenv(deniedName, "test-value")

	script := filepath.Join(t.TempDir(), "print-env-names")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv | cut -d= -f1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out, _, err := runHookScriptWithEnvironment(
		hookDeleteTimeout, script, tmux.ProgramCodex, []string{customName},
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
