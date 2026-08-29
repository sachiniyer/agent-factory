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
		{name: "volumes from", args: []string{"--volumes-from", "repo-donor"}, wantErr: "--volumes-from"},
		{name: "volumes from inline", args: []string{"--volumes-from=repo-donor"}, wantErr: "--volumes-from"},
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

// TestDockerRunShorthandTables_CoverEveryDockerShorthand pins the two tables
// checkShorthandCluster reads against the short options `docker run --help`
// documents (Docker 29.4.0). A shorthand missing from both is treated as
// unknown and fails closed, which is safe but refuses arguments af could have
// read; a shorthand in the WRONG table is the unsafe direction, because it
// stops or misdirects the walk through a combined short option (#3401).
func TestDockerRunShorthandTables_CoverEveryDockerShorthand(t *testing.T) {
	documented := map[byte]bool{
		'a': false, 'c': false, 'e': false, 'h': false, 'l': false,
		'm': false, 'p': false, 'u': false, 'v': false, 'w': false,
		'd': true, 'i': true, 'P': true, 'q': true, 't': true,
	}
	for shorthand, isBoolean := range documented {
		_, boolean := dockerRunBooleanShorthands[shorthand]
		_, takesValue := dockerRunValueShorthands[shorthand]
		require.Falsef(t, boolean && takesValue, "-%c is in both shorthand tables", shorthand)
		require.Equalf(t, isBoolean, boolean, "-%c is classified as boolean=%v; `docker run --help` says boolean=%v", shorthand, boolean, isBoolean)
		require.Equalf(t, !isBoolean, takesValue, "-%c is classified as value-taking=%v; `docker run --help` says value-taking=%v", shorthand, takesValue, !isBoolean)
	}
	require.Len(t, dockerRunBooleanShorthands, 5, "a shorthand was added to the boolean table without a --help entry to back it")
	require.Len(t, dockerRunValueShorthands, 10, "a shorthand was added to the value table without a --help entry to back it")
	for _, guarded := range []byte(dockerGuardedShorthands) {
		require.Containsf(t, dockerRunValueShorthands, guarded, "guarded -%c must take a value", guarded)
	}
}

// TestDockerAccount_RejectsProtectedMountsInCombinedShortOptions covers the
// recognition half of the guard. Docker's flag parser walks a combined short
// option positionally, so the `v` behind a boolean option is the volume option
// itself — `docker run -tv /evil:/af-account` mounts exactly as `-v` does
// (measured on Docker 29.4.0), while the validator matched only an argument
// that WAS `-v` or began with it (#3401).
func TestDockerAccount_RejectsProtectedMountsInCombinedShortOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "tty then volume", args: []string{"-tv", "/tmp/other:/af-account"}},
		{name: "interactive tty then volume", args: []string{"-itv", "/tmp/other:/af-account/.config"}},
		{name: "cluster with an inline value", args: []string{"-tv/tmp/other:/af-account"}},
		{name: "cluster with an equals value", args: []string{"-tv=/tmp/other:/af-account"}},
		{name: "publish-all then volume", args: []string{"-Ptv", "/tmp/other:/af-home"}},
		{name: "quiet then volume", args: []string{"-qtv", "/tmp/other:/af-account/auth.json"}},
		{name: "every boolean then volume", args: []string{"-idPqtv", "/tmp/other:/af-account"}},
		{name: "cluster on the runtime home with a mode", args: []string{"-itv", "/tmp/other:/af-home/.config:ro"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, "codex")
			require.Errorf(t, err, "a combined short option mounted over the account boundary: %v", tt.args)
			require.Contains(t, err.Error(), "account mount")
		})
	}
}

