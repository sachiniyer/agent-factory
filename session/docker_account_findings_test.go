package session

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

const accountContainerDirForTest = "/af-account"

type dockerAccountFixture struct {
	repo       string
	accountDir string
}

func newDockerAccountFixture(t *testing.T, home, agent string, runArgs []string) dockerAccountFixture {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	accountDir, err := agentaccount.Register(home, agent, "work")
	require.NoError(t, err)

	repo := initTempGitRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://example.invalid/fixture.git")
	dockerConfig := map[string]any{"image": "example.invalid/agent:latest"}
	if runArgs != nil {
		dockerConfig["run_args"] = runArgs
	}
	writeInRepoConfig(t, repo, map[string]any{"backend": "docker", "docker": dockerConfig})
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af")))
	return dockerAccountFixture{repo: repo, accountDir: accountDir}
}

func createDockerAccountSession(f dockerAccountFixture, program string, passthrough []string) (*Instance, error) {
	return NewInstance(InstanceOptions{
		Title:                 "account-boundary",
		Path:                  f.repo,
		Program:               program,
		Account:               "work",
		Backend:               BackendDocker,
		SessionEnvPassthrough: passthrough,
	})
}

func fakeLocalDockerResponse(args []string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "context" && args[1] == "inspect" {
		return []byte("unix:///var/run/docker.sock\n"), nil
	}
	if len(args) > 0 && args[0] == "info" {
		return []byte("account-test-engine\n"), nil
	}
	return nil, nil
}

func dockerEnvArg(args []string, name string) (string, bool) {
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-e" {
			continue
		}
		value := args[index+1]
		if value == name || strings.HasPrefix(value, name+"=") {
			return value, true
		}
	}
	return "", false
}

func TestDockerAccount_StripsAmbientIdentityPassthrough(t *testing.T) {
	f := newDockerAccountFixture(t, "", "codex", nil)
	t.Setenv("OPENAI_API_KEY", "ambient-identity")
	var runArgs, dockerEnv []string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, environ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
			dockerEnv = append([]string(nil), environ...)
			return nil, fmt.Errorf("stop after docker run")
		}
		return fakeLocalDockerResponse(args)
	}))

	_, _ = createDockerAccountSession(f, "codex", []string{"OPENAI_API_KEY"})
	require.NotEmpty(t, runArgs, "the account create never reached docker run")
	assignment, found := dockerEnvArg(runArgs, "OPENAI_API_KEY")
	require.True(t, found, "account containers must override an image-provided API key too")
	require.Equal(t, "OPENAI_API_KEY=", assignment,
		"the ambient API key must not compete with the mounted account")
	require.False(t, environmentHasName(dockerEnv, "OPENAI_API_KEY"),
		"the Docker CLI process must not inherit the ambient identity value")
}

func TestDockerAccount_RefusesCloudAuthenticationMode(t *testing.T) {
	f := newDockerAccountFixture(t, "", "claude", nil)
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	dockerCalled := false
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		dockerCalled = true
		return fakeLocalDockerResponse(args)
	}))

	_, err := createDockerAccountSession(f, "claude", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CLAUDE_CODE_USE_BEDROCK")
	require.False(t, dockerCalled, "cloud-mode refusal must happen before provisioning")
}

func TestDockerAccount_RejectsIdentityRunArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "short env", args: []string{"-e", "OPENAI_API_KEY=repo-identity"}, wantErr: "OPENAI_API_KEY"},
		{name: "short env equals", args: []string{"-e=CODEX_API_KEY=repo-identity"}, wantErr: "CODEX_API_KEY"},
		{name: "long env", args: []string{"--env=CODEX_ACCESS_TOKEN=repo-identity"}, wantErr: "CODEX_ACCESS_TOKEN"},
		{name: "env file", args: []string{"--env-file", "repo.env"}, wantErr: "env-file"},
		{name: "account mount", args: []string{"--mount", "type=bind,src=/tmp/other,dst=/af-account"}, wantErr: "account mount"},
		{name: "account volume", args: []string{"-v", "/tmp/other:/af-account"}, wantErr: "account mount"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDockerAccountFixture(t, "", "codex", tt.args)
			runCalled := false
			t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "run" {
					runCalled = true
				}
				return fakeLocalDockerResponse(args)
			}))

			_, err := createDockerAccountSession(f, "codex", nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "docker.run_args")
			require.Contains(t, err.Error(), tt.wantErr)
			require.False(t, runCalled, "a conflicting repo environment must refuse before docker run")
		})
	}
}

func TestDockerAccount_AllowsSimilarlyNamedMountTargets(t *testing.T) {
	err := validateAccountDockerRunArgs(
		[]string{"--mount", "type=bind,src=/tmp/cache,dst=/af-account-cache"},
		"codex",
	)
	require.NoError(t, err)
}

