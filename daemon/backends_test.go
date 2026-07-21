package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
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

// addOrigin gives a repo an `origin` remote. docker/ssh clone the workspace from
// origin, so a repo without one genuinely cannot use them — tests that expect those
// backends to be usable must look like a real repo.
func addOrigin(t *testing.T, repoRoot string) {
	t.Helper()
	require.NoError(t, exec.Command("git", "-C", repoRoot, "remote", "add", "origin", "https://example.invalid/repo.git").Run())
}

// stubLookPath makes every executable resolve, so a test's expectations do not
// depend on whether the CI image happens to ship a `docker` binary.
func stubLookPath(t *testing.T) {
	t.Helper()
	t.Cleanup(session.SetLookPathForTest(func(file string) (string, error) { return "/usr/bin/" + file, nil }))
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
		assert.NotEmptyf(t, opt.Label, "%s must carry a user-facing label", opt.Name)
	}
	assert.Equal(t, config.SupportedBackends, names, "the catalog is the canonical enum, in canonical order")
	// The RPC feeds the canonical enum straight into the catalog builder and does no
	// filtering of its own. This is the second half of the anti-drift pair (see
	// TestListBackends_NewBackendReachesClientsWithNoClientChange): that one proves
	// the builder passes any list through, this one proves the list it is given is
	// the canonical one. Together they say a new backend reaches every client.
	cfg, cfgErr := config.ResolveConfig(repo)
	assert.Equal(t, backendCatalog(config.SupportedBackends, cfg, cfgErr, repo), resp.Backends,
		"ListBackends must hand the canonical enum to backendCatalog untouched")

	assert.Equal(t, BackendAvailable, optionByName(t, resp, config.BackendLocal).Status, "local needs no repo config")
	assert.Empty(t, optionByName(t, resp, config.BackendLocal).Reason)

	// Unavailable, but present and explained — a client that hid these would leave
	// the user wondering where docker went; the useful answer is the reason.
	for _, name := range []string{config.BackendDocker, config.BackendSSH, config.BackendHook} {
		opt := optionByName(t, resp, name)
		assert.Equalf(t, BackendUnavailable, opt.Status, "%s is unconfigured in this repo and must not be offered as usable", name)
		assert.NotEmptyf(t, opt.Reason, "%s must carry an actionable reason, not just a status", name)
	}
	assert.Contains(t, optionByName(t, resp, config.BackendDocker).Reason, "docker.image")
	assert.Contains(t, optionByName(t, resp, config.BackendSSH).Reason, "ssh.host")
}

// TestListBackends_ReflectsRepoConfig proves the availability answer tracks the
// repo's actual config: set docker.image and ssh.host, and both become selectable
// with no reason to show.
func TestListBackends_ReflectsRepoConfig(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubLookPath(t)
	repo := setupControlRepo(t)
	addOrigin(t, repo)
	writeRepoBackendConfig(t, repo, map[string]any{
		"docker": map[string]any{"image": "my-runtime:latest"},
		"ssh":    map[string]any{"host": "build-box:2222"},
	})

	resp := listBackends(t, repo)

	for _, name := range []string{config.BackendLocal, config.BackendDocker, config.BackendSSH} {
		opt := optionByName(t, resp, name)
		assert.Equalf(t, BackendAvailable, opt.Status, "%s is configured in this repo and must be selectable", name)
		assert.Emptyf(t, opt.Reason, "%s is available, so there is nothing to explain", name)
	}
	// hook was not configured, so it stays unavailable — availability is per
	// backend, not a single "repo is configured" flag.
	assert.Equal(t, BackendUnavailable, optionByName(t, resp, config.BackendHook).Status)
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
		stubLookPath(t)
		repo := setupControlRepo(t)
		addOrigin(t, repo)
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
		assert.Equal(t, BackendUnavailable, optionByName(t, resp, config.BackendDocker).Status)
		assert.Contains(t, optionByName(t, resp, config.BackendDocker).Reason, "docker.image")
		// The DEFAULT itself must carry the problem, not just the docker row: a client
		// renders the default as its own choice and would otherwise show it as fine.
		assert.Equal(t, BackendUnavailable, resp.DefaultStatus)
		assert.Contains(t, resp.DefaultReason, "docker.image")
	})
}

