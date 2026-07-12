package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeInRepoConfig drops a .agent-factory/config.json with the given fields at
// repoRoot, so config.ResolveConfig (and the runtime resolver above it) picks up
// a `backend` selection for the repo.
func writeInRepoConfig(t *testing.T, repoRoot string, fields map[string]any) {
	t.Helper()
	dir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0755))
	data, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.ConfigFileName), data, 0644))
}

// TestParseBackendKind pins the enum validation: empty defaults to local, the
// four canonical values round-trip, and anything else is a clear error (the
// validation the --backend flag and the config `backend` key both run).
func TestParseBackendKind(t *testing.T) {
	cases := []struct {
		in      string
		want    BackendKind
		wantErr bool
	}{
		{"", BackendLocal, false},
		{"local", BackendLocal, false},
		{"docker", BackendDocker, false},
		{"ssh", BackendSSH, false},
		{"hook", BackendHook, false},
		{"nope", "", true},
		{"Local", "", true}, // case-sensitive
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseBackendKind(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolveRuntime_RegistryMapsEveryBackend proves the registry maps each
// canonical backend to its Runtime and rejects an unknown one. This is the
// "registry maps backend→Runtime" gate.
func TestResolveRuntime_RegistryMapsEveryBackend(t *testing.T) {
	cases := []struct {
		kind BackendKind
		want Runtime
	}{
		{BackendLocal, localRuntime{}},
		{BackendHook, hookRuntime{}},
		{BackendDocker, dockerRuntime{}},
		{BackendSSH, sshRuntime{}},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			rt, err := ResolveRuntime(tc.kind)
			require.NoError(t, err)
			assert.IsType(t, tc.want, rt)
		})
	}

	if _, err := ResolveRuntime("kubernetes"); err == nil {
		t.Fatal("ResolveRuntime(unknown): want error, got nil")
	}
}

// TestLocalRuntimeProvision_Unchanged locks the local default: the local runtime
// provisions a bare LocalBackend and no remote endpoint — byte-identical to the
// pre-Phase-4 factory.
func TestLocalRuntimeProvision_Unchanged(t *testing.T) {
	res, err := localRuntime{}.Provision(ProvisionSpec{RepoRoot: t.TempDir()})
	require.NoError(t, err)
	if _, ok := res.Backend.(*LocalBackend); !ok {
		t.Fatalf("local runtime must provision a *LocalBackend, got %T", res.Backend)
	}
	assert.Nil(t, res.Endpoint, "an in-process runtime exposes no remote endpoint")
}

// TestDockerSSHRuntime_NotImplemented proves docker and ssh are registered but
// fail create with a clear, actionable error naming the PR that implements them
// — and that the error echoes the configured image/host (so the config sections
// are genuinely consumed).
func TestDockerSSHRuntime_NotImplemented(t *testing.T) {
	repoRoot := initTempGitRepo(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "docker",
		"docker":  map[string]any{"image": "my-runtime:latest"},
		"ssh":     map[string]any{"host": "build-box:2222"},
	})

	_, derr := dockerRuntime{}.Provision(ProvisionSpec{RepoRoot: repoRoot})
	require.Error(t, derr)
	assert.Contains(t, derr.Error(), "docker backend is not yet implemented")
	assert.Contains(t, derr.Error(), "PR4")
	assert.Contains(t, derr.Error(), "my-runtime:latest")

	_, serr := sshRuntime{}.Provision(ProvisionSpec{RepoRoot: repoRoot})
	require.Error(t, serr)
	assert.Contains(t, serr.Error(), "ssh backend is not yet implemented")
	assert.Contains(t, serr.Error(), "PR5")
	assert.Contains(t, serr.Error(), "build-box:2222")
}

// fakeRuntime is a Runtime that returns a fixed provision result — used to
// exercise the interface contract, including the remote-endpoint half that the
// real local/hook runtimes leave nil and docker/ssh don't reach yet.
type fakeRuntime struct {
	res ProvisionResult
	err error
}

func (f fakeRuntime) Provision(ProvisionSpec) (ProvisionResult, error) { return f.res, f.err }

