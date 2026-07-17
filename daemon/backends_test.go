package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// ListBackends is a pure read: it resolves a repo's config and answers from the
// backend enum, never touching the manager or provisioning anything. A zero
// controlServer is therefore the whole fixture — no daemon, no tmux, no sessions.
func listBackends(t *testing.T, repoPath string) ListBackendsResponse {
	t.Helper()
	var resp ListBackendsResponse
	require.NoError(t, (&controlServer{}).ListBackends(ListBackendsRequest{RepoPath: repoPath}, &resp))
	return resp
}

// writeRepoBackendConfig writes an in-repo .agent-factory/config.json carrying the
// given keys — the file a user edits to opt a repo into docker/ssh/hook.
func writeRepoBackendConfig(t *testing.T, repoRoot string, cfg map[string]any) {
	t.Helper()
	dir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	blob, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), blob, 0o644))
}

func optionByName(t *testing.T, resp ListBackendsResponse, name string) BackendOption {
	t.Helper()
	for _, opt := range resp.Backends {
		if opt.Name == name {
			return opt
		}
	}
	t.Fatalf("backend %q missing from the catalog %+v", name, resp.Backends)
	return BackendOption{}
}

// TestListBackends_OffersEverySupportedBackend is the core of #1933: the daemon —
// not a hand-typed list in a client — decides what a create may select. A repo
// with no in-repo config gets the whole enum in canonical order, with local usable
// and the config-gated runtimes reported unavailable rather than hidden.
func TestListBackends_OffersEverySupportedBackend(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)

	resp := listBackends(t, repo)

	names := make([]string, 0, len(resp.Backends))
	for _, opt := range resp.Backends {
		names = append(names, opt.Name)
	}
	assert.Equal(t, config.SupportedBackends, names, "the catalog is the canonical enum, in canonical order")

	assert.True(t, optionByName(t, resp, config.BackendLocal).Available, "local needs no repo config")
	assert.Empty(t, optionByName(t, resp, config.BackendLocal).Reason)

	// Unavailable, but present and explained — a client that hid these would leave
	// the user wondering where docker went; the useful answer is the reason.
	for _, name := range []string{config.BackendDocker, config.BackendSSH, config.BackendHook} {
		opt := optionByName(t, resp, name)
		assert.Falsef(t, opt.Available, "%s is unconfigured in this repo and must not be offered as usable", name)
		assert.NotEmptyf(t, opt.Reason, "%s must carry an actionable reason, not just a disabled flag", name)
	}
	assert.Contains(t, optionByName(t, resp, config.BackendDocker).Reason, "docker.image")
	assert.Contains(t, optionByName(t, resp, config.BackendSSH).Reason, "ssh.host")
}

// TestListBackends_ReflectsRepoConfig proves the availability answer tracks the
// repo's actual config: set docker.image and ssh.host, and both become selectable
// with no reason to show.
func TestListBackends_ReflectsRepoConfig(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{
		"docker": map[string]any{"image": "my-runtime:latest"},
		"ssh":    map[string]any{"host": "build-box:2222"},
	})

	resp := listBackends(t, repo)

	for _, name := range []string{config.BackendLocal, config.BackendDocker, config.BackendSSH} {
		opt := optionByName(t, resp, name)
		assert.Truef(t, opt.Available, "%s is configured in this repo and must be selectable", name)
		assert.Emptyf(t, opt.Reason, "%s is available, so there is nothing to explain", name)
	}
	// hook was not configured, so it stays unavailable — availability is per
	// backend, not a single "repo is configured" flag.
	assert.False(t, optionByName(t, resp, config.BackendHook).Available)
}

// TestListBackends_DefaultMatchesTheCreatePath pins the contract the web relies on
// to avoid overriding a repo default: Default reports what a create with NO
// explicit backend resolves to. The web renders it as a label and sends nothing.
func TestListBackends_DefaultMatchesTheCreatePath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("no repo config defaults to local", func(t *testing.T) {
		repo := setupControlRepo(t)
		assert.Equal(t, config.BackendLocal, listBackends(t, repo).Default)
	})

	t.Run("repo backend config is the default", func(t *testing.T) {
		repo := setupControlRepo(t)
		writeRepoBackendConfig(t, repo, map[string]any{
			"backend": "docker",
			"docker":  map[string]any{"image": "my-runtime:latest"},
		})

		resp := listBackends(t, repo)
		assert.Equal(t, config.BackendDocker, resp.Default, "a repo that declares backend=docker defaults to docker")

		// The same decision function the real create path uses must agree — this is
		// the whole point of reporting a default rather than assuming local.
		kind, err := session.BackendKindFor(session.InstanceOptions{}, repo)
		require.NoError(t, err)
		assert.Equal(t, string(kind), resp.Default, "the reported default must be the backend a real create would pick")
	})

	t.Run("a default whose config is broken is still reported, with its reason", func(t *testing.T) {
		// backend=docker declared but docker.image missing. The default is honestly
		// "docker" (that IS what a create resolves to) AND docker is unavailable, so
		// a client can warn instead of silently offering a default that cannot run.
		repo := setupControlRepo(t)
		writeRepoBackendConfig(t, repo, map[string]any{"backend": "docker"})

		resp := listBackends(t, repo)
		assert.Equal(t, config.BackendDocker, resp.Default)
		assert.False(t, optionByName(t, resp, config.BackendDocker).Available)
		assert.Contains(t, optionByName(t, resp, config.BackendDocker).Reason, "docker.image")
	})
}

// TestListBackends_NewBackendReachesClientsWithNoClientChange is THE anti-drift
// test for #1933 (the daemon half; web/src/backends.test.ts is the other half).
//
// It registers a backend the way a real one is added — a runtime in the registry
// plus an entry in the canonical enum — and asserts it flows out of the RPC with
// no edit to this handler and, crucially, none to any client. If someone later
// reintroduces a hard-coded list inside ListBackends, this fails.
func TestListBackends_NewBackendReachesClientsWithNoClientChange(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)

	require.NotContains(t, config.SupportedBackends, "fargate", "precondition: the fake backend must not already exist")

	restoreRuntime := session.SetRuntimeForTest(session.BackendKind("fargate"), func() session.Runtime { return nil })
	t.Cleanup(restoreRuntime)

	prev := config.SupportedBackends
	config.SupportedBackends = append(append([]string{}, prev...), "fargate")
	t.Cleanup(func() { config.SupportedBackends = prev })

	resp := listBackends(t, repo)

	opt := optionByName(t, resp, "fargate")
	assert.True(t, opt.Available, "a backend with no repo-config requirement is selectable as soon as it is registered")
	assert.Empty(t, opt.Reason)
	assert.Equal(t, "fargate", resp.Backends[len(resp.Backends)-1].Name, "it lands in enum order, not appended by a special case")
}

// TestListBackends_RequiresARepo: availability and the default are both properties
// of a specific repo, so a path that is not a repo is an error rather than a
// silently local-only catalog.
func TestListBackends_RequiresARepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	var resp ListBackendsResponse
	err := (&controlServer{}).ListBackends(ListBackendsRequest{RepoPath: t.TempDir()}, &resp)
	assert.Error(t, err, "a non-repo path cannot answer which backends its sessions may use")
}
