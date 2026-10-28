package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/keys"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

func TestRemovedPRKeyBindingsLoadAndWarn(t *testing.T) {
	for _, action := range []string{"open_pr", "copy_pr"} {
		for _, value := range []string{`"P"`, `["P", "ctrl+p"]`} {
			t.Run(action+value, func(t *testing.T) {
				warnings := captureLog(t, &aflog.WarningLog)
				source := t.TempDir() + "/config.toml"
				t.Cleanup(func() { require.NoError(t, keys.ApplyOverrides(nil)) })
				for range 2 {
					cfg, err := parseConfigTOML([]byte(fmt.Sprintf("[keys]\n%s = %s\nquit = \"P\"\n", action, value)), source)
					require.NoError(t, err, "removed actions must not refuse startup or conflict with live bindings")
					require.NotContains(t, cfg.Keys, action)
					require.NotContains(t, cfg.KeymapOverrides(), action)
					require.Equal(t, []string{"P"}, cfg.KeymapOverrides()["quit"])
					require.NoError(t, keys.ApplyOverrides(cfg.KeymapOverrides()))
					require.Equal(t, keys.KeyQuit, keys.GlobalKeyStringsMap["P"])
				}
				require.Contains(t, warnings.String(), "keys."+action+" was removed; ignored")
				require.Equal(t, 1, strings.Count(warnings.String(), "was removed; ignored"), "warn once per source and removed action")
			})
		}
	}
}
