package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// TestKeyDiffGapWatcherEventsPerMinute: watcher_events_per_minute is
// EffectNextDaemonStart (config/effect.go:100), so a change must be reported
// Pending. keyDiff omits it, so the bucketing loop never sees it and the
// change is silently dropped.
func TestKeyDiffGapWatcherEventsPerMinute(t *testing.T) {
	m := applyConfigTestManager(t)

	_, err := config.SetGlobalConfigValue("watcher_events_per_minute", "24")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.Pending, "watcher_events_per_minute",
		"watcher_events_per_minute is EffectNextDaemonStart; a change must be reported Pending, not silently dropped")
}

// TestKeyDiffGapUpgradeClearUnverifiableArtifacts: upgrade_clear_unverifiable_artifacts
// is EffectAppliedLive (config/effect.go:114), so a change must be reported Applied.
// keyDiff omits it, so the bucketing loop never sees it and the change is silently dropped.
func TestKeyDiffGapUpgradeClearUnverifiableArtifacts(t *testing.T) {
	m := applyConfigTestManager(t)

	_, err := config.SetGlobalConfigValue("upgrade_clear_unverifiable_artifacts", "true")
	require.NoError(t, err)

	result, err := m.ApplyConfig()
	require.NoError(t, err)
	require.Contains(t, result.Applied, "upgrade_clear_unverifiable_artifacts",
		"upgrade_clear_unverifiable_artifacts is EffectAppliedLive; a change must be reported Applied, not silently dropped")
}
