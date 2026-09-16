package daemon

import (
	"errors"
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

// The server-side readback must verify a deferred key against the FILE — the
// store the next daemon start or af launch actually reads — not the live
// snapshot the apply just swapped in. The snapshot cannot see a competing
// write that lands after the apply's own load, so a deferred key checked
// there promises a next-start effect the stored file will not deliver (#4247).
func TestAppliedSavedValueVerifiesDeferredKeyAgainstDisk(t *testing.T) {
	configClientHome(t)
	// Simulate the post-apply, post-competing-write state: the live snapshot
	// the apply swapped in still holds THIS save's values, while the file now
	// holds the competing write's.
	snapshot := config.DefaultConfig()
	snapshot.BranchPrefix = "mine"
	snapshot.DefaultProgram = "aider"
	_, err := config.SetGlobalConfigValue("branch_prefix", "winner")
	require.NoError(t, err)
	_, err = config.SetGlobalConfigValue("default_program", "gemini")
	require.NoError(t, err)

	// branch_prefix is consumed at the next daemon start: the file decides, so
	// the competing write reads as superseded even though the snapshot still
	// holds this save — the case the live-snapshot check could not see.
	require.Equal(t, savedValueSuperseded,
		appliedSavedValue(snapshot, config.ApplyOutcome{}, "branch_prefix", "mine"))

	// default_program is consumed by the running daemon: the snapshot decides,
	// so a write that landed after the apply does not contradict what the
	// daemon is serving.
	require.Equal(t, savedValueConfirmed,
		appliedSavedValue(snapshot, config.ApplyOutcome{}, "default_program", "aider"))
}

// A failed listener rebind defers the key dynamically: its static class is
// live, but the daemon keeps the old listener and the value only reaches one
// at the next start — which reads the file. A competing write landing between
// the apply's load and the readback must read as a lost race, not confirm
// against the snapshot that still holds this save (#4247).
func TestAppliedSavedValueVerifiesFailedListenerKeyAgainstDisk(t *testing.T) {
	configClientHome(t)
	// The snapshot the apply swapped in holds THIS save's address; the file
	// now holds the competing write's.
	snapshot := config.DefaultConfig()
	snapshot.ListenAddr = "127.0.0.1:8080"
	_, err := config.SetGlobalConfigValue("network.listen_addr", "127.0.0.1:9090")
	require.NoError(t, err)

	rebindFailed := config.ApplyOutcome{DaemonApplied: true, FailedListenerKeys: []string{"network.listen_addr"}}
	require.Equal(t, savedValueSuperseded,
		appliedSavedValue(snapshot, rebindFailed, "network.listen_addr", "127.0.0.1:8080"),
		"a failed-listener key is consumed from the file at next start, so the file decides")

	// The same key with a successful rebind stays live-checked: the snapshot
	// holds this save, so the post-apply write does not contradict it.
	require.Equal(t, savedValueConfirmed,
		appliedSavedValue(snapshot, config.ApplyOutcome{DaemonApplied: true}, "network.listen_addr", "127.0.0.1:8080"))
}

// The version-skewed fallback runs its definitive disk verdict on a successful
// apply; a FAILED-listener key needs the same treatment, because a diverged
// file means the deferred next-start effect will not happen — not merely an
// unconfirmable live value.
func TestApplyFallbackDiskVerdictReadsFailedListenerKeyFromDisk(t *testing.T) {
	configClientHome(t)
	_, err := config.SetGlobalConfigValue("network.listen_addr", "127.0.0.1:9090")
	require.NoError(t, err)

	outcome := config.ApplyOutcome{DaemonApplied: true, FailedListenerKeys: []string{"network.listen_addr"}}
	var warnings []string
	applyFallbackDiskVerdict(&outcome, &warnings, "network.listen_addr", "127.0.0.1:8080")
	require.True(t, outcome.SavedValueSuperseded,
		"a failed-listener key defers to the file, so a diverged file is a lost race")
	require.False(t, outcome.DaemonApplyUnconfirmed,
		"the live-key unconfirmed downgrade must not fire for a dynamically deferred key")
}

// refusingSupersedingControl models an old daemon whose ApplyConfig lands a
// competing write and then refuses the apply (upgrade admission, quiescing):
// the client sees an unconfirmed apply while the file already holds the
// competing value.
type refusingSupersedingControl struct {
	key   string
	value string
}

func (s *refusingSupersedingControl) ApplyConfig(_ ApplyConfigRequest, _ *ApplyConfigResponse) error {
	_, _ = config.SetGlobalConfigValue(s.key, s.value)
	return errors.New("apply refused during upgrade")
}

// An unconfirmed apply still promises a deferred key's next-start effect, but
// the file decides whether that promise holds — a competing write landing
// before the refused or lost apply returns leaves a different value stored.
// The unconfirmed branch must run the same definitive disk readback as the
// success branch, or it promises an effect the file will not deliver (#4247).
func TestClientFallbackUnconfirmedApplyVerifiesDeferredKeyOnDisk(t *testing.T) {
	configClientHome(t)
	_, err := config.SetGlobalConfigValue("branch_prefix", "orig")
	require.NoError(t, err)
	serveControlStub(t, &refusingSupersedingControl{key: "branch_prefix", value: "winner"})

	resp, err := SetGlobalConfigValue("branch_prefix", "mine")
	require.NoError(t, err)
	require.Equal(t, config.ApplyStatusSuperseded, resp.ApplyOutcome,
		"the competing write reached the file while the apply was unconfirmed")
	require.NotContains(t, resp.RestartNotice, "takes effect",
		"a lost race must not promise the save takes effect next start")
}
