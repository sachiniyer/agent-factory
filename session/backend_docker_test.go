package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
