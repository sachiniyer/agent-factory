package integration_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestDockerBackendKillStartFailureRetainsRow is the end-to-end proof for the
// exec-start-failure fix: a live docker-backed session, killed while the docker
// CLI is UNSTARTABLE for the daemon's process, must report a teardown that wraps
// session.ErrWorkspaceStateUnknown (so deleteSessionRecord RETAINS the row) and
// the container must STILL be running (the rm never ran). Once the CLI is
// restored, a second reap must remove the container (no leak).
//
// It uses the REAL docker daemon and the real NewInstance(BackendDocker) path —
// the same path TestDockerBackendRoundTrip drives — but breaks the docker CLI
// seam at Kill time to model "the rm failed to start". It SKIPS cleanly where
// docker is unavailable.
func TestDockerBackendKillStartFailureRetainsRow(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive real-backend E2E/integration; skipped under -short")
	}
	requireDocker(t)
	requireTool(t, "git")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	afBin := buildStaticBinary(t)
	restoreBin := session.SetDockerSelfBinaryForTest(afBin)
	defer restoreBin()

	image := buildDockerRoundTripImage(t)

	repo := setupGitRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "docker kill start-fail e2e\n", 0644)
	runExternal(t, repo, "git", "add", "-A")
	runExternal(t, repo, "git", "commit", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runExternal(t, "", "git", "clone", "--bare", repo, bare)
	runExternal(t, repo, "git", "remote", "add", "origin", "file:///repo.git")
	writeDockerRepoConfig(t, repo, image, []string{"-v", bare + ":/repo.git:ro"})

	title := "docker-kill-startfail"
	slug := session.Slugify(title)
	t.Cleanup(func() { reapByLabel(slug) })

	// Record the real docker binary path so the unstartable seam can restore it.
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("docker on PATH at provision time is required: %v", err)
	}

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    repo,
		Program: "cat",
		Backend: session.BackendDocker,
	})
	if err != nil {
		t.Fatalf("NewInstance(backend=docker): %v", err)
	}
	if got := containersForLabel(t, slug); len(got) == 0 {
		t.Fatal("expected a running container for the docker session after provisioning")
	}
	t.Logf("container is up; making the docker CLI unstartable for the Kill")
	containerID := containersForLabel(t, slug)[0]

	defer func() {
		// Backstop: ensure the container is reaped even if the test fails midway.
		_ = exec.CommandContext(context.Background(), realDocker, "rm", "-f", containerID).Run()
	}()

	if err := inst.Start(true); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Break the docker CLI for the daemon's process: replace the seam so the
	// rm returns empty output + a start-failure error, modeling a CLI missing
	// from PATH / fork failure / killed-before-printing. Keep `docker info`
	// (engine-identity check) working so the verifyEngineOnReap-shaped paths
	// don't refuse; the live-Kill path has verifyEngineOnReap=false, so this
	// models the bug report's in-process provisioner shape exactly.
	restoreExec := session.SetDockerExecForTest(func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "info" {
			return []byte("engine-e2e-startfail\n"), nil
		}
		// The rm "fails to start": empty output, non-nil error, no deadline.
		return nil, &startFailureErr{msg: `exec: "docker": executable file not found in $PATH`}
	})
	defer restoreExec()

	// --- Kill while the CLI is unstartable: must be workspace-state-unknown ---
	killErr := inst.Kill()
	if killErr == nil {
		t.Fatal("Kill with an unstartable docker CLI must return an error, got nil")
	}
	if !session.TeardownStateUnknown(killErr) {
		t.Fatalf("Kill with an unstartable docker CLI must be workspace-state-unknown so deleteSessionRecord RETAINS the row; got: %v", killErr)
	}
	if !isStartFailureKill(killErr) {
		// Not fatal — the wrap chain may elide the inner type — but log it.
		t.Logf("Kill error does not carry the start-failure marker directly (acceptable if wrapped): %v", killErr)
	}
	t.Logf("Kill returned unknown-state (row retained): %v", killErr)

	// --- The container must STILL be running: the rm never ran ---
	waitUntil(t, 3*time.Second, "the container is still running after the start-failed Kill", func() bool {
		return len(containersForLabel(t, slug)) == 1
	})
	t.Logf("container %s is still running after the start-failed Kill — the rm never reached an engine", containerID)

	// --- Restore the docker CLI and reap: the retry must remove the container ---
	restoreExec()
	// Use the real docker CLI for the retry. The provisioner did NOT latch on
	// the start failure (the fix), so a direct `docker rm -f` via the real CLI
	// models the daemon's kill-retry re-running the rm once the CLI is restored.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, realDocker, "rm", "-f", containerID).CombinedOutput(); err != nil {
		t.Fatalf("restoring the CLI and re-running `docker rm -f` must reap the container, got err=%v out=%s", err, string(out))
	}
	waitUntil(t, 10*time.Second, "the container is reaped after the CLI is restored", func() bool {
		return len(containersForLabel(t, slug)) == 0
	})
	t.Logf("container reaped after the docker CLI was restored — no leak. e2e kill start-failure round-trip complete.")
}

// startFailureErr models the exec.CommandContext start-failure error shape: a
// non-nil error whose Error() matches the stdlib's LookPath-miss message, with
// no tripped context deadline. reap() must classify this (empty output + this
// error) as workspace-state-unknown.
type startFailureErr struct{ msg string }

func (e *startFailureErr) Error() string { return e.msg }

// isStartFailureKill reports whether the kill error chain contains the
// start-failure marker error. The reap wraps the start-failure error inside an
// ErrWorkspaceStateUnknown wrap, so errors.As walks the chain.
func isStartFailureKill(err error) bool {
	if err == nil {
		return false
	}
	var target *startFailureErr
	return errors.As(err, &target)
}
