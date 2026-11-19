package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
)

// #7447: a Docker provision in a throwaway sandbox home records the engine
// about to host its container before the container exists, so a later reap
// can tell an empty listing on its own engine from "nothing left anywhere".

// engineRecordDocker is a fake docker CLI that provisions a container on
// engineID. atRun runs when `docker run` arrives, before it "creates" the
// container.
func engineRecordDocker(t *testing.T, engineID string, atRun func()) func() {
	t.Helper()
	return SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) == 0 {
			return nil, fmt.Errorf("missing docker command")
		}
		switch args[0] {
		case "info":
			return []byte(engineID + "\n"), nil
		case "run":
			if atRun != nil {
				atRun()
			}
			return []byte(dockerCreatedID + "\n"), nil
		case "cp":
			return nil, nil
		case "port":
			return []byte("127.0.0.1:49152\n"), nil
		case "exec":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "sh -c") && strings.Contains(joined, "id -u") {
				return []byte("0:0\n"), nil
			}
			if strings.Contains(joined, "/proc/self/mountinfo") {
				// The cp fallback's write-target guard reads the table
				// (#7577): resolved parents, then a rootfs-only mountinfo
				// so the write proceeds.
				return probeAnswer(args), nil
			}
			if strings.Contains(joined, "cat "+dockerBannerPath) {
				return []byte(`{"addr":":8000","token":"test-only","title":"engine-record"}` + "\n" + remoteProcAlive + "\n"), nil
			}
			return nil, nil
		default:
			return dockerUnexpectedCall(args)
		}
	})
}

// markedSandboxHome returns a home carrying a valid throwaway marker.
func markedSandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, throwawaysandbox.WriteMarker(home, &throwawaysandbox.Marker{
		Version: 1, Kind: "test-7447", CreatedUnix: time.Now().Unix(), MaxLifetimeSeconds: 3600,
	}))
	return home
}

func engineRecordProvisioner(t *testing.T, home string) *dockerProvisioner {
	return &dockerProvisioner{
		spec:   ProvisionSpec{Title: "engine-record", CloneURL: "file:///repo"},
		image:  "image:latest",
		afBin:  stagingBinarySource(t),
		homeID: home,
	}
}

// TestDockerProvisionRecordsTheEngineBeforeTheContainer: in a marked home,
// the engine is already recorded when `docker run` arrives.
func TestDockerProvisionRecordsTheEngineBeforeTheContainer(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home := markedSandboxHome(t)
	const engineID = "engine-7447-a"
	var recordedAtRun []string
	var readErr error
	defer engineRecordDocker(t, engineID, func() {
		recordedAtRun, readErr = throwawaysandbox.ReadRecordedEngines(home)
	})()

	_, err := engineRecordProvisioner(t, home).provision()
	require.NoError(t, err)

	require.NoError(t, readErr, "the engine must be recorded before docker run creates the container")
	assert.Equal(t, []string{engineID}, recordedAtRun)
}

// TestDockerProvisionRefusesWhenTheEngineCannotBeRecorded: a container whose
// engine nobody recorded is the leak the record closes, so a failed record
// refuses the provision before `docker run`.
func TestDockerProvisionRefusesWhenTheEngineCannotBeRecorded(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	previous := recordThrowawayEngine
	recordThrowawayEngine = func(string, string) error { return errors.New("disk full") }
	t.Cleanup(func() { recordThrowawayEngine = previous })
	ran := false
	defer engineRecordDocker(t, "engine-7447-a", func() { ran = true })()

	_, err := engineRecordProvisioner(t, markedSandboxHome(t)).provision()

	require.ErrorIs(t, err, errThrowawayEngineUnrecorded)
	assert.False(t, ran, "no container may be created when its engine cannot be recorded")
}

// TestDockerProvisionRecordsNothingOutsideAMarkedHome: an ordinary home gets
// no engine record; the record exists only for throwaway sandboxes.
func TestDockerProvisionRecordsNothingOutsideAMarkedHome(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	home := t.TempDir()
	defer engineRecordDocker(t, "engine-7447-a", nil)()

	_, err := engineRecordProvisioner(t, home).provision()
	require.NoError(t, err)

	_, statErr := os.Stat(throwawaysandbox.RecordedEnginesPath(home))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "an unmarked home must get no engine record")
}
