package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// The file lock inside the writer releases before the apply loads config.toml,
// so a competing write can land in that gap and be the value the apply carries.
// A save whose own value never reached the daemon must not report "applied".

// TestServerConfigSaveReportsSupersededValue holds the apply mutex so the real
// save completes its write before a competing SetGlobalConfigValue lands; the
// apply then loads the competing value, and the response must not claim the
// saved one is live.
func TestServerConfigSaveReportsSupersededValue(t *testing.T) {
	for _, unset := range []bool{false, true} {
		name := "set"
		if unset {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			m := applyConfigTestManager(t)
			_, err := config.SetGlobalConfigValue("default_program", "codex")
			require.NoError(t, err)
			_, err = m.ApplyConfig()
			require.NoError(t, err)
			server := &controlServer{manager: m}
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
					done <- server.UnsetConfigValue(UnsetConfigValueRequest{Key: "default_program"}, &unsetResp)
				} else {
					done <- server.SetConfigValue(SetConfigValueRequest{Key: "default_program", Value: "aider"}, &setResp)
				}
			}()
			// The save's own write must be on disk before the race lands: an unset
			// restores the default, a set writes its value.
			require.Eventually(t, func() bool {
				cfg, lerr := config.LoadConfig()
				if lerr != nil {
					return false
				}
				v, _ := config.CurrentValue(cfg, "default_program")
				if unset {
					return v == "claude"
				}
				return v == "aider"
			}, 5*time.Second, time.Millisecond, "the save must reach disk before the competing write")
			_, err = config.SetGlobalConfigValue("default_program", "gemini")
			require.NoError(t, err)
			m.configApplyMu.Unlock()
			locked = false
			select {
			case err = <-done:
				done <- err // leave completion available to the deferred cleanup
			case <-time.After(5 * time.Second):
				t.Fatal("save handler did not finish")
			}
			require.NoError(t, err, "the disk save succeeded; only the reported live state is wrong")
			require.Equal(t, "gemini", m.Config().DefaultProgram,
				"the apply carried the competing write, not the save's value")
			if unset {
				require.Equal(t, config.ApplyStatusSuperseded, unsetResp.ApplyOutcome)
				require.NotContains(t, unsetResp.RestartNotice, "using the new value now")
			} else {
				require.Equal(t, config.ApplyStatusSuperseded, setResp.ApplyOutcome)
				require.NotContains(t, setResp.RestartNotice, "using the new value now")
			}
		})
	}
}

// supersedingControl models a daemon OLD enough not to serve SetConfigValue —
// so a client takes its local-write fallback and pokes ApplyConfig — while a
// competing write lands between the local save and the apply.
type supersedingControl struct {
	key   string
	value string
}

func (s *supersedingControl) ApplyConfig(_ ApplyConfigRequest, resp *ApplyConfigResponse) error {
	_, _ = config.SetGlobalConfigValue(s.key, s.value)
	resp.Applied = []string{s.key}
	return nil
}

// TestClientFallbackConfigSaveReportsSupersededValue: the fallback cannot read
// the old daemon's live config (GetConfig arrived with SetConfigValue in
// #1960), so it verifies the post-apply disk — the file the apply loaded.
func TestClientFallbackConfigSaveReportsSupersededValue(t *testing.T) {
	for _, unset := range []bool{false, true} {
		name := "set"
		if unset {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			configClientHome(t)
			_, err := config.SetGlobalConfigValue("default_program", "codex")
			require.NoError(t, err)
			serveControlStub(t, &supersedingControl{key: "default_program", value: "gemini"})

			if unset {
				resp, uerr := UnsetGlobalConfigValue("default_program")
				require.NoError(t, uerr)
				require.Equal(t, config.ApplyStatusSuperseded, resp.ApplyOutcome)
				require.NotContains(t, resp.RestartNotice, "using the new value now")
				return
			}
			resp, serr := SetGlobalConfigValue("default_program", "aider")
			require.NoError(t, serr)
			require.Equal(t, config.ApplyStatusSuperseded, resp.ApplyOutcome)
			require.NotContains(t, resp.RestartNotice, "using the new value now")
		})
	}
}
