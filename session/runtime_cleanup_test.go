package session

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"

	"golang.org/x/crypto/ssh"
)

// TestSSHCleanupHandleSurvivesTombstoneRoundTrip covers the restart half of
// #2198's cleanup retry contract. A kill tombstone can survive only if the exact
// off-box teardown handle survives with it; an inert no-op backend would delete
// the retained row while leaving its remote process and directory behind.
func TestSSHCleanupHandleSurvivesTombstoneRoundTrip(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "restart-reap"},
		cfg:        config.SSHConfig{Host: "cleanup.example.test", User: "remote"},
		sessionDir: "/home/remote/.af-sessions/restart-reap.1234",
		remotePID:  "4242",
	}
	teardown := p.reap
	inst := &Instance{
		ID:    "restart-reap-id",
		Title: "restart-reap",
		Path:  "/repo",
		backend: &sshBackend{
			remoteAgentBackend: remoteAgentBackend{reap: teardown},
			provisioner:        p,
			cleanup: &SSHRuntimeCleanupData{
				Config:     p.cfg,
				SessionDir: p.sessionDir,
				RemotePID:  p.remotePID,
			},
		},
		runtimeTeardown: teardown,
	}

	// persistKillTombstone snapshots first and flips UserKilled only on its copy.
	// The private staging handle must survive that exact ordering without leaking
	// into the ordinary daemon snapshot.
	precommit := inst.ToInstanceData()
	snapshotRaw, err := json.Marshal(precommit)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, private := range []string{p.cfg.Host, p.sessionDir, p.remotePID, "runtime_cleanup"} {
		if strings.Contains(string(snapshotRaw), private) {
			t.Fatalf("daemon snapshot leaked storage-only cleanup value %q: %s", private, snapshotRaw)
		}
	}

	precommit.UserKilled = true
	raw, err := json.Marshal(precommit.ForStorage())
	if err != nil {
		t.Fatalf("marshal tombstone: %v", err)
	}
	if !strings.Contains(string(raw), "runtime_cleanup") {
		t.Fatalf("kill tombstone omitted its durable cleanup handle: %s", raw)
	}
	// The live attempt sends the identity-guarded kill, then loses certainty while
	// removing the directory. The raw tombstone above is what a daemon restart
	// reloads in that window; it still carries the original PID by design, and the
	// remote argv guard makes retrying that stale numeric value safe.
	inst.MarkUserKilled()
	p.client = &ssh.Client{}
	p.reapRunKill = func(time.Duration, string) (bool, error) { return true, nil }
	p.reapRunCombined = func(time.Duration, string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	p.reapCloseClient = func() {}
	if err := inst.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("pre-restart rm timeout did not retain tombstone: %v", err)
	}
	var stored InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal tombstone: %v", err)
	}
	stored = stored.ForStorage() // Storage.LoadInstances normalizes here before FromInstanceData.
	if stored.RuntimeCleanup == nil {
		t.Fatal("storage normalization dropped the loaded cleanup handle")
	}
	restored, err := FromInstanceData(stored)
	if err != nil {
		t.Fatalf("restore tombstone: %v", err)
	}
	if restored.runtimeTeardown == nil {
		t.Fatal("SSH cleanup handle disappeared across the tombstone round-trip")
	}
	restoredBackend, ok := restored.backend.(*sshBackend)
	if !ok || restoredBackend.provisioner == nil {
		t.Fatalf("restored backend has no SSH reaper: %T", restored.backend)
	}
	restoredP := restoredBackend.provisioner
	restoredP.client = &ssh.Client{}
	var killCalls, rmCalls int
	restoredP.reapRunKill = func(_ time.Duration, script string) (bool, error) {
		killCalls++
		if !strings.Contains(script, restoredP.afPath()) || !strings.Contains(script, stored.RuntimeCleanup.SSH.RemotePID) {
			t.Fatalf("restored kill lost its process identity guard: %q", script)
		}
		return true, nil
	}
	restoredP.reapRunCombined = func(_ time.Duration, script string) ([]byte, error) {
		rmCalls++
		if !strings.HasPrefix(script, "rm -rf ") || !strings.Contains(script, stored.RuntimeCleanup.SSH.SessionDir) {
			t.Fatalf("restored reap targeted the wrong directory: %q", script)
		}
		return nil, nil
	}
	restoredP.reapCloseClient = func() {}
	if err := restored.Kill(); err != nil {
		t.Fatalf("restored tombstone could not execute its SSH cleanup: %v", err)
	}
	if killCalls != 1 || rmCalls != 1 {
		t.Fatalf("restored cleanup work: kill=%d rm=%d, want 1/1", killCalls, rmCalls)
	}
}

