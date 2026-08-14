package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// applyConfigTestManager builds a manager whose live config is DefaultConfig and
// whose on-disk config lives under a throwaway AGENT_FACTORY_HOME, so a test can
// write a key with config.SetGlobalConfigValue and then ApplyConfig it.
func applyConfigTestManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	return m
}

// A plain `af` launch is the apply boundary for a hand-edited global theme. It
// must not apply an unrelated listener/auth/program edit as a side effect, and an
// already-open web client needs an events-plane signal to fetch the new palette.
func TestApplyThemeChangesOnlyThemeAndPublishes(t *testing.T) {
	m := applyConfigTestManager(t)
	_, events := m.events.subscribe()
	home := os.Getenv("AGENT_FACTORY_HOME")
	require.NotEmpty(t, home)
	require.NoError(t, os.WriteFile(filepath.Join(home, config.TomlConfigFileName), []byte(`schema_version = 1
default_program = "codex"
listen_addr = "127.0.0.1:9999"
theme = "zenburn"
`), 0o600))

	changed, err := m.ApplyTheme()
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, "zenburn", m.Config().Theme.Preset())
	assert.Equal(t, config.DefaultConfig().DefaultProgram, m.Config().DefaultProgram,
		"theme-only launch apply must not advance unrelated config")
	assert.Equal(t, config.DefaultConfig().ListenAddr, m.Config().ListenAddr,
		"theme-only launch apply must never attempt a listener rebind")

	select {
	case event := <-events:
		assert.Equal(t, agentproto.EventThemeChanged, event.Type)
	default:
		t.Fatal("theme apply did not notify connected event subscribers")
	}
}

// TestApplyConfigSwapsLiveConfig: a hot-reloadable key written to disk is live via
// Config() after ApplyConfig, with no restart, and is reported Applied.
func TestApplyConfigSwapsLiveConfig(t *testing.T) {
	m := applyConfigTestManager(t)
	require.Equal(t, config.DefaultConfig().DefaultProgram, m.Config().DefaultProgram)

	_, err := config.SetGlobalConfigValue("default_program", "codex")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Equal(t, "codex", m.Config().DefaultProgram, "Config() must reflect the saved value after ApplyConfig")
	require.Contains(t, result.Applied, "default_program")
	require.NotContains(t, result.Pending, "default_program")
}

// TestApplyConfigRebuildsLimitDetector is the guard for the silent-no-op the
// call-site audit caught: the limit detector is built ONCE at construction and
// reused across poll ticks, so a config swap alone would leave a saved
// limit_patterns override with no effect while ApplyConfig reported success.
// ApplyConfig must rebuild it in place.
func TestApplyConfigRebuildsLimitDetector(t *testing.T) {
	m := applyConfigTestManager(t)
	before := m.limitDetector.Load()

	_, err := config.SetGlobalConfigValue("limit_patterns.claude", "CUSTOM-LIMIT-BANNER")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.Applied, "limit_patterns")

	after := m.limitDetector.Load()
	require.NotSame(t, before, after, "ApplyConfig must rebuild the limit detector, not reuse the startup snapshot")
	require.Equal(t, "CUSTOM-LIMIT-BANNER", m.Config().LimitPatterns["claude"], "the rebuilt detector is built from the swapped config")
}

// TestApplyConfigReportsNetworkKeysApplied: since #2480 PR2 the network listener
// keys apply live — the auth/CORS keys per request (livePosture), listen_addr /
// preview_listen_addr by rebind — so a change is reported Applied, not Pending.
// (The live-read enforcement and the rebind/brick-prevention behavior are pinned
// with real listeners in listener_reload_test.go; this pins the classification the
// save-surface notice reads.)
func TestApplyConfigReportsNetworkKeysApplied(t *testing.T) {
	m := applyConfigTestManager(t)

	_, err := config.SetGlobalConfigValue("require_token", "true")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.Applied, "require_token", "a network key applies live since PR2, so it is reported applied, not pending")
	require.NotContains(t, result.Pending, "require_token")
}

func TestApplyConfigReportsCanonicalBackendSettingsApplied(t *testing.T) {
	m := applyConfigTestManager(t)
	for key, value := range map[string]string{
		"docker.mount_agent_credentials": "true",
		"ssh.host_key_verification":      "accept-new",
		"sandbox.ssh":                    "ssh sandbox.example",
	} {
		_, err := config.SetGlobalConfigValue(key, value)
		require.NoError(t, err)
	}

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	for _, key := range []string{
		"docker.mount_agent_credentials",
		"ssh.host_key_verification",
		"sandbox.ssh",
	} {
		require.Contains(t, result.Applied, key)
		require.NotContains(t, result.Pending, key)
	}
}