// TestDockerAccount_RejectsIdentityEnvInCombinedShortOptions covers the
// identity half of the same gap: `-e` behind a boolean option sets an
// environment variable exactly as `-e` alone does (measured: `docker create
// -ite AF_PROBE=leaked` records AF_PROBE=leaked in .Config.Env).
func TestDockerAccount_RejectsIdentityEnvInCombinedShortOptions(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "interactive then env", args: []string{"-ie", "CODEX_API_KEY=repo-identity"}, wantErr: "CODEX_API_KEY"},
		{name: "interactive tty then env", args: []string{"-ite", "OPENAI_API_KEY=repo-identity"}, wantErr: "OPENAI_API_KEY"},
		{name: "tty then env", args: []string{"-te", "CODEX_ACCESS_TOKEN=repo-identity"}, wantErr: "CODEX_ACCESS_TOKEN"},
		{name: "cluster with an inline value", args: []string{"-teCODEX_API_KEY=repo-identity"}, wantErr: "CODEX_API_KEY"},
		{name: "cluster with an equals value", args: []string{"-te=CODEX_ACCESS_TOKEN=repo-identity"}, wantErr: "CODEX_ACCESS_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, "codex")
			require.Errorf(t, err, "a combined short option set an identity variable: %v", tt.args)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestDockerAccount_RefusesUnreadableCombinedShortOptions is the fail-closed
// rule. When a cluster holds a character af cannot classify, af cannot tell
// whether a later `v` or `e` is an option or part of that character's value —
// so it refuses and names the argument rather than guessing. A refusal is an
// annoyance with an obvious remedy; an accept is a credential-boundary breach.
func TestDockerAccount_RefusesUnreadableCombinedShortOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown option before a volume", args: []string{"-Xv", "/tmp/other:/af-account"}},
		{name: "unknown option before a harmless volume", args: []string{"-Xv", "/tmp/cache:/af-account-cache"}},
		{name: "unknown option before an env", args: []string{"-Ze", "CODEX_API_KEY=repo-identity"}},
		{name: "unknown option after a boolean", args: []string{"-itYv", "/tmp/other:/af-account"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, "codex")
			require.Errorf(t, err, "an unreadable combined short option was accepted: %v", tt.args)
			require.Contains(t, err.Error(), "combined short option")
			require.Contains(t, err.Error(), tt.args[0], "the refusal must name the argument the operator has to fix")
		})
	}
}

// TestDockerAccount_AllowsCombinedShortOptionsItCanRead is the other direction:
// failing closed must not become refusing everything. Each case here is one af
// can read to the end, so it stays allowed.
func TestDockerAccount_AllowsCombinedShortOptionsItCanRead(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "boolean cluster", args: []string{"-itd"}},
		{name: "boolean cluster with an unknown trailer", args: []string{"-itX"}},
		{name: "cluster mounting a similarly named path", args: []string{"-itv", "/tmp/cache:/af-account-cache"}},
		{name: "cluster mounting a similarly named path inline", args: []string{"-itv/tmp/cache:/af-account-cache"}},
		{name: "cluster setting a harmless variable", args: []string{"-ite", "TZ=UTC"}},
		// -u takes a value, so Docker reads the `v` as the START of that value,
		// not as --volume: `docker run -uv /tmp/evil:/af-account` fails with
		// "invalid reference format" because the path became the IMAGE name,
		// and nothing is mounted (measured on Docker 29.4.0). af reads it the
		// same way rather than refusing an argument Docker never acts on.
		{name: "value-taking option consumes the v", args: []string{"-uv", "/tmp/other:/af-account"}},
		{name: "value-taking option consumes the e", args: []string{"-ue", "CODEX_API_KEY=repo-identity"}},
		{name: "inline user value containing guarded letters", args: []string{"-udev"}},
		{name: "inline workdir value containing guarded letters", args: []string{"-w/workspace/service"}},
		{name: "inline label value containing guarded letters", args: []string{"-lenv=prod"}},
		{name: "publish with an inline value", args: []string{"-p8080:80"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoErrorf(t, validateAccountDockerRunArgs(tt.args, "codex"), "a readable combined short option was refused: %v", tt.args)
		})
	}
}

