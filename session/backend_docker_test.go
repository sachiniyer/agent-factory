package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// createThenFailRunOutput is the combined output `docker run -d` produces when it
// CREATES the container (prints its 64-char id to stdout) but the subsequent start
// step fails and docker exits non-zero — e.g. a run_arg names a device that does
// not exist. CombinedOutput interleaves the stdout id and the stderr error.
const dockerCreatedID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func createThenFailRunOutput() []byte {
	return []byte(dockerCreatedID +
		"\ndocker: Error response from daemon: error gathering device information while adding custom device \"/dev/nope\": no such file or directory.\n")
}

// sawDockerRm reports whether the recorded docker invocations include a
// `rm -f <id>` — the reap call that removes a container so it does not leak.
func sawDockerRm(calls [][]string, id string) bool {
	for _, c := range calls {
		if len(c) == 3 && c[0] == "rm" && c[1] == "-f" && c[2] == id {
			return true
		}
	}
	return false
}

// TestDockerProvision_CreateThenFail_ReapsContainer is the #2008 regression:
// `docker run -d` creates the container but fails to start it, so its id is on
// stdout while the command exits non-zero. Provisioning must capture that id and
// reap the container (`docker rm -f <id>`) instead of leaving it orphaned in
// `created` state.
//
// It drives the runtime against a FAKE docker CLI (SetDockerExecForTest), so no
// real docker daemon on the box is touched.
func TestDockerProvision_CreateThenFail_ReapsContainer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// A local engine so the pre-run locality guard passes without a docker call;
	// remote engines are refused before `docker run` (see
	// TestDockerProvision_NonAccountRefusesRemoteDockerEngine).
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{"backend": "docker", "docker": map[string]any{"image": "img:latest"}})

	// Cheap hermetic preconditions so Provision reaches runContainer: docker "found"
	// on PATH and a stub `af` binary to copy in. Neither is executed under the fake.
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	defer SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af"))()

	var calls [][]string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "info":
			return []byte("engine-create-fail\n"), nil
		case "run":
			// Container CREATED (id on stdout) but the start step fails → non-zero exit.
			return createThenFailRunOutput(), fmt.Errorf("exit status 125")
		case "rm":
			return []byte(dockerCreatedID + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected docker call in this test: %v", args)
		}
	})()

	_, err := dockerRuntime{}.Provision(ProvisionSpec{RepoRoot: repoRoot, Title: "leaky", CloneURL: "file:///x"})
	require.Error(t, err, "a `docker run` that creates-then-fails must surface an error")

	require.Truef(t, sawDockerRm(calls, dockerCreatedID),
		"#2008: created-then-failed container %s was not reaped (`docker rm -f` never issued); docker calls=%v",
		dockerCreatedID, calls)
}

func TestDockerProvisionPersistsEngineIdentityForCleanup(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	const engineID = "engine-that-created-container"
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) == 0 {
			return nil, fmt.Errorf("missing docker command")
		}
		switch args[0] {
		case "info":
			return []byte(engineID + "\n"), nil
		case "run":
			return []byte(dockerCreatedID + "\n"), nil
		case "cp":
			return nil, nil
		case "port":
			return []byte("127.0.0.1:49152\n"), nil
		case "exec":
			if len(args) >= 4 && args[2] == "cat" && args[3] == dockerBannerPath {
				return []byte(`{"addr":":8000","token":"test-only","title":"engine-bound"}`), nil
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected docker command: %v", args)
		}
	})()

	p := &dockerProvisioner{
		spec:  ProvisionSpec{Title: "engine-bound", CloneURL: "file:///repo"},
		image: "image:latest",
		afBin: "/tmp/test-af",
	}
	result, err := p.provision()
	require.NoError(t, err)
	backend, ok := result.Backend.(*dockerBackend)
	require.Truef(t, ok, "provisioned backend type = %T, want *dockerBackend", result.Backend)
	require.NotNil(t, backend.cleanup)
	assert.Equal(t, dockerCreatedID, backend.cleanup.ContainerID)
	assert.Equal(t, engineID, backend.cleanup.EngineID,
		"cleanup handle must bind the container to the Docker engine that created it")
}

// TestRunContainer_CapturesCreatedIDOnError pins the fix at its source: even when
// `docker run` returns an error, runContainer must extract the created container's
// id from the (combined) output and store it, so the provision-failure reap —
// guarded on p.containerID != "" — can remove it. Before the fix p.containerID
// stayed empty on the error path.
func TestRunContainer_CapturesCreatedIDOnError(t *testing.T) {
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		return createThenFailRunOutput(), fmt.Errorf("exit status 125")
	})()

	p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "leaky"}}
	err := p.runContainer()

	require.Error(t, err)
	assert.Equal(t, dockerCreatedID, p.containerID,
		"created-then-failed container id must be captured so the failed provision can reap it")
}

