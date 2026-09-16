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
//
// The two cases need different keys: the save writes an applied-live scalar
// (default_program), while global unset only admits migrated alias keys
// (network.require_token).
func TestServerConfigSaveReportsSupersededValue(t *testing.T) {
	cases := []struct {
		name string
		// key is the saved key; unset drives the unset handler, otherwise set.
		unset bool
		key   string
		// seed primes disk and live config before the handler runs.
		seed string
		// saveValue is the set value (unset case: unused — the save removes key).
		saveValue string
		// savedOnDisk is what CurrentValue reports once the handler's write has
		// landed: the set value for a set, the default after an unset.
		savedOnDisk string
		// competing is the racing write that wins the apply.
		competing string
	}{
		{name: "set", key: "default_program", seed: "codex", saveValue: "aider", savedOnDisk: "aider", competing: "gemini"},
		{name: "unset", unset: true, key: "network.require_token", seed: "true", savedOnDisk: "false", competing: "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := applyConfigTestManager(t)
			_, err := config.SetGlobalConfigValue(tc.key, tc.seed)
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
				if tc.unset {
					done <- server.UnsetConfigValue(UnsetConfigValueRequest{Key: tc.key}, &unsetResp)
				} else {
					done <- server.SetConfigValue(SetConfigValueRequest{Key: tc.key, Value: tc.saveValue}, &setResp)
				}
			}()
			// The save's own write must be on disk before the race lands.
			require.Eventually(t, func() bool {
				cfg, lerr := config.LoadConfig()
				if lerr != nil {
					return false
				}
				v, _ := config.CurrentValue(cfg, tc.key)
				return v == tc.savedOnDisk
			}, 5*time.Second, time.Millisecond, "the save must reach disk before the competing write")
			_, err = config.SetGlobalConfigValue(tc.key, tc.competing)
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
			live, ok := config.CurrentValue(m.Config(), tc.key)
			require.True(t, ok)
			require.Equal(t, tc.competing, live,
				"the apply carried the competing write, not the save's value")
			if tc.unset {
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

// The fallback cannot read the old daemon's live config (GetConfig arrived with
// SetConfigValue in #1960), so the only readback it has is the post-apply disk.
//
// That read happens only after the apply RPC has
// returned. A competing write landing between the old daemon's load and that
// read leaves the daemon correctly serving THIS save while the file shows
// another value, so the disk state is not evidence of which generation the
// daemon loaded. The outcome is therefore unconfirmed rather than a definitive
// lost race (#4247) — but the load-bearing half is unchanged: this path must
// never claim the daemon is serving the value this save wrote.
func TestClientFallbackConfigSaveCannotConfirmADivergedValue(t *testing.T) {
	cases := []struct {
		name      string
		unset     bool
		key       string
		seed      string
		saveValue string
		competing string
	}{
		{name: "set", key: "default_program", seed: "codex", saveValue: "aider", competing: "gemini"},
		{name: "unset", unset: true, key: "network.require_token", seed: "true", competing: "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configClientHome(t)
			_, err := config.SetGlobalConfigValue(tc.key, tc.seed)
			require.NoError(t, err)
			serveControlStub(t, &supersedingControl{key: tc.key, value: tc.competing})

			if tc.unset {
				resp, uerr := UnsetGlobalConfigValue(tc.key)
				require.NoError(t, uerr)
				require.Equal(t, config.ApplyStatusUnconfirmed, resp.ApplyOutcome)
				require.NotContains(t, resp.RestartNotice, "using the new value now")
				require.Contains(t, resp.Warnings, unconfirmedReadbackWarning)
				return
			}
			resp, serr := SetGlobalConfigValue(tc.key, tc.saveValue)
			require.NoError(t, serr)
			require.Equal(t, config.ApplyStatusUnconfirmed, resp.ApplyOutcome)
			require.NotContains(t, resp.RestartNotice, "using the new value now")
			require.Contains(t, resp.Warnings, unconfirmedReadbackWarning)
		})
	}
}
