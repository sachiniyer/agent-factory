package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

const trustedDockerEnvironmentImage = "ghcr.io/example/af-session@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func saveDockerEnvironmentTrust(t *testing.T, images ...string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.DockerEnvTrustedImages = append([]string(nil), images...)
	require.NoError(t, config.SaveConfig(cfg))
}

func prepareDockerEnvironmentTrustTest(t *testing.T, image string, runArgs []string) string {
	t.Helper()
	repoRoot := initTempGitRepo(t)
	docker := map[string]any{"image": image}
	if runArgs != nil {
		docker["run_args"] = runArgs
	}
	writeInRepoConfig(t, repoRoot, map[string]any{"backend": "docker", "docker": docker})
	return repoRoot
}

func installDockerEnvironmentTrustTestSeams(t *testing.T) {
	t.Helper()
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af")))
}

func TestDockerEnvironmentValuesRequireHostTrustBeforeAnyDockerCall(t *testing.T) {
	const fakeAccessToken = "test-codex-access-token"
	saveDockerEnvironmentTrust(t)
	t.Setenv("CODEX_ACCESS_TOKEN", fakeAccessToken)
	repoRoot := prepareDockerEnvironmentTrustTest(t, "ghcr.io/example/af-session:latest", nil)
	installDockerEnvironmentTrustTestSeams(t)

	dockerCalled := false
	defer SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
		dockerCalled = true
		return nil, errors.New("unexpected Docker call")
	})()

	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "untrusted-environment",
		Program:  tmux.ProgramCodex,
		CloneURL: "file:///fixture.git",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "docker_env_trusted_images")
	require.Contains(t, err.Error(), "exact immutable image digest")
	require.NotContains(t, err.Error(), fakeAccessToken)
	require.False(t, dockerCalled, "an untrusted image must be rejected before Docker can start or pull it")
}

func TestDockerRejectsEmbeddedHTTPCloneCredentialsBeforeAnyDockerCall(t *testing.T) {
	for _, name := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "ftp_proxy", "all_proxy", "no_proxy",
	} {
		t.Setenv(name, "")
	}
	saveDockerEnvironmentTrust(t)
	repoRoot := prepareDockerEnvironmentTrustTest(t, "example.invalid/session:latest", nil)
	installDockerEnvironmentTrustTestSeams(t)

	dockerCalled := false
	defer SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
		dockerCalled = true
		return nil, errors.New("unexpected Docker call")
	})()

	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "embedded-origin-credential",
		Program:  tmux.ProgramAider,
		CloneURL: "https://fixture-user:fixture-secret@example.invalid/project.git",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "embedded HTTP credentials")
	require.Contains(t, err.Error(), "origin")
	require.NotContains(t, err.Error(), "fixture-user")
	require.NotContains(t, err.Error(), "fixture-secret")
	require.False(t, dockerCalled, "an embedded clone credential must be rejected before Docker sees the URL")
}