// TestParseCreatedContainerID covers the extraction: a create-then-fail blob
// yields the id regardless of line order, a clean success line yields it, and
// output with no container id (docker failed before creating anything) yields ""
// so nothing bogus is reaped.
func TestParseCreatedContainerID(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"created then fail (id then error)", dockerCreatedID + "\ndocker: Error response from daemon: bad --device\n", dockerCreatedID},
		{"created then fail (error then id)", "docker: Error response from daemon: bad --device\n" + dockerCreatedID + "\n", dockerCreatedID},
		{"clean success", dockerCreatedID + "\n", dockerCreatedID},
		{"no id — failed before create", "docker: Error response from daemon: invalid reference format\n", ""},
		{"empty", "", ""},
		{"image digest line is not a bare id", "sha256:" + dockerCreatedID + "\ndocker: pull error\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseCreatedContainerID([]byte(tc.out)))
		})
	}
}

// TestDockerProvision_NonAccountRefusesRemoteDockerEngine is the non-account half of
// the remote-engine refusal. Account sessions already refused a remote engine
// (TestDockerAccount_RefusesRemoteDockerEngine, via ensureAccountDockerEngineLocal,
// for the bind-mount-identity reason); a non-account session against a remote
// engine used to provision an unreachable http://127.0.0.1:<port> endpoint and
// fail later with an opaque `connection refused`. Provision must now refuse the
// remote engine BEFORE `docker run`, naming the remote-engine cause, exactly as the
// account path does for its own reason.
func TestDockerProvision_NonAccountRefusesRemoteDockerEngine(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "tcp://remote.example.invalid:2376")
	t.Setenv("DOCKER_CONTEXT", "")
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{"backend": "docker", "docker": map[string]any{"image": "img:latest"}})
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	defer SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af"))()

	runCalled := false
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runCalled = true
		}
		// Any docker call reaching the fake means the locality guard did not run
		// first, so the returned error (rather than the remote-engine refusal)
		// surfaces and fails the assertions below — pinning the guard's ordering.
		return nil, fmt.Errorf("unexpected docker call in remote-engine test: %v", args)
	})()

	_, err := dockerRuntime{}.Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "remote-engine",
		CloneURL: "file:///x",
	})
	require.Error(t, err, "a non-account session against a remote engine must be refused at create time")
	require.Falsef(t, runCalled, "a remote engine must be refused before `docker run`, not after a container exists")
	require.Contains(t, err.Error(), "remote", "the refusal must name the remote-engine cause, not an unrelated step")
	require.Contains(t, err.Error(), "tcp://remote.example.invalid:2376", "the refusal must name the offending endpoint")
}

// TestBackendUnusableReason_DockerRefusesRemoteEngine is the choose-time half of
// the remote-engine refusal. The runtime refuses a remote engine at create time
// (TestDockerProvision_NonAccountRefusesRemoteDockerEngine); the picker must not
// offer docker there either, or a client would select it and discover the failure
// only at create time — the exact "offered, then fails later somewhere less
// obvious" trap the picker exists to close. With DOCKER_HOST set, locality is read
// from it directly (no `docker` call), so the answer is hermetic. Both surfaces
// share resolveDockerEngineEndpoint, so choose-time and create-time cannot
// disagree.
func TestBackendUnusableReason_DockerRefusesRemoteEngine(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://remote.example:2376")
	t.Setenv("DOCKER_CONTEXT", "")
	repo := repoWithOriginForTest(t)
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	cfg := &config.ResolvedConfig{Docker: &config.DockerConfig{Image: "my-runtime:latest"}}

	err := BackendUnusableReason(BackendDocker, cfg, repo)
	require.Error(t, err, "a remote engine must not be offered as usable at choose time")
	assert.Contains(t, err.Error(), "remote")
	assert.Contains(t, err.Error(), "tcp://remote.example:2376")

	// A local engine is selectable again, so the refusal is the only thing this
	// test moved.
	t.Run("local engine is selectable again", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
		t.Setenv("DOCKER_CONTEXT", "")
		assert.NoError(t, BackendUnusableReason(BackendDocker, cfg, repo))
	})
}

// TestBackendUnusableReason_DockerFailsClosedOnProbeError pins the fail-closed
// posture: when locality cannot be established (DOCKER_HOST unset and `docker
// context inspect` fails), the picker must report docker unavailable — not usable
// — matching the runtime's own refusal (ensureDockerEngineLocal fails closed).
// "I could not prove the engine is local" is not "available".
func TestBackendUnusableReason_DockerFailsClosedOnProbeError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	repo := repoWithOriginForTest(t)
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "context" && args[1] == "inspect" {
			return []byte("no context"), fmt.Errorf("context store corrupted")
		}
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}))
	cfg := &config.ResolvedConfig{Docker: &config.DockerConfig{Image: "my-runtime:latest"}}

	err := BackendUnusableReason(BackendDocker, cfg, repo)
	require.Error(t, err, "an unprovable locality must not be offered as usable")
	assert.Contains(t, err.Error(), "local", "the reason must name the locality check that failed")
}
