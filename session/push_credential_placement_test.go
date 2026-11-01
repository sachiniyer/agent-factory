package session

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
)

func TestPushCredentialPlacementReadsTheRecordedRuntime(t *testing.T) {
	for name, tc := range map[string]struct {
		backend Backend
		want    PushPlacement
		ok      bool
	}{
		"listed ssh host": {
			backend: &sshBackend{cleanup: &SSHRuntimeCleanupData{SessionDir: "/s", HostName: "box1",
				Config: config.SSHConfig{Host: "box1.example", User: "dev"}, CallbackRef: "ref-1"}},
			want: PushPlacement{ListedHost: "box1", Destination: config.SSHConfig{Host: "box1.example", User: "dev"}, CallbackRef: "ref-1",
				Machine: RuntimeMachine{Host: "box1.example", User: "dev"}}, ok: true,
		},
		"repo [ssh] target": {
			backend: &sshBackend{cleanup: &SSHRuntimeCleanupData{SessionDir: "/s"}},
		},
		"free-form sandbox": {
			backend: &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{SessionDir: "/s", SSHCommand: "ssh box", CallbackRef: "ref-2"}},
			want: PushPlacement{FreeFormSandbox: true, SandboxSSH: "ssh box", CallbackRef: "ref-2",
				Machine: RuntimeMachine{SandboxSSH: "ssh box"}}, ok: true,
		},
		"reaped ssh runtime":     {backend: &sshBackend{}},
		"reaped sandbox runtime": {backend: &sandboxBackend{}},
		"docker":                 {backend: &dockerBackend{cleanup: &DockerRuntimeCleanupData{ContainerID: "c", EngineID: "e"}}},
		"local":                  {backend: &LocalBackend{}},
	} {
		t.Run(name, func(t *testing.T) {
			inst := &Instance{Title: "t", backend: tc.backend}
			got, ok := inst.PushCredentialPlacement()
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPushCredentialOriginIsTheDaemonCheckoutsOrigin(t *testing.T) {
	t.Setenv("GH_HOST", "")
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-q")
	inst := &Instance{Title: "t", Path: repo}
	_, ok := inst.PushCredentialOrigin()
	assert.False(t, ok, "a checkout with no origin has nothing to answer for")

	git("remote", "add", "origin", "https://github.com/Owner/Repo.git")
	got, ok := inst.PushCredentialOrigin()
	require.True(t, ok)
	assert.Equal(t, pushcred.Origin{Host: "github.com", Path: "owner/repo"}, got)

	git("remote", "set-url", "origin", "git@github.com:owner/repo.git")
	_, ok = inst.PushCredentialOrigin()
	assert.False(t, ok, "an ssh origin authenticates with the host's own keys")
}