func TestRuntimeCleanupHandleRoundTripsEveryOffBoxBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend Backend
	}{
		{
			name: "docker",
			backend: &dockerBackend{
				provisioner: &dockerProvisioner{containerID: "sha256:cleanup-container"},
				cleanup: &DockerRuntimeCleanupData{
					ContainerID: "sha256:cleanup-container",
					EngineID:    "engine-cleanup",
				},
			},
		},
		{
			name: "ssh",
			backend: &sshBackend{
				provisioner: &sshProvisioner{
					cfg:        config.SSHConfig{Host: "builder.internal", User: "ci"},
					sessionDir: "/srv/af/session.123",
					remotePID:  "991",
				},
				cleanup: &SSHRuntimeCleanupData{
					Config:     config.SSHConfig{Host: "builder.internal", User: "ci"},
					SessionDir: "/srv/af/session.123",
					RemotePID:  "991",
				},
			},
		},
		{
			name: "remote",
			backend: &HookBackend{
				provisioner: &hookProvisioner{
					hooks:         config.RemoteHooks{DeleteCmd: "/opt/hooks/delete"},
					slug:          "cleanup-slug",
					launchStarted: true,
				},
				cleanup: &HookRuntimeCleanupData{DeleteCmd: "/opt/hooks/delete", Slug: "cleanup-slug"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{ID: tc.name + "-id", Title: tc.name + "-cleanup", Path: "/repo", backend: tc.backend}
			if liveStored := inst.ToInstanceData().ForStorage(); liveStored.RuntimeCleanup != nil {
				t.Fatalf("ordinary live record persisted cleanup handle: %#v", liveStored.RuntimeCleanup)
			}
			inst.userKilled = true
			stored := inst.ToInstanceData().ForStorage()
			if stored.RuntimeCleanup == nil {
				t.Fatal("tombstone omitted cleanup handle")
			}
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded InstanceData
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			decoded = decoded.ForStorage()
			if !reflect.DeepEqual(decoded.RuntimeCleanup, stored.RuntimeCleanup) {
				t.Fatalf("storage normalization changed cleanup handle:\n got %#v\nwant %#v", decoded.RuntimeCleanup, stored.RuntimeCleanup)
			}
			restored, err := FromInstanceData(decoded)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if restored.runtimeTeardown == nil {
				t.Fatal("restored tombstone has no teardown")
			}
			roundTrip := restored.ToInstanceData().ForStorage().RuntimeCleanup
			if !reflect.DeepEqual(roundTrip, stored.RuntimeCleanup) {
				t.Fatalf("cleanup handle changed across restart:\n got %#v\nwant %#v", roundTrip, stored.RuntimeCleanup)
			}
		})
	}
}

func restoreDockerTombstoneForTest(t *testing.T, engineID string) *Instance {
	t.Helper()
	restored, err := FromInstanceData(InstanceData{
		ID:          "docker-tombstone-id",
		Title:       "docker-tombstone",
		Path:        "/repo",
		BackendType: "docker",
		UserKilled:  true,
		RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
			ContainerID: "sha256:cleanup-container",
			EngineID:    engineID,
		}},
	})
	if err != nil {
		t.Fatalf("restore docker tombstone: %v", err)
	}
	return restored
}

func TestRestoredDockerCleanupRefusesDifferentEngine(t *testing.T) {
	restored := restoreDockerTombstoneForTest(t, "engine-a")
	var calls [][]string
	restoreExec := SetDockerExecForTest(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 0 && args[0] == "info" {
			return []byte("engine-b\n"), nil
		}
		return []byte("sha256:cleanup-container\n"), nil
	})
	defer restoreExec()

	err := restored.Kill()
	if !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("cleanup on a different Docker engine must retain the tombstone as unknown, got %v", err)
	}
	for _, call := range calls {
		if len(call) > 0 && call[0] == "rm" {
			t.Fatalf("cleanup targeted a container before proving the Docker engine identity: calls=%v", calls)
		}
	}
}

func TestRestoredDockerCleanupReapsMatchingEngine(t *testing.T) {
	restored := restoreDockerTombstoneForTest(t, "engine-a")
	var calls [][]string
	restoreExec := SetDockerExecForTest(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 0 && args[0] == "info" {
			return []byte("engine-a\n"), nil
		}
		return []byte("sha256:cleanup-container\n"), nil
	})
	defer restoreExec()

	if err := restored.Kill(); err != nil {
		t.Fatalf("cleanup on the recorded Docker engine failed: %v", err)
	}
	if len(calls) != 2 || len(calls[0]) == 0 || calls[0][0] != "info" || len(calls[1]) == 0 || calls[1][0] != "rm" {
		t.Fatalf("restored cleanup calls=%v, want identity probe followed by rm", calls)
	}
}

