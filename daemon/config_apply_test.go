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
