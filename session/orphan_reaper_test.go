package session

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunContainerStampsHomeLabel: a container is tagged af.home=<homeID> so the
// orphan sweep can scope to this daemon's home, alongside the af.session slug.
func TestRunContainerStampsHomeLabel(t *testing.T) {
	var runArgs []string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
		}
		return nil, fmt.Errorf("stop after capturing docker run")
	})()

	p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "my session"}, homeID: "/home/af"}
	_ = p.runContainer()

	require.Contains(t, runArgs, "af.home=/home/af", "container must carry the af.home ownership label")
	require.Contains(t, runArgs, "af.session=my-session", "container must carry the af.session slug label")
}

// TestRunContainerOmitsHomeLabelWhenUnknown: when the home can't be resolved the
// label is omitted (the container is simply never sweep-tracked — the safe way).
func TestRunContainerOmitsHomeLabelWhenUnknown(t *testing.T) {
	var runArgs []string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runArgs = append([]string(nil), args...)
		}
		return nil, fmt.Errorf("stop after capturing docker run")
	})()

	p := &dockerProvisioner{image: "img:latest", spec: ProvisionSpec{Title: "s"}, homeID: ""}
	_ = p.runContainer()

	for _, a := range runArgs {
		require.NotContains(t, a, "af.home=", "no af.home label when the home is unknown")
	}
}

// dockerExecStub builds a fake docker CLI for sweep tests: `info` returns an
// engine id (per call, so a changed id can force an engine mismatch), `ps` returns
// the given "<id>\t<slug>" lines, `rm` records the removed id.
func dockerExecStub(t *testing.T, infoIDs []string, psOutput string, removed *[]string) func() {
	t.Helper()
	infoCall := 0
	return SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		require.NotEmpty(t, args)
		switch args[0] {
		case "info":
			id := infoIDs[min(infoCall, len(infoIDs)-1)]
			infoCall++
			return []byte(id + "\n"), nil
		case "ps":
			// Record the filter so tests can assert home-scoping.
			return []byte(psOutput), nil
		case "rm":
			*removed = append(*removed, args[len(args)-1])
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	})
}

// TestSweepReapsOrphanSparesProtected is the core contract: a listed container
// whose slug is not a live/pending session is reaped; one whose slug IS protected
// is spared. Only the orphan's id reaches `docker rm`.
func TestSweepReapsOrphanSparesProtected(t *testing.T) {
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	var removed []string
	defer dockerExecStub(t, []string{"engine-1"}, "cid-orphan\tgone\ncid-live\talive\n", &removed)()

	got := SweepOrphanContainers("/home/af", map[string]bool{"alive": true})

	assert.Equal(t, 2, got.Listed)
	assert.Equal(t, 1, got.Reaped)
	assert.Equal(t, 1, got.Skipped)
	assert.Equal(t, []string{"cid-orphan"}, removed, "only the unprotected orphan is removed")
}

// TestSweepScopesQueryToHome asserts the listing filters on BOTH af.session and
// af.home=<homeID> — the fail-safe scoping that leaves a foreign/unlabelled
// container out of the candidate set entirely.
func TestSweepScopesQueryToHome(t *testing.T) {
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	var psArgs []string
	defer SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch args[0] {
		case "info":
			return []byte("engine-1\n"), nil
		case "ps":
			psArgs = append([]string(nil), args...)
			return []byte(""), nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	})()

	SweepOrphanContainers("/home/af", nil)

	require.True(t, slices.Contains(psArgs, "label="+dockerSessionLabel), "must filter on the af.session label")
	require.True(t, slices.Contains(psArgs, "label="+dockerHomeLabel+"=/home/af"), "must scope to this home")
}

// TestSweepLeavesUnknownStateForNextSweep: when a reap can't confirm the outcome
// (here: the engine identity changed under it → ErrWorkspaceStateUnknown), the
// container is NOT counted reaped and NOT removed — it is left for a later sweep.
func TestSweepLeavesUnknownStateForNextSweep(t *testing.T) {
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	var removed []string
	// First info (sweep's own read) = engine-1; the reap's verify sees engine-2 →
	// #2382 mismatch → ErrWorkspaceStateUnknown, no rm.
	defer dockerExecStub(t, []string{"engine-1", "engine-2"}, "cid-orphan\tgone\n", &removed)()

	got := SweepOrphanContainers("/home/af", nil)

	assert.Equal(t, 1, got.Listed)
	assert.Equal(t, 0, got.Reaped)
	assert.Equal(t, 1, got.Unknown)
	assert.Empty(t, removed, "an unknown-state container must not be removed")
}

// TestSweepNoOpWithoutHomeOrDocker: an empty home disables the sweep, and a
// missing docker CLI makes it a no-op — neither touches docker.
func TestSweepNoOpWithoutHomeOrDocker(t *testing.T) {
	called := false
	restoreExec := SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
		called = true
		return nil, fmt.Errorf("docker must not be called")
	})
	defer restoreExec()

	// Empty home: disabled even if docker is present.
	restoreLook := SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })
	assert.Equal(t, OrphanSweepResult{}, SweepOrphanContainers("", map[string]bool{"x": true}))
	restoreLook()

	// No docker CLI: no-op.
	restoreLook = SetLookPathForTest(func(string) (string, error) { return "", fmt.Errorf("not found") })
	assert.Equal(t, OrphanSweepResult{}, SweepOrphanContainers("/home/af", nil))
	restoreLook()

	assert.False(t, called, "the sweep must not invoke docker in either no-op path")
}
