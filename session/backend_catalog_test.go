package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// TestSupportedBackendsMatchesRuntimeRegistry is the drift guard behind #1933's
// "render the choices from what the daemon actually supports" contract. Two lists
// describe backends: config.SupportedBackends (what clients may OFFER) and
// runtimeRegistry (what the daemon can actually CONSTRUCT). Nothing in the type
// system ties them together, so this test does.
//
// Both directions are failures, and each is a real bug a contributor would
// otherwise ship silently:
//   - registered but not supported → a working backend the CLI's `--backend` help
//     omits and no client ever offers. That is precisely bug #1933, one backend
//     down.
//   - supported but not registered → every client offers a choice that fails at
//     create time with "unknown backend".
//
// Adding a backend means touching both lists; this test is what tells you the
// second one exists.
func TestSupportedBackendsMatchesRuntimeRegistry(t *testing.T) {
	registered := make(map[string]bool, len(runtimeRegistry))
	for kind := range runtimeRegistry {
		registered[string(kind)] = true
	}

	supported := make(map[string]bool, len(config.SupportedBackends))
	for _, name := range config.SupportedBackends {
		supported[name] = true
		assert.Truef(t, registered[name], "backend %q is in config.SupportedBackends but has no runtime registered: every client will offer it and every create will fail with \"unknown backend\" — register a runtime in runtimeRegistry or drop it from the enum", name)
	}

	for kind := range runtimeRegistry {
		assert.Truef(t, supported[string(kind)], "runtime %q is registered but missing from config.SupportedBackends: it works, but the --backend help omits it and no client (web included) will ever offer it — add it to the enum (#1933)", kind)
	}

	assert.Len(t, config.SupportedBackends, len(runtimeRegistry), "the backend enum and the runtime registry must describe the same set")
}

// TestSupportedBackendsAreParseable pins the third list that used to hand-repeat
// the enum: ParseBackendKind. Every offered backend must parse, or a client would
// faithfully render a choice the create path then rejects.
func TestSupportedBackendsAreParseable(t *testing.T) {
	for _, name := range config.SupportedBackends {
		kind, err := ParseBackendKind(name)
		require.NoErrorf(t, err, "backend %q is offered to users but ParseBackendKind rejects it", name)
		assert.Equal(t, BackendKind(name), kind)
	}

	// The empty value is the "no explicit choice" signal every client sends to get
	// the repo default — it must stay parseable and must NOT be a backend name.
	kind, err := ParseBackendKind("")
	require.NoError(t, err)
	assert.Equal(t, BackendLocal, kind)
}

// TestBackendConfigError_ReportsRepoConfigRequirements covers the choose-time
// answer #1933 needs: which backends a repo's config can actually satisfy, and an
// actionable reason naming the missing key for those it cannot.
func TestBackendConfigError_ReportsRepoConfigRequirements(t *testing.T) {
	t.Run("nil config leaves only local usable", func(t *testing.T) {
		assert.NoError(t, BackendConfigError(BackendLocal, nil), "local needs nothing from the repo config")

		for _, tc := range []struct {
			kind BackendKind
			key  string
		}{
			{BackendDocker, "docker.image"},
			{BackendSSH, "ssh.host"},
			{BackendHook, "remote_hooks"},
		} {
			err := BackendConfigError(tc.kind, nil)
			require.Errorf(t, err, "%s must report its unmet config requirement", tc.kind)
			assert.Containsf(t, err.Error(), tc.key, "the %s reason must name the missing key so the message is actionable", tc.kind)
		}
	})

	t.Run("configured backends report no error", func(t *testing.T) {
		cfg := &config.ResolvedConfig{
			Docker:      &config.DockerConfig{Image: "my-runtime:latest"},
			SSH:         &config.SSHConfig{Host: "build-box"},
			RemoteHooks: &config.RemoteHooks{},
		}
		assert.NoError(t, BackendConfigError(BackendDocker, cfg))
		assert.NoError(t, BackendConfigError(BackendSSH, cfg))
		assert.NoError(t, BackendConfigError(BackendHook, cfg))
	})

	t.Run("present but blank counts as unset", func(t *testing.T) {
		// A `docker = {}` / `ssh = { host = "  " }` section is the shape a user lands
		// on mid-setup. It must read as unset, not as configured-with-empty, or the
		// web would offer the backend and the create would fail anyway.
		cfg := &config.ResolvedConfig{
			Docker: &config.DockerConfig{Image: "   "},
			SSH:    &config.SSHConfig{Host: ""},
		}
		assert.Error(t, BackendConfigError(BackendDocker, cfg))
		assert.Error(t, BackendConfigError(BackendSSH, cfg))
	})
}

// TestBackendConfigError_IsTheMessageProvisionPrints is the anti-drift link for
// the REASON text. #1933 asks the web to state a missing-config requirement at
// choose time instead of letting the user discover it as a create failure — which
// is only an improvement if both surfaces say the same thing.
//
// Asserting equality (not "contains") is deliberate: it fails the moment someone
// reworded one path, which is exactly when the two would start disagreeing.
func TestBackendConfigError_IsTheMessageProvisionPrints(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("docker", func(t *testing.T) {
		repo := initTempGitRepo(t)
		writeInRepoConfig(t, repo, map[string]any{"backend": "docker", "docker": map[string]any{}})

		_, err := dockerRuntime{}.Provision(ProvisionSpec{RepoRoot: repo, Title: "s", CloneURL: "file:///x"})
		require.Error(t, err)
		assert.Equal(t, BackendConfigError(BackendDocker, nil).Error(), err.Error(),
			"the docker create-time error and the choose-time reason must be one string from one place")
	})

	t.Run("ssh", func(t *testing.T) {
		repo := initTempGitRepo(t)
		writeInRepoConfig(t, repo, map[string]any{"backend": "ssh", "ssh": map[string]any{}})

		_, err := sshRuntime{}.Provision(ProvisionSpec{RepoRoot: repo, Title: "s", CloneURL: "file:///x"})
		require.Error(t, err)
		assert.Equal(t, BackendConfigError(BackendSSH, nil).Error(), err.Error(),
			"the ssh create-time error and the choose-time reason must be one string from one place")
	})
}
