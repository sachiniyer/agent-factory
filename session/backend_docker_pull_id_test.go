package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dockerPullCID = "3f5a1c9e7b2d4f6081a3c5e7d9b1f3a5c7e9b1d3f5a7c9e1b3d5f7a9c1e3b5d"

// realistic stderr of `docker run -d` when the image is not local, followed by
// the id on stdout (#4329).
const dockerPullOutput = "Unable to find image 'ubuntu:24.04' locally\n" +
	"24.04: Pulling from library/ubuntu\n" +
	"a1b2c3d4e5f6: Pulling fs layer\n" +
	"a1b2c3d4e5f6: Download complete\n" +
	"a1b2c3d4e5f6: Pull complete\n" +
	"Digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n" +
	"Status: Downloaded newer image for ubuntu:24.04\n" +
	dockerPullCID + "\n"

func TestRunContainerStoresOnlyTheIDWhenDockerPullsTheImage(t *testing.T) {
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		return []byte(dockerPullOutput), nil
	})()
	p := &dockerProvisioner{spec: ProvisionSpec{Title: "pull"}, image: "ubuntu:24.04"}
	require.NoError(t, p.runContainer())
	assert.Equal(t, dockerPullCID, p.containerID)
}

func TestRunContainerFailsClearlyWhenNoIDWasPrinted(t *testing.T) {
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		return []byte("Unable to find image 'x' locally\nStatus: Downloaded newer image for x\n"), nil
	})()
	p := &dockerProvisioner{spec: ProvisionSpec{Title: "noid"}, image: "x"}
	err := p.runContainer()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no container id")
	assert.Empty(t, p.containerID, "garbage must never be stored as the id")
}

func TestRunContainerErrorPathKeepsTheIDAfterPullOutput(t *testing.T) {
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		return []byte(dockerPullOutput + "docker: Error response from daemon: no such device.\n"), fmt.Errorf("exit status 125")
	})()
	p := &dockerProvisioner{spec: ProvisionSpec{Title: "errpath"}, image: "ubuntu:24.04"}
	require.Error(t, p.runContainer())
	assert.Equal(t, dockerPullCID, p.containerID)
}
