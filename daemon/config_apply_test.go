package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

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

// TestApplyConfigReportsNetworkKeysPending: the network listener keys cannot be
// applied in place in PR1 (they are served from the frozen startup config until
// the in-process listener reload), so a changed listen_addr is reported Pending —
// surfaced, never silently dropped.
func TestApplyConfigReportsNetworkKeysPending(t *testing.T) {
	m := applyConfigTestManager(t)

	_, err := config.SetGlobalConfigValue("listen_addr", "127.0.0.1:9999")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.Pending, "listen_addr", "a changed network key must be reported pending, not applied")
	require.NotContains(t, result.Applied, "listen_addr")
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