// TestRuntimeContract_SurfacesEndpoint proves the Runtime contract carries a
// remote agent-server endpoint end-to-end: a runtime that provisions a sandbox
// returns its authed endpoint alongside the backend, which is exactly what the
// docker/ssh runtimes will fill in PR4/PR5.
func TestRuntimeContract_SurfacesEndpoint(t *testing.T) {
	ep := &AgentServerEndpoint{URL: "wss://127.0.0.1:9", Token: "tok", Fingerprint: validFingerprint}
	var rt Runtime = fakeRuntime{res: ProvisionResult{Backend: &LocalBackend{}, Endpoint: ep}}

	res, err := rt.Provision(ProvisionSpec{RepoRoot: t.TempDir()})
	require.NoError(t, err)
	require.NotNil(t, res.Backend)
	require.NotNil(t, res.Endpoint)
	assert.Equal(t, ep.URL, res.Endpoint.URL)
	assert.Equal(t, ep.Token, res.Endpoint.Token)
}

// TestResolveBackendKind_Precedence pins the selection precedence: an explicit
// --backend wins, then ForceRemote (the legacy hook selector), then the repo's
// `backend` config; a non-repo path and an unreadable config both fall back to
// local so a local create is never blocked here.
func TestResolveBackendKind_Precedence(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	// Explicit flag wins over everything (even ForceRemote).
	got, err := resolveBackendKind(InstanceOptions{Backend: BackendDocker, ForceRemote: true}, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, BackendDocker, got)

	// Invalid explicit flag errors.
	if _, err := resolveBackendKind(InstanceOptions{Backend: "nope"}, t.TempDir()); err == nil {
		t.Fatal("resolveBackendKind(invalid --backend): want error")
	}

	// ForceRemote selects hook when no explicit backend is given.
	got, err = resolveBackendKind(InstanceOptions{ForceRemote: true}, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, BackendHook, got)

	// A non-repo path with no explicit selection defaults to local.
	got, err = resolveBackendKind(InstanceOptions{}, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, BackendLocal, got)

	// The repo `backend` config key drives the default path.
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{"backend": "ssh", "ssh": map[string]any{"host": "h"}})
	got, err = resolveBackendKind(InstanceOptions{}, repoRoot)
	require.NoError(t, err)
	assert.Equal(t, BackendSSH, got)
}

// TestDefaultBackendFactory_ConfigSelection drives the full factory the way
// NewInstance does: local is byte-identical, a docker config fails cleanly with
// the not-implemented error, and a hook config still resolves to a working
// HookBackend (the "hook still works" gate).
func TestDefaultBackendFactory_ConfigSelection(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("local default is a plain LocalBackend", func(t *testing.T) {
		repoRoot := initTempGitRepo(t) // no in-repo config → local
		b, err := defaultBackendFactory(InstanceOptions{Title: "s"}, repoRoot)
		require.NoError(t, err)
		if _, ok := b.(*LocalBackend); !ok {
			t.Fatalf("local default must be *LocalBackend, got %T", b)
		}
	})

	t.Run("docker config fails not-implemented", func(t *testing.T) {
		repoRoot := initTempGitRepo(t)
		writeInRepoConfig(t, repoRoot, map[string]any{"backend": "docker", "docker": map[string]any{"image": "img"}})
		_, err := defaultBackendFactory(InstanceOptions{Title: "s"}, repoRoot)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not yet implemented")
	})

	t.Run("hook config still builds a HookBackend", func(t *testing.T) {
		repoRoot := initTempGitRepo(t)
		writeInRepoConfig(t, repoRoot, map[string]any{
			"backend": "hook",
			"remote_hooks": map[string]any{
				"launch_cmd": "echo",
				"attach_cmd": "echo",
				"delete_cmd": "echo",
			},
		})
		b, err := defaultBackendFactory(InstanceOptions{Title: "s"}, repoRoot)
		require.NoError(t, err)
		if _, ok := b.(*HookBackend); !ok {
			t.Fatalf("backend=hook must build a *HookBackend, got %T", b)
		}
	})

	t.Run("hook config without hooks errors cleanly", func(t *testing.T) {
		repoRoot := initTempGitRepo(t)
		writeInRepoConfig(t, repoRoot, map[string]any{"backend": "hook"})
		_, err := defaultBackendFactory(InstanceOptions{Title: "s"}, repoRoot)
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "remote hooks not configured"),
			"want 'remote hooks not configured', got %v", err)
	})
}