func TestDockerEnvironmentGrantForwardsNamesWithoutHostPathsOrAmbientSecrets(t *testing.T) {
	const (
		fakeAccessToken  = "test-codex-access-token"
		fakeGitHubToken  = "test-github-token"
		fakeControlToken = "test-daemon-token"
	)
	saveDockerEnvironmentTrust(t, trustedDockerEnvironmentImage)
	t.Setenv("CODEX_ACCESS_TOKEN", fakeAccessToken)
	t.Setenv("GH_TOKEN", fakeGitHubToken)
	t.Setenv("CODEX_HOME", "/host/codex")
	t.Setenv("AF_DAEMON_TOKEN", fakeControlToken)
	t.Setenv("AF_TEST_UNRELATED_SECRET", "test-unrelated-secret")
	repoRoot := prepareDockerEnvironmentTrustTest(t, trustedDockerEnvironmentImage, nil)
	installDockerEnvironmentTrustTestSeams(t)

	var gotRunEnv, gotArgs []string
	var laterEnvironments [][]string
	defer SetDockerExecForTest(func(_ context.Context, environ []string, args ...string) ([]byte, error) {
		switch args[0] {
		case "run":
			gotRunEnv = append([]string(nil), environ...)
			gotArgs = append([]string(nil), args...)
			return []byte(dockerCreatedID), nil
		case "exec":
			laterEnvironments = append(laterEnvironments, append([]string(nil), environ...))
			return nil, errors.New("stop after capturing post-run Docker client environment")
		case "rm":
			laterEnvironments = append(laterEnvironments, append([]string(nil), environ...))
			return nil, nil
		default:
			return nil, errors.New("unexpected Docker operation")
		}
	})()

	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "trusted-environment",
		Program:  tmux.ProgramCodex,
		CloneURL: "https://github.com/example/project.git",
	})
	require.Error(t, err)
	require.NotEmpty(t, laterEnvironments, "the test must inspect at least one post-run Docker client call")
	for _, name := range []string{"CODEX_ACCESS_TOKEN", "GH_TOKEN"} {
		require.Truef(t, dockerHasEnvName(gotArgs, name), "Docker run omitted approved name %s", name)
		require.Truef(t, environmentHasName(gotRunEnv, name), "Docker run client omitted approved name %s", name)
		for _, environment := range laterEnvironments {
			require.Falsef(t, environmentHasName(environment, name), "Docker client retained %s after container creation", name)
		}
	}
	for _, name := range []string{"CODEX_HOME", "AF_DAEMON_TOKEN", "AF_TEST_UNRELATED_SECRET"} {
		require.Falsef(t, dockerHasEnvName(gotArgs, name), "Docker run admitted host-only or unrelated name %s", name)
		require.Falsef(t, environmentHasName(gotRunEnv, name), "Docker run client retained host-only or unrelated name %s", name)
	}
	for _, arg := range gotArgs {
		for _, value := range []string{fakeAccessToken, fakeGitHubToken, fakeControlToken} {
			require.False(t, strings.Contains(arg, value), "Docker argv rendered an environment value")
		}
	}
}

func TestDockerEnvironmentGrantRequiresAgentAsExecutable(t *testing.T) {
	saveDockerEnvironmentTrust(t, trustedDockerEnvironmentImage)
	t.Setenv("OPENAI_API_KEY", "test-openai-token")
	repoRoot := prepareDockerEnvironmentTrustTest(t, trustedDockerEnvironmentImage, nil)
	installDockerEnvironmentTrustTestSeams(t)

	var runArgs []string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
		}
		return nil, errors.New("stop after capturing Docker arguments")
	})()

	_, _ = (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "agent-name-as-data",
		Program:  "./collect codex",
		CloneURL: "file:///fixture.git",
	})
	require.NotEmpty(t, runArgs, "test did not reach docker run")
	require.False(t, dockerHasEnvName(runArgs, "OPENAI_API_KEY"),
		"agent name used as another command's argument granted Codex credentials")
}

func TestDockerEnvironmentGrantRejectsRepoControlledRunArgs(t *testing.T) {
	saveDockerEnvironmentTrust(t, trustedDockerEnvironmentImage)
	t.Setenv("CODEX_ACCESS_TOKEN", "test-codex-access-token")
	repoRoot := prepareDockerEnvironmentTrustTest(t, trustedDockerEnvironmentImage, []string{"--memory", "2g"})
	installDockerEnvironmentTrustTestSeams(t)

	dockerCalled := false
	defer SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
		dockerCalled = true
		return nil, errors.New("unexpected Docker call")
	})()

	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "trusted-image-untrusted-arguments",
		Program:  tmux.ProgramCodex,
		CloneURL: "file:///fixture.git",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "docker.run_args")
	require.Contains(t, err.Error(), "host environment")
	require.False(t, dockerCalled, "repository-controlled Docker arguments must be rejected before Docker sees host values")
}

func TestDockerClientConnectionStateRejectsRepoControlledRunArgs(t *testing.T) {
	saveDockerEnvironmentTrust(t)
	t.Setenv("SSL_CERT_FILE", filepath.Join(t.TempDir(), "docker-ca.pem"))
	repoRoot := prepareDockerEnvironmentTrustTest(t, "ghcr.io/example/af-session:latest", []string{"--memory", "2g"})
	installDockerEnvironmentTrustTestSeams(t)

	dockerCalled := false
	defer SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
		dockerCalled = true
		return nil, errors.New("unexpected Docker call")
	})()

	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "client-ca-untrusted-arguments",
		Program:  tmux.ProgramAider,
		CloneURL: "file:///fixture.git",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "docker.run_args")
	require.Contains(t, err.Error(), "custom CA paths")
	require.False(t, dockerCalled, "repository-controlled Docker arguments must be rejected before they can request client-only CA state")
}