// TestDockerAccount_ClusterScanDoesNotSkipTheNextArgument is the trap the fix
// had to avoid. Docker gives `-ie` the NEXT argument as its value, so a
// validator that consumed that argument the way Docker does would step over a
// real --mount written after the cluster and never check it. The scan stays
// non-consumptive so every later argument is still examined on its own.
func TestDockerAccount_ClusterScanDoesNotSkipTheNextArgument(t *testing.T) {
	tests := [][]string{
		{"-ie", "--mount", "type=bind,src=/tmp/other,dst=/af-account"},
		{"-ie", "--mount", "type=bind,src=/tmp/other,DST=/af-account/.config"},
		{"-it", "--mount", `type=bind,src=/tmp/other,"DST=/af-home"`},
		{"-tv", "/tmp/cache:/af-account-cache", "--volume", "/tmp/other:/af-account"},
		{"-Xv", "/tmp/cache:/af-account-cache", "-v", "/tmp/other:/af-account"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.Errorf(t, validateAccountDockerRunArgs(args, "codex"),
				"a mount after a combined short option escaped validation: %v", args)
		})
	}
}

// TestDockerAccount_AllowsExplicitBooleanShortOptions covers the `-f=value`
// form, which pflag resolves BEFORE it consults an option's own kind: an
// explicit `=` makes the entire suffix that option's value, boolean included,
// so `-t=false` ends the cluster rather than continuing into `false`
// (parseSingleShortArg, pflag v1.0.6). Walking into the suffix rejected
// `docker.run_args = ["-t=false"]` — a valid configuration — because `false`
// contains an `e`, which would stop an account session from starting at all.
//
// Measured on Docker 29.4.0: every form below runs, and `-t=v/tmp:/probe`
// fails with "invalid argument ... for -t, --tty flag: strconv.ParseBool",
// proving the suffix is the boolean's value and never a nested option.
func TestDockerAccount_AllowsExplicitBooleanShortOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "tty false", args: []string{"-t=false"}},
		{name: "detach true", args: []string{"-d=true"}},
		{name: "interactive false", args: []string{"-i=false"}},
		{name: "quiet true", args: []string{"-q=true"}},
		{name: "publish-all false", args: []string{"-P=false"}},
		{name: "explicit boolean after a boolean", args: []string{"-it=false"}},
		// The suffix is -t's value even when it looks like a mount, so Docker
		// rejects the value rather than mounting anything.
		{name: "explicit boolean with a mount-shaped value", args: []string{"-t=v/tmp/other:/af-account"}},
		{name: "explicit boolean beside a harmless mount", args: []string{"-t=false", "-v", "/tmp/cache:/af-account-cache"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoErrorf(t, validateAccountDockerRunArgs(tt.args, "codex"),
				"a valid explicit boolean was refused, which would stop the session from starting: %v", tt.args)
		})
	}
}

// TestDockerAccount_ExplicitBooleanDoesNotHideALaterMount is the safety half of
// the case above: ending the cluster scan at an explicit boolean must not let
// anything after it through unchecked.
func TestDockerAccount_ExplicitBooleanDoesNotHideALaterMount(t *testing.T) {
	tests := [][]string{
		{"-t=false", "--mount", "type=bind,src=/tmp/other,dst=/af-account"},
		{"-t=false", "--mount", "type=bind,src=/tmp/other,DST=/af-account/.config"},
		{"-i=false", "-v", "/tmp/other:/af-home"},
		{"-t=false", "-tv", "/tmp/other:/af-account"},
		{"-tv=/tmp/other:/af-account"},
		{"-te=CODEX_API_KEY=repo-identity"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.Errorf(t, validateAccountDockerRunArgs(args, "codex"),
				"an explicit boolean shadowed a later guarded option: %v", args)
		})
	}
}

