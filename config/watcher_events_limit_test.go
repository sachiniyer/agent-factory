package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWatcherEventsPerMinuteConfigContract pins the public shape of the watch
// rate cap: it is discoverable, defaults to the historical 10/min behavior,
// accepts positive integers, and tells the operator that a daemon restart is
// required. A hidden constant could satisfy the runtime test while leaving the
// setting unavailable through af config set/get.
func TestWatcherEventsPerMinuteConfigContract(t *testing.T) {
	setupProvenanceTest(t, "schema_version = 1\n")

	var entry ManifestEntry
	var ok bool
	for _, candidate := range Manifest() {
		if candidate.Key == "watcher_events_per_minute" {
			entry, ok = candidate, true
			break
		}
	}
	require.True(t, ok, "watcher_events_per_minute must be discoverable in the config manifest")
	assert.Equal(t, "10", entry.Default)
	assert.True(t, entry.Settable)
	assert.Equal(t, EffectNextDaemonStart, KeyEffectClass(entry.Key))

	shown, ok := CurrentValue(DefaultConfig(), entry.Key)
	require.True(t, ok)
	assert.Equal(t, "10", shown)

	result, err := SetGlobalConfigValue(entry.Key, "25")
	require.NoError(t, err)
	assert.Equal(t, "25", result.Value)

	cfg, err := LoadConfig()
	require.NoError(t, err)
	shown, ok = CurrentValue(cfg, entry.Key)
	require.True(t, ok)
	assert.Equal(t, "25", shown)

	_, err = SetGlobalConfigValue(entry.Key, "0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a positive integer")
}

// TestWatcherEventsPerMinuteExplainNamesGlobalWinner pins the provenance path
// used by `af config get … --explain`: an explicitly configured rate must be
// attributed to the global file rather than presented as a built-in default.
func TestWatcherEventsPerMinuteExplainNamesGlobalWinner(t *testing.T) {
	setupProvenanceTest(t, "schema_version = 1\nwatcher_events_per_minute = 24\n")

	resolved, err := ResolveGlobalConfig()
	require.NoError(t, err)
	value := requireResolvedValue(t, resolved, "watcher_events_per_minute")
	require.NotNil(t, value.Winner)
	assert.Equal(t, SourceGlobal.String(), value.Winner.Layer)
	assert.Equal(t, 24, value.Value)
	assert.True(t, strings.HasSuffix(value.Winner.Path, TomlConfigFileName))
}

func TestWatcherEventsPerMinuteLoaderDefaultsInvalidHandEdits(t *testing.T) {
	for _, raw := range []int{0, -1} {
		cfg, err := parseLoadedConfigTOML(
			[]byte("schema_version = 1\nwatcher_events_per_minute = "+fmt.Sprint(raw)+"\n"),
			"config.toml", "config.toml",
		)
		require.NoError(t, err)
		assert.Equal(t, DefaultWatcherEventsPerMinute, cfg.WatcherEventsPerMinute)
	}
}
