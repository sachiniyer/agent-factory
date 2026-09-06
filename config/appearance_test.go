package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppearanceMigrationAndWrite(t *testing.T) {
	for _, old := range []string{"auto", "system", "nord", "invalid", ""} {
		require.Equal(t, "system", NormalizeAppearance(old))
	}
	for _, value := range []string{"light", "dark", "system", "auto"} {
		t.Run(value, func(t *testing.T) {
			path := writeTempConfig(t, "appearance = '"+value+"'\n")
			cfg, err := LoadConfig()
			require.NoError(t, err)
			require.Equal(t, NormalizeAppearance(value), cfg.Appearance)
			_, err = SetGlobalConfigValue("appearance", "light")
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Contains(t, string(data), "appearance = 'light'")
			require.False(t, strings.Contains(string(data), "appearance = 'auto'"))
		})
	}
	require.Error(t, ValidateAppearance("auto"))
	require.Equal(t, EffectNextAfLaunch, KeyEffectClass("appearance"))
}