func TestDockerAccount_RejectsNormalizedProtectedMountTargets(t *testing.T) {
	tests := [][]string{
		{"--mount", "type=bind,src=/tmp/other,dst=/af-account/"},
		{"--mount", "type=bind,src=/tmp/other,dst=/af-account/auth.json"},
		{"--volume", "/tmp/other:/af-home/."},
	}
	for _, args := range tests {
		err := validateAccountDockerRunArgs(args, "codex")
		require.Error(t, err, "normalized protected target escaped validation: %v", args)
	}
}

func TestDockerAccount_UsesAMountFormThatAcceptsColonPaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), "af:home")
	f := newDockerAccountFixture(t, home, "codex", nil)
	var runArgs []string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
			return nil, fmt.Errorf("stop after docker run")
		}
		return fakeLocalDockerResponse(args)
	}))

	_, _ = createDockerAccountSession(f, "codex", nil)
	require.NotEmpty(t, runArgs, "the account create never reached docker run")
	wantSource := "src=" + f.accountDir
	found := false
	for index := 0; index+1 < len(runArgs); index++ {
		if runArgs[index] == "--mount" && strings.Contains(runArgs[index+1], wantSource) &&
			strings.Contains(runArgs[index+1], "dst="+accountContainerDirForTest) {
			found = true
		}
	}
	require.Truef(t, found, "colon-containing account path was not encoded as a Docker --mount: %v", runArgs)
}

func TestDockerAccount_RefusesRemoteDockerEngine(t *testing.T) {
	f := newDockerAccountFixture(t, "", "codex", nil)
	t.Setenv("DOCKER_HOST", "tcp://remote.example:2376")
	runCalled := false
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runCalled = true
		}
		return fakeLocalDockerResponse(args)
	}))

	_, err := createDockerAccountSession(f, "codex", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "remote Docker")
	require.False(t, runCalled, "a local account path must never be sent to a remote daemon")
}

func TestDockerAccount_ReprovisionCarriesThePersistedAccount(t *testing.T) {
	f := newDockerAccountFixture(t, "", "codex", nil)
	var runArgs []string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
			return nil, fmt.Errorf("stop after docker run")
		}
		return fakeLocalDockerResponse(args)
	}))
	i := &Instance{
		Title: "account-recovery", Path: f.repo, Program: "codex", Account: "work",
		Branch: "root/account-recovery", backend: newInertSandboxBackend("docker"),
	}

	_ = i.reprovisionRemote()
	require.NotEmpty(t, runArgs, "reprovision never reached docker run")
	found := false
	for _, arg := range runArgs {
		if strings.Contains(arg, f.accountDir) {
			found = true
		}
	}
	require.True(t, found, "the replacement sandbox omitted the persisted account mount")
}

func TestDockerAccount_ReprovisionRefusesResolvedAgentDrift(t *testing.T) {
	f := newDockerAccountFixture(t, "", "codex", nil)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides["codex"] = "opencode"
	require.NoError(t, config.SaveConfig(cfg))
	dockerCalled := false
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		dockerCalled = true
		return fakeLocalDockerResponse(args)
	}))
	i := &Instance{
		Title: "account-recovery", Path: f.repo, Program: "codex", Account: "work",
		Branch: "root/account-recovery", backend: newInertSandboxBackend("docker"),
	}

	err := i.reprovisionRemote()
	require.Error(t, err)
	require.Contains(t, err.Error(), "codex account")
	require.Contains(t, err.Error(), "opencode")
	require.False(t, dockerCalled, "agent drift must refuse before reaping or provisioning")
}

func TestDockerAccount_AgentServerRunsAsTheAccountOwner(t *testing.T) {
	f := newDockerAccountFixture(t, "", "codex", nil)
	var calls [][]string
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if out, err := fakeLocalDockerResponse(args); out != nil || err != nil {
			return out, err
		}
		switch args[0] {
		case "run":
			return []byte(dockerCreatedID + "\n"), nil
		case "cp", "rm":
			return nil, nil
		case "port":
			return []byte("127.0.0.1:49152\n"), nil
		case "exec":
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "stat -c %u:%g "+accountContainerDirForTest):
				return []byte("1000:1000\n"), nil
			case strings.Contains(joined, "cat "+dockerBannerPath):
				return []byte(`{"addr":":8000","token":"test-only","title":"account-boundary"}`), nil
			default:
				return nil, nil
			}
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}))

	_, err := createDockerAccountSession(f, "codex", nil)
	require.NoError(t, err)
	foundOwnedServer := false
	for _, call := range calls {
		joined := strings.Join(call, " ")
		if len(call) > 1 && call[0] == "exec" && call[1] == "-d" &&
			strings.Contains(joined, "--user 1000:1000") && strings.Contains(joined, "HOME=/af-home") {
			foundOwnedServer = true
		}
	}
	require.True(t, foundOwnedServer,
		"the detached agent-server was not run as the bind-mounted account owner; calls=%v", calls)
}
