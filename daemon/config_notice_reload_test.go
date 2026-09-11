package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

func requireReloadFailureNotice(t *testing.T, notice string, warnings, applied []string) {
	t.Helper()
	require.Equal(t, "Saved — the running daemon could not apply the new configuration and is still using its previous value. Fix the reload error in the warning, then restart the daemon to apply the saved value.", notice)
	require.NotContains(t, notice, "no daemon")
	require.NotContains(t, notice, "using the new value now")
	require.Empty(t, applied)
	require.Contains(t, strings.Join(warnings, "\n"), "reload config:")
}

// Hold the existing apply mutex so the real save completes before a concurrent
// malformed edit makes the real reload fail. No production failure hook is needed.
func TestServerConfigSaveReportsReloadFailure(t *testing.T) {
	for _, unset := range []bool{false, true} {
		name := "set"
		if unset {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			m := applyConfigTestManager(t)
			initial := "false"
			if unset {
				initial = "true"
			}
			_, err := config.SetGlobalConfigValue("network.require_token", initial)
			require.NoError(t, err)
			_, err = m.ApplyConfig()
			require.NoError(t, err)
			before := m.Config()
			server := &controlServer{manager: m}
			dir, err := config.GetConfigDir()
			require.NoError(t, err)
			path := filepath.Join(dir, config.TomlConfigFileName)
			var setResp SetConfigValueResponse
			var unsetResp UnsetConfigValueResponse
			done := make(chan error, 1)
			m.configApplyMu.Lock()
			locked := true
			defer func() {
				if locked {
					m.configApplyMu.Unlock()
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("save handler did not finish")
				}
			}()
			go func() {
				if unset {
					done <- server.UnsetConfigValue(UnsetConfigValueRequest{Key: "network.require_token"}, &unsetResp)
				} else {
					done <- server.SetConfigValue(SetConfigValueRequest{Key: "network.require_token", Value: "true"}, &setResp)
				}
			}()
			require.Eventually(t, func() bool {
				data, readErr := os.ReadFile(path)
				return readErr == nil && !strings.Contains(string(data), "require_token = "+initial)
			}, 5*time.Second, time.Millisecond, "the save must reach disk before breaking reload")
			saved, err := os.ReadFile(path)
			require.NoError(t, err)
			if unset {
				require.NotContains(t, string(saved), "require_token")
			} else {
				require.Contains(t, string(saved), "require_token = true")
			}
			require.NoError(t, os.WriteFile(path, []byte("[broken"), 0600))
			m.configApplyMu.Unlock()
			locked = false
			select {
			case err = <-done:
				done <- err // leave completion available to the deferred cleanup
			case <-time.After(5 * time.Second):
				t.Fatal("save handler did not finish")
			}
			require.NoError(t, err, "the disk save succeeded even though live apply failed")
			require.Same(t, before, m.Config(), "failed reload must keep the previous live snapshot")
			require.Equal(t, unset, m.Config().RequireToken)
			if unset {
				require.NotNil(t, unsetResp.Result)
				require.True(t, unsetResp.Result.Removed)
				requireReloadFailureNotice(t, unsetResp.RestartNotice, unsetResp.Warnings, unsetResp.Applied)
			} else {
				require.NotNil(t, setResp.Result)
				requireReloadFailureNotice(t, setResp.RestartNotice, setResp.Warnings, setResp.Applied)
			}
		})
	}
}

type reloadFailureControl struct{}

func (*reloadFailureControl) ApplyConfig(_ ApplyConfigRequest, _ *ApplyConfigResponse) error {
	return errors.New("reload config: forced reload failure")
}

func TestClientFallbackConfigSaveReportsReloadFailure(t *testing.T) {
	for _, unset := range []bool{false, true} {
		name := "set"
		if unset {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			configClientHome(t)
			initial := "false"
			if unset {
				initial = "true"
			}
			_, err := config.SetGlobalConfigValue("network.require_token", initial)
			require.NoError(t, err)
			serveControlStub(t, &reloadFailureControl{})
			if unset {
				resp, err := UnsetGlobalConfigValue("network.require_token")
				require.NoError(t, err)
				require.NotNil(t, resp.Result)
				require.True(t, resp.Result.Removed)
				requireReloadFailureNotice(t, resp.RestartNotice, resp.Warnings, resp.Applied)
			} else {
				resp, err := SetGlobalConfigValue("network.require_token", "true")
				require.NoError(t, err)
				require.NotNil(t, resp.Result)
				requireReloadFailureNotice(t, resp.RestartNotice, resp.Warnings, resp.Applied)
			}
			cfg, err := config.LoadConfig()
			require.NoError(t, err)
			require.Equal(t, !unset, cfg.RequireToken, "the successful set/unset must remain on disk")
		})
	}
}