// TestListBackends_InvalidConfiguredDefaultIsSurfaced is the regression for the
// review's finding 1, and the sharpest form of "a probe that cannot answer,
// answering anyway".
//
// A repo whose `backend` key names something unrecognized has NO default: such a
// create fails outright (session.resolveBackendKind: "misconfiguration should fail
// the create, not silently run local"). Reporting "local" there told a user with a
// broken config that everything was normal — and implied their sessions would land
// on local, a backend they never chose.
func TestListBackends_InvalidConfiguredDefaultIsSurfaced(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{"backend": "bogus"})

	resp := listBackends(t, repo)

	assert.NotEqual(t, config.BackendLocal, resp.Default, "reporting local here is a lie: this create fails, it does not fall back to local")
	assert.Empty(t, resp.Default, "there is no default to name when the configured one is not a backend")
	assert.Equal(t, BackendUnavailable, resp.DefaultStatus)
	assert.Contains(t, resp.DefaultReason, `"bogus"`, "name the offending value — 'unknown backend' alone leaves the user hunting")
	assert.Contains(t, resp.DefaultReason, config.InRepoConfigDirName, "name the file the bad value is in")
	assert.Contains(t, resp.DefaultReason, config.SupportedBackendsString(), "and say what the valid values are")

	// The real create must agree that this is broken — otherwise the catalog is
	// pessimistic rather than honest.
	_, err := session.BackendKindFor(session.InstanceOptions{}, repo)
	assert.Error(t, err, "precondition: a create with no explicit backend genuinely fails for this repo")
}

// TestListBackends_HookNeedsRunnableCommands is the regression for finding 2:
// availability must mean "I checked", not "it is configured". A hook whose command
// does not exist is the worst case in this class — the section IS present, so a
// config-only check calls it available, the user picks it, and the failure lands
// later, mid-provision, looking like something else.
func TestListBackends_HookNeedsRunnableCommands(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{
		"remote_hooks": map[string]any{
			"launch_cmd": "af-definitely-not-installed-launch",
			"delete_cmd": "af-definitely-not-installed-delete",
		},
	})

	resp := listBackends(t, repo)
	opt := optionByName(t, resp, config.BackendHook)

	assert.Equal(t, BackendUnavailable, opt.Status, "remote_hooks is fully configured, but the command does not exist — offering it would be a promise we cannot keep")
	assert.Contains(t, opt.Reason, "af-definitely-not-installed-launch", "name the command that is missing")
	assert.Contains(t, opt.Reason, "PATH", "and say why it could not be used, so the user can act")
}

// TestListBackends_HookAvailableWhenCommandsResolve is the other half: a hook whose
// commands DO resolve is offered. Without this, "always unavailable" would pass the
// test above.
func TestListBackends_HookAvailableWhenCommandsResolve(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubLookPath(t)
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{
		"remote_hooks": map[string]any{"launch_cmd": "af-launch", "delete_cmd": "af-delete"},
	})

	opt := optionByName(t, listBackends(t, repo), config.BackendHook)
	assert.Equal(t, BackendAvailable, opt.Status)
	assert.Empty(t, opt.Reason)
}

// TestListBackends_HookLabelNamesConfiguredLauncher is the regression for #2189:
// "hook" describes af's plumbing, not the remote sandbox a repo configured. The
// daemon owns the resolved repo config, so its wire response must carry the human
// label rather than making each client reverse-engineer launch_cmd independently.
func TestListBackends_HookLabelNamesConfiguredLauncher(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubLookPath(t)
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{
		"remote_hooks": map[string]any{
			"launch_cmd": "./infra/coder-launch.sh",
			"delete_cmd": "./infra/coder-delete.sh",
		},
	})

	// Decode the public JSON shape rather than reaching for a Go field that the
	// old response did not have. That lets this regression compile against the
	// broken implementation and fail on the actual missing wire contract.
	blob, err := json.Marshal(listBackends(t, repo))
	require.NoError(t, err)
	var wire struct {
		Backends []struct {
			Name  string `json:"name"`
			Label string `json:"label"`
		} `json:"backends"`
	}
	require.NoError(t, json.Unmarshal(blob, &wire))

	for _, opt := range wire.Backends {
		if opt.Name == config.BackendHook {
			assert.Equal(t, "Remote sandbox · coder-launch.sh (hook)", opt.Label)
			return
		}
	}
	t.Fatalf("backend %q missing from the catalog %+v", config.BackendHook, wire.Backends)
}