// TestApplyConfigReportsBranchPrefixPending is the regression for the lie the
// per-key notice rework caught: branch_prefix is read from the FROZEN startup
// config in the title-reservation helpers (manager_create.go, deliberately not
// threaded live), so a change does NOT hot-reload — yet it was reported Applied,
// telling the user "the daemon is using it now" while the daemon kept deriving
// branches from the old prefix. It must be Pending.
func TestApplyConfigReportsBranchPrefixPending(t *testing.T) {
	m := applyConfigTestManager(t)

	_, err := config.SetGlobalConfigValue("branch_prefix", "test-branch/")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.Pending, "branch_prefix",
		"branch_prefix is read from the frozen startup config, so a change waits for the next daemon start")
	require.NotContains(t, result.Applied, "branch_prefix",
		"reporting branch_prefix applied claims a change is live while the daemon still uses the old prefix")
}

// TestApplyBucketsAgreeWithEffectClasses pins the invariant that keeps the daemon
// and the save-surface notice (config.EffectNotice) from disagreeing: every key
// the daemon diffs is classified applied-live or next-daemon-start — never
// client-side, never unclassified — so ApplyConfig can never file a key into a
// bucket the notice would describe a different way.
func TestApplyBucketsAgreeWithEffectClasses(t *testing.T) {
	for key := range keyDiff {
		switch config.KeyEffectClass(key) {
		case config.EffectAppliedLive, config.EffectNextDaemonStart:
			// ok: these are the only two buckets ApplyConfig files into.
		default:
			t.Errorf("keyDiff key %q has effect class %v; a diffed key must be applied-live or next-daemon-start",
				key, config.KeyEffectClass(key))
		}
	}
}

// Turning require_token OFF must not leave sandbox callback credentials behind
// (#2999, #3012 review).
//
// mintSandboxCallback refuses to ISSUE one while require_token is false, because a
// scoped credential against a listener that authenticates nobody enforces nothing.
// That check runs once, at provision time, and auth keys apply live with no rebind
// — so without this, relaxing the key silently converts every already-issued
// credential into exactly what the refusal exists to prevent.
func TestApplyConfig_DisablingRequireTokenRevokesSandboxCredentials(t *testing.T) {
	m := applyConfigTestManager(t)

	_, err := config.SetGlobalConfigValue("require_token", "true")
	require.NoError(t, err)
	_, err = m.ApplyConfig()
	require.NoError(t, err)
	require.True(t, m.Config().RequireToken, "the premise of this test is that the key starts enabled")

	secret, err := m.sandboxTokens.mint("sess-a")
	require.NoError(t, err)
	_, ok := m.sandboxTokens.sessionFor(secret)
	require.True(t, ok, "anti-vacuous: the credential must be live before the flip, or the assertion below proves nothing")

	_, err = config.SetGlobalConfigValue("require_token", "false")
	require.NoError(t, err)
	result, err := m.ApplyConfig()
	require.NoError(t, err)

	_, ok = m.sandboxTokens.sessionFor(secret)
	assert.False(t, ok, "a credential issued under require_token=true must not outlive it: the gate short-circuits on the tokenless posture before consulting the registry, so its scope stops being enforced")

	// And the operator must be TOLD, including that this does not re-isolate the
	// sandboxes — revoking without saying so would read as a security action.
	joined := strings.Join(result.Warnings, "\n")
	assert.Contains(t, joined, "require_token is now false")
	assert.Contains(t, joined, "does not re-isolate")
}

// The converse: an unrelated config change must leave credentials alone, or every
// save would silently sever every sandbox's callback.
func TestApplyConfig_UnrelatedChangeKeepsSandboxCredentials(t *testing.T) {
	m := applyConfigTestManager(t)
	_, err := config.SetGlobalConfigValue("require_token", "true")
	require.NoError(t, err)
	_, err = m.ApplyConfig()
	require.NoError(t, err)

	secret, err := m.sandboxTokens.mint("sess-a")
	require.NoError(t, err)

	_, err = config.SetGlobalConfigValue("default_program", "codex")
	require.NoError(t, err)
	_, err = m.ApplyConfig()
	require.NoError(t, err)

	owner, ok := m.sandboxTokens.sessionFor(secret)
	assert.True(t, ok, "an unrelated save must not revoke callback credentials")
	assert.Equal(t, "sess-a", owner)
}