func TestDockerClientTransportStateRejectsRepoControlledRunArgs(t *testing.T) {
	for _, name := range []string{
		"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH",
		"DOCKER_TLS_VERIFY", "DOCKER_API_VERSION", "DOCKER_DEFAULT_PLATFORM",
		"DOCKER_CONTENT_TRUST", "DOCKER_CONTENT_TRUST_SERVER", "BUILDKIT_HOST",
		"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "ftp_proxy", "all_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
		"SSH_AGENT_PID",
	} {
		t.Setenv(name, "")
	}
	saveDockerEnvironmentTrust(t)
	const socketPath = "/run/user/1000/test-agent.sock"
	t.Setenv("SSH_AUTH_SOCK", socketPath)
	repoRoot := prepareDockerEnvironmentTrustTest(t, "ghcr.io/example/af-session:latest", []string{
		"-e", "SSH_AUTH_SOCK", "-v", socketPath + ":" + socketPath,
	})
	installDockerEnvironmentTrustTestSeams(t)

	dockerCalled := false
	defer SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
		dockerCalled = true
		return nil, errors.New("unexpected Docker call")
	})()

	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "client-transport-untrusted-arguments",
		Program:  tmux.ProgramAider,
		CloneURL: "file:///fixture.git",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "docker.run_args")
	require.Contains(t, err.Error(), "transport")
	require.False(t, dockerCalled, "repository-controlled Docker arguments must not receive the host SSH transport")
}

func TestDockerRunClearsImplicitClientProxyConfiguration(t *testing.T) {
	p := &dockerProvisioner{
		image:          "example.invalid/session:latest",
		runEnvironment: []string{"PATH=/usr/bin"},
	}
	var gotArgs []string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(dockerCreatedID), nil
	})()

	require.NoError(t, p.runContainer())
	for _, name := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "ftp_proxy", "all_proxy", "no_proxy",
	} {
		require.Truef(t, dockerHasEnvSpec(gotArgs, name+"="), "docker run did not clear implicit client proxy %s", name)
	}
}

func dockerHasEnvSpec(args []string, spec string) bool {
	for idx := 0; idx+1 < len(args); idx++ {
		if args[idx] == "-e" && args[idx+1] == spec {
			return true
		}
	}
	return false
}

func TestDockerGitHubCredentialScopeUsesFrozenEnvironmentSnapshot(t *testing.T) {
	const enterpriseHost = "github.enterprise.test"
	saveDockerEnvironmentTrust(t, trustedDockerEnvironmentImage)
	t.Setenv("GH_HOST", enterpriseHost)
	t.Setenv("GH_ENTERPRISE_TOKEN", "test-enterprise-token")
	repoRoot := prepareDockerEnvironmentTrustTest(t, trustedDockerEnvironmentImage, nil)
	installDockerEnvironmentTrustTestSeams(t)

	var cloneScript string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch args[0] {
		case "run":
			require.NoError(t, os.Setenv("GH_HOST", "other.enterprise.test"))
			return []byte(dockerCreatedID), nil
		case "exec":
			script := args[len(args)-1]
			if strings.Contains(script, "https://"+enterpriseHost+"/example/project.git") {
				cloneScript = script
				return nil, errors.New("stop after capturing clone command")
			}
			return nil, nil
		case "rm":
			return nil, nil
		case "cp":
			return nil, nil
		default:
			return nil, errors.New("unexpected Docker operation")
		}
	})()

	_, err := (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "frozen-github-host",
		Program:  tmux.ProgramAider,
		CloneURL: "https://" + enterpriseHost + "/example/project.git",
	})
	require.Error(t, err)
	require.NotEmptyf(t, cloneScript, "the test must reach the in-container clone: %v", err)
	require.Contains(t, cloneScript, "credential.https://"+enterpriseHost+".helper")
	require.Contains(t, cloneScript, "GH_ENTERPRISE_TOKEN")
	require.NotContains(t, cloneScript, "other.enterprise.test")
}