// TestListBackends_EmptyHookCommandIsNotAvailable: remote_hooks present but
// launch_cmd empty. RemoteHooks.Validate is what create runs, so reusing it here is
// what stops "configured" from ever again meaning merely "the section exists".
func TestListBackends_EmptyHookCommandIsNotAvailable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	stubLookPath(t)
	repo := setupControlRepo(t)
	writeRepoBackendConfig(t, repo, map[string]any{
		"remote_hooks": map[string]any{"launch_cmd": "", "delete_cmd": "af-delete"},
	})

	opt := optionByName(t, listBackends(t, repo), config.BackendHook)
	assert.Equal(t, BackendUnavailable, opt.Status)
	assert.Contains(t, opt.Reason, "launch_cmd")
}

// TestListBackends_UnreadableConfigIsUnknownNotUnavailable pins the third outcome.
//
// A config that will not parse means we cannot tell whether docker.image is set —
// docker.image might be perfectly fine, in a file with a stray comma elsewhere.
// Answering "requires docker.image" there would be a fabricated finding pointing at
// the wrong line; answering "available" would be a promise nobody checked. The only
// honest answer is "I could not check, and here is what stopped me".
func TestListBackends_UnreadableConfigIsUnknownNotUnavailable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupControlRepo(t)
	dir := filepath.Join(repo, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte("{ this is not json"), 0o644))

	resp := listBackends(t, repo)

	for _, name := range []string{config.BackendDocker, config.BackendSSH, config.BackendHook} {
		opt := optionByName(t, resp, name)
		assert.Equalf(t, BackendUnknown, opt.Status, "%s cannot be evaluated against a config that will not parse", name)
		assert.NotContainsf(t, opt.Reason, "requires", "%s must not invent a missing-key finding it never checked for", name)
		assert.NotEmptyf(t, opt.Reason, "%s must say what stopped the check", name)
	}

	// local reads no repo config, and a create with no explicit backend still runs
	// local through this failure — so calling it unknown would be its own lie, and
	// the user can still create a session while they fix the file.
	assert.Equal(t, BackendAvailable, optionByName(t, resp, config.BackendLocal).Status)
	assert.Equal(t, config.BackendLocal, resp.Default)
	assert.Equal(t, BackendAvailable, resp.DefaultStatus)
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

	require.NotContains(t, config.SupportedBackends, "fargate", "precondition: the fake backend must not be a real one")

	// A list carrying a backend this handler has never heard of, exactly as the
	// canonical enum would look the moment someone adds one — PASSED IN, not
	// assigned to config.SupportedBackends. See backendCatalog: that assignment is
	// a data race against session.ParseBackend's read, and it is the twin of the
	// tmux.SupportedPrograms race `go test -race` failed on (#1970/#2079).
	//
	// Registering a runtime is no longer needed either, which removes a second
	// global mutation: BackendUnusableReason switches on the KNOWN kinds and
	// consults no registry, so an unrecognized backend correctly reports no config
	// requirement — that is the property under test.
	cfg, cfgErr := config.ResolveConfig(repo)
	catalog := backendCatalog([]string{config.BackendLocal, "fargate"}, cfg, cfgErr, repo)

	names := make([]string, 0, len(catalog))
	for _, opt := range catalog {
		names = append(names, opt.Name)
	}
	assert.Equal(t, []string{config.BackendLocal, "fargate"}, names,
		"the catalog is the list it was given: same values, same order, nothing added or dropped")

	fargate := catalog[len(catalog)-1]
	assert.Equal(t, "fargate", fargate.Name, "a newly added backend lands in enum order, not appended by a special case")
	assert.Equal(t, "fargate", fargate.Label, "an unknown backend keeps its daemon-provided wire name as the label")
	assert.Equal(t, BackendAvailable, fargate.Status, "a backend with no repo-config requirement is selectable as soon as it is in the enum")
	assert.Empty(t, fargate.Reason)
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