func TestRestoredDockerCleanupRetriesIdentityProbe(t *testing.T) {
	restored := restoreDockerTombstoneForTest(t, "engine-a")
	var infoCalls, rmCalls int
	restoreExec := SetDockerExecForTest(func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) == 0 {
			return nil, errors.New("missing docker command")
		}
		switch args[0] {
		case "info":
			infoCalls++
			if infoCalls == 1 {
				return nil, errors.New("docker daemon temporarily unavailable")
			}
			return []byte("engine-a\n"), nil
		case "rm":
			rmCalls++
			return []byte("sha256:cleanup-container\n"), nil
		default:
			return nil, errors.New("unexpected docker command")
		}
	})
	defer restoreExec()

	if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("an unverifiable Docker target must retain the tombstone, got %v", err)
	}
	if rmCalls != 0 {
		t.Fatalf("identity-probe failure still issued docker rm %d time(s)", rmCalls)
	}
	if err := restored.Kill(); err != nil {
		t.Fatalf("cleanup did not retry after Docker identity became verifiable: %v", err)
	}
	if infoCalls != 2 || rmCalls != 1 {
		t.Fatalf("retry work: info=%d rm=%d, want 2/1", infoCalls, rmCalls)
	}
}

func TestLegacyDockerCleanupWithoutEngineIdentityFailsClosed(t *testing.T) {
	restored := restoreDockerTombstoneForTest(t, "")
	var calls int
	restoreExec := SetDockerExecForTest(func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		return nil, nil
	})
	defer restoreExec()

	if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("legacy Docker tombstone without engine identity must remain retryable, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("legacy Docker tombstone guessed a target and invoked Docker %d time(s)", calls)
	}
}

func TestArchivedKillTombstoneDoesNotPersistRuntimeCleanup(t *testing.T) {
	const secretHost = "archived-cleanup.example.test"
	cleanup := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:     config.SSHConfig{Host: secretHost, User: "remote"},
		SessionDir: "/srv/af/already-archived",
		RemotePID:  "4242",
	}}
	stored := (InstanceData{
		ID:             "archived-id",
		Title:          "archived",
		BackendType:    "ssh",
		Liveness:       LiveArchived,
		UserKilled:     true,
		RuntimeCleanup: cleanup,
		runtimeCleanup: cleanup,
	}).ForStorage()
	if stored.RuntimeCleanup != nil {
		t.Fatalf("archived tombstone retained unused cleanup identity: %#v", stored.RuntimeCleanup)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal archived tombstone: %v", err)
	}
	if strings.Contains(string(raw), secretHost) {
		t.Fatalf("archived tombstone leaked cleanup identity: %s", raw)
	}
}

func TestMissingRemoteCleanupHandleFailsClosed(t *testing.T) {
	restored, err := FromInstanceData(InstanceData{
		ID:          "legacy-tombstone-id",
		Title:       "legacy-tombstone",
		Path:        "/repo",
		BackendType: "ssh",
		UserKilled:  true,
	})
	if err != nil {
		t.Fatalf("restore legacy tombstone: %v", err)
	}
	if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("missing cleanup handle was laundered into success: %v", err)
	}
}

func TestMalformedRemoteCleanupHandleFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		cleanup *RuntimeCleanupData
	}{
		{
			name:    "multiple variants",
			backend: "ssh",
			cleanup: &RuntimeCleanupData{
				SSH:    &SSHRuntimeCleanupData{Config: config.SSHConfig{Host: "host"}, SessionDir: "/session", RemotePID: "42"},
				Docker: &DockerRuntimeCleanupData{ContainerID: "container"},
			},
		},
		{
			name:    "wrong variant",
			backend: "docker",
			cleanup: &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
				Config: config.SSHConfig{Host: "host"}, SessionDir: "/session", RemotePID: "42",
			}},
		},
		{
			name:    "invalid ssh pid",
			backend: "ssh",
			cleanup: &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
				Config: config.SSHConfig{Host: "host"}, SessionDir: "/session", RemotePID: "not-a-pid",
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restored, err := FromInstanceData(InstanceData{
				ID: "malformed-tombstone-id", Title: "malformed-tombstone", Path: "/repo",
				BackendType: tc.backend, UserKilled: true, RuntimeCleanup: tc.cleanup,
			})
			if err != nil {
				t.Fatalf("restore malformed tombstone: %v", err)
			}
			if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
				t.Fatalf("malformed cleanup handle was laundered into success: %v", err)
			}
		})
	}
}

func TestArchivedRemoteTombstoneNeedsNoCleanupHandle(t *testing.T) {
	restored, err := FromInstanceData(InstanceData{
		ID:          "archived-tombstone-id",
		Title:       "archived-tombstone",
		Path:        "/repo",
		BackendType: "ssh",
		Liveness:    LiveArchived,
		UserKilled:  true,
	})
	if err != nil {
		t.Fatalf("restore archived tombstone: %v", err)
	}
	if err := restored.Kill(); err != nil {
		t.Fatalf("already-reaped archive required a remote cleanup handle: %v", err)
	}
}