// TestDockerAccount_RejectsVolumesFrom covers #3403. --volumes-from names a
// CONTAINER rather than a path, so no amount of string matching on the argument
// can tell where the donor's mounts land — and they land at the donor's own
// container paths, af's account boundary included.
//
// Measured on Docker 29.4.0, with af's own `-v <account>:/af-account` mount
// present exactly as runContainer writes it:
//
//	docker create -v /legit:/af-account --volumes-from donor …
//	  where donor is `docker create -v /evil:/af-account/.config …`
//	→ .Mounts holds BOTH /af-account=/legit and /af-account/.config=/evil,
//	  and the running container reads ATTACKER-CONFIG out of
//	  /af-account/.config/settings.json while /af-account/auth.json still
//	  reads LEGIT-ACCOUNT.
//
// A donor mounting /af-home is worse: af never bind-mounts the runtime home, so
// nothing shadows it and the donor's content becomes the whole of HOME
// (measured: `ls /af-home` lists only the donor's file).
func TestDockerAccount_RejectsVolumesFrom(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "separate value", args: []string{"--volumes-from", "repo-donor"}},
		{name: "inline value", args: []string{"--volumes-from=repo-donor"}},
		{name: "read-only donor", args: []string{"--volumes-from", "repo-donor:ro"}},
		{name: "inline read-only donor", args: []string{"--volumes-from=repo-donor:ro"}},
		{name: "container id", args: []string{"--volumes-from", "6295d858ca0bc69f6c767a96ade73276cf3c837303367d48343fd21fe27ed370"}},
		{name: "after a harmless option", args: []string{"-it", "--volumes-from", "repo-donor"}},
		{name: "after a harmless mount", args: []string{"-v", "/tmp/cache:/af-account-cache", "--volumes-from", "repo-donor"}},
		{name: "second occurrence", args: []string{"--volumes-from", "harmless-donor", "--volumes-from", "repo-donor"}},
		{name: "no value at all", args: []string{"--volumes-from"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, "codex")
			require.Errorf(t, err, "--volumes-from installed the donor's mounts over the account boundary: %v", tt.args)
			require.Contains(t, err.Error(), "--volumes-from", "the refusal must name the option the operator has to remove")
		})
	}
}

// TestDockerAccount_AllowsSingleDashVolumesFrom is the precision half. The
// --env-file rule above also refuses `-env-file`, but that spelling is not the
// long option: pflag reads a single dash as a shorthand cluster, so `-env-file`
// is `-e` carrying the value `nv-file`. `-volumes-from` is likewise `-v`
// carrying `olumes-from`, which installs no donor volumes at all — measured,
// `docker create -volumes-from donor …` fails with "Unable to find image
// 'donor:latest' locally", because `donor` became the IMAGE. af reads it the
// same way, as an ordinary -v value that names no protected path.
func TestDockerAccount_AllowsSingleDashVolumesFrom(t *testing.T) {
	require.NoError(t, validateAccountDockerRunArgs([]string{"-volumes-from", "repo-donor"}, "codex"))
}

// TestDockerAccount_RefusesEveryMountInstallingOption is the anti-recurrence
// pin. This guard knows a hand-list of options, and #3398, #3401 and #3403 were
// each a member of that list going unnamed; the list below is every `docker run`
// option (Docker 29.4.0) that can put a caller-chosen filesystem at a
// caller-chosen container path. Adding a row here without a matching case in
// validateAccountDockerRunArgs fails.
//
// Audited and measured NOT to be members, so a future audit does not have to
// re-derive them:
//
//   - --device puts a device NODE at a container path, but with af's account
//     mount present `--device /dev/zero:/af-account/.config/settings.json`
//     leaves the account's own file readable and intact.
//   - --init mounts docker-init at the fixed path /sbin/docker-init.
//   - --use-api-socket mounts the Docker socket at the fixed path
//     /var/run/docker.sock. Not under the boundary — but see #3403's follow-up:
//     it hands the container the host daemon.
//   - --volume-driver, --read-only, --privileged, --cap-add, --pid, --userns
//     install no mount of their own (.Mounts unchanged from the baseline).
func TestDockerAccount_RefusesEveryMountInstallingOption(t *testing.T) {
	tests := []struct {
		option string
		args   []string
	}{
		{option: "--volume", args: []string{"--volume", "/tmp/other:/af-account"}},
		{option: "--mount", args: []string{"--mount", "type=bind,src=/tmp/other,dst=/af-account"}},
		{option: "--tmpfs", args: []string{"--tmpfs", "/af-account/.config"}},
		{option: "--volumes-from", args: []string{"--volumes-from", "repo-donor"}},
	}
	for _, tt := range tests {
		t.Run(tt.option, func(t *testing.T) {
			require.Errorf(t, validateAccountDockerRunArgs(tt.args, "codex"),
				"%s can install a filesystem at a container path and is not guarded", tt.option)
		})
	}
}
