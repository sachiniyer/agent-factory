package session

import (
	"context"
	"fmt"
	"os"
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

func TestDockerAccount_RefusesHostDetectedExecutableProvenance(t *testing.T) {
	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("HOME", t.TempDir())

	f := newDockerAccountFixture(t, "", "claude", nil)
	dockerCalled := false
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		dockerCalled = true
		return fakeLocalDockerResponse(args)
	}))

	_, err := createDockerAccountSession(f, "claude", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), claudePath)
	require.False(t, dockerCalled,
		"a host-qualified executable must be refused before its provenance crosses into Docker")
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
	cfg.ProgramOverrides = map[string]string{"codex": "opencode"}
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

// TestDockerAccount_RejectsProtectedMountTargetsInAnyFieldCase pins the Docker
// spelling of a mount target rather than one casing of it. Docker reads a
// --mount value as a single CSV record and lowercases each field's key before
// matching it, so every spelling below names the same container path that a
// plain `dst=` does. A case-sensitive comparison let a repo's run_args install
// `DST=/af-account/.config` over af's account boundary (#3398).
//
// Verified against Docker 29.4.0: each value here mounts (an unknown key such
// as `mydst=` is refused by Docker itself, which is why this check stays an
// exact whole-key match rather than a substring one).
func TestDockerAccount_RejectsProtectedMountTargetsInAnyFieldCase(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "lowercase dst", args: []string{"--mount", "type=bind,src=/tmp/other,dst=/af-account"}},
		{name: "lowercase destination", args: []string{"--mount", "type=bind,src=/tmp/other,destination=/af-account"}},
		{name: "lowercase target", args: []string{"--mount", "type=bind,src=/tmp/other,target=/af-account"}},
		{name: "uppercase dst", args: []string{"--mount", "type=bind,src=/tmp/other,DST=/af-account"}},
		{name: "uppercase destination", args: []string{"--mount", "type=bind,src=/tmp/other,DESTINATION=/af-account"}},
		{name: "uppercase target", args: []string{"--mount", "type=bind,src=/tmp/other,TARGET=/af-account"}},
		{name: "mixed case dst", args: []string{"--mount", "type=bind,src=/tmp/other,DsT=/af-account"}},
		{name: "mixed case destination", args: []string{"--mount", "type=bind,src=/tmp/other,DeStInAtIoN=/af-account"}},
		{name: "mixed case target", args: []string{"--mount", "type=bind,src=/tmp/other,TaRgEt=/af-account"}},
		{name: "uppercase dst on a subdirectory", args: []string{"--mount", "type=bind,src=/tmp/other,DST=/af-account/.config"}},
		{name: "mixed case target on a subdirectory", args: []string{"--mount", "type=bind,src=/tmp/other,TaRgEt=/af-account/auth.json"}},
		{name: "uppercase dst on the runtime home", args: []string{"--mount", "type=bind,src=/tmp/other,DST=/af-home"}},
		{name: "uppercase destination on a runtime subdirectory", args: []string{"--mount", "type=bind,src=/tmp/other,DESTINATION=/af-home/.config"}},
		{name: "uppercase keys throughout", args: []string{"--mount", "TYPE=bind,SRC=/tmp/other,DST=/af-account"}},
		{name: "uppercase dst in the inline form", args: []string{"--mount=type=bind,src=/tmp/other,DST=/af-account"}},
		{name: "uppercase dst before other fields", args: []string{"--mount", "DST=/af-account,type=bind,src=/tmp/other"}},
		{name: "uppercase dst with readonly", args: []string{"--mount", "type=bind,src=/tmp/other,DST=/af-account,readonly"}},
		{name: "uppercase dst on a volume mount", args: []string{"--mount", "type=volume,src=repo-vol,DST=/af-account"}},
		{name: "uppercase dst on a tmpfs mount", args: []string{"--mount", "type=tmpfs,DST=/af-account/.config"}},
		{name: "quoted lowercase dst", args: []string{"--mount", `type=bind,src=/tmp/other,"dst=/af-account"`}},
		{name: "quoted uppercase dst", args: []string{"--mount", `type=bind,src=/tmp/other,"DST=/af-account"`}},
		{name: "quoted target on a subdirectory", args: []string{"--mount", `type=bind,src=/tmp/other,"TARGET=/af-account/.config"`}},
		{name: "quoted dst in the inline form", args: []string{`--mount=type=bind,src=/tmp/other,"dst=/af-home"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, "codex")
			require.Errorf(t, err, "repo-controlled run_args mounted over the account boundary: %v", tt.args)
			require.Contains(t, err.Error(), "account mount")
		})
	}
}

// TestDockerAccount_AllowsNonProtectedMountTargetsInAnyFieldCase holds the other
// half of #3398: case-folding the FIELD NAME must not turn this check into a
// substring or case-insensitive PATH match. Container paths are case-sensitive
// on Linux, and /af-account-cache is a different directory from /af-account —
// Docker mounts each exactly where it says, so af refuses neither.
func TestDockerAccount_AllowsNonProtectedMountTargetsInAnyFieldCase(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "uppercase dst on a similarly named path", args: []string{"--mount", "type=bind,src=/tmp/cache,DST=/af-account-cache"}},
		{name: "mixed case target on a similarly named path", args: []string{"--mount", "type=bind,src=/tmp/cache,TaRgEt=/af-accountant"}},
		{name: "uppercase destination on a similarly named runtime path", args: []string{"--mount", "type=bind,src=/tmp/cache,DESTINATION=/af-homework"}},
		{name: "uppercase dst on a differently cased path", args: []string{"--mount", "type=bind,src=/tmp/cache,DST=/AF-ACCOUNT"}},
		{name: "quoted uppercase dst on a similarly named path", args: []string{"--mount", `type=bind,src=/tmp/cache,"DST=/af-account-cache"`}},
		{name: "uppercase source only", args: []string{"--mount", "type=bind,SRC=/tmp/af-account,dst=/workspace/cache"}},
		{name: "volume form on a similarly named path", args: []string{"-v", "/tmp/cache:/af-account-cache"}},
		{name: "tmpfs form on a similarly named path", args: []string{"--tmpfs", "/af-account-cache"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoErrorf(t, validateAccountDockerRunArgs(tt.args, "codex"), "harmless mount refused: %v", tt.args)
		})
	}
}

// TestDockerAccount_RejectsProtectedTargetsInColonMountForms guards the ':'
// halves of the same check against the #3398 fix. --volume and --tmpfs carry no
// field names to fold, so their targets must keep being refused exactly as
// before.
func TestDockerAccount_RejectsProtectedTargetsInColonMountForms(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "short volume", args: []string{"-v", "/tmp/other:/af-account"}},
		{name: "short volume inline", args: []string{"-v/tmp/other:/af-account/.config"}},
		{name: "long volume", args: []string{"--volume", "/tmp/other:/af-account/.config"}},
		{name: "long volume inline", args: []string{"--volume=/tmp/other:/af-home"}},
		{name: "read-only volume", args: []string{"--volume", "/tmp/other:/af-account/auth.json:ro"}},
		{name: "relabelled volume", args: []string{"-v", "/tmp/other:/af-home/.config:z"}},
		{name: "tmpfs", args: []string{"--tmpfs", "/af-account/.config"}},
		{name: "tmpfs inline with options", args: []string{"--tmpfs=/af-home:size=64m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, "codex")
			require.Errorf(t, err, "a colon-form mount reached the account boundary: %v", tt.args)
			require.Contains(t, err.Error(), "account mount")
		})
	}
}
