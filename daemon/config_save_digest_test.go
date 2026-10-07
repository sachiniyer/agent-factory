package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// The digest decides whether a save may report that the running daemon is
// serving the value it just wrote (#4247). These drive the real window it
// guards: the writer's file lock is released before Manager.ApplyConfig loads
// config.toml, so a competing write can land in between and be the file the
// apply carries.
//
// The window is opened by holding the apply mutex — the same technique the
// reload-failure tests use — so nothing here depends on a production hook or a
// timing hope.

// configSaveRace runs one save against a competing write that lands after the
// save's bytes reach disk and before the apply loads them. It returns the two
// responses and leaves the daemon applied.
func configSaveRace(t *testing.T, unset bool, compete func(t *testing.T, path string)) (SetConfigValueResponse, UnsetConfigValueResponse) {
	t.Helper()
	m := applyConfigTestManager(t)
	initial := "false"
	if unset {
		initial = "true"
	}
	_, err := config.SetGlobalConfigValue("network.require_token", initial)
	require.NoError(t, err)
	_, err = m.ApplyConfig()
	require.NoError(t, err)
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
	// The save writes under its own file lock and then blocks on the apply
	// mutex this test holds, which is precisely the gap the digest covers.
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(path)
		return readErr == nil && !strings.Contains(string(data), "require_token = "+initial)
	}, 5*time.Second, time.Millisecond, "the save must reach disk before the competing write")

	if compete != nil {
		compete(t, path)
	}

	m.configApplyMu.Unlock()
	locked = false
	select {
	case err = <-done:
		done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("save handler did not finish")
	}
	require.NoError(t, err, "the disk save succeeded regardless of what the apply loaded")
	return setResp, unsetResp
}

// The base case, and the control for everything below: an undisturbed save still
// reports `applied`. Without this the tests below would pass just as well
// against a mechanism that never confirms anything.
func TestServerConfigSaveConfirmsAnUndisturbedSave(t *testing.T) {
	for _, unset := range []bool{false, true} {
		name := "set"
		if unset {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			setResp, unsetResp := configSaveRace(t, unset, nil)
			notice, outcome, warnings := setResp.RestartNotice, setResp.ApplyOutcome, setResp.Warnings
			if unset {
				notice, outcome, warnings = unsetResp.RestartNotice, unsetResp.ApplyOutcome, unsetResp.Warnings
			}
			require.Equal(t, config.ApplyStatusApplied, outcome)
			require.Equal(t, "Applied — the running daemon is using the new value now.", notice)
			require.NotContains(t, strings.Join(warnings, "\n"), "could not be confirmed")
		})
	}
}

// A competing write to the SAME key is finding #8's misreport: the apply
// succeeds carrying the other writer's value, and the save used to report
// `applied` for its own. It now withholds that claim.
//
// A competing write that leaves this save's value alone is the documented
// over-report. The digest compares whole files, so it cannot tell the two apart —
// and reporting the second as unconfirmed too is the deliberate failure
// direction, because the error is always to withhold a claim rather than make a
// false one. A future change that "fixes" this case into `applied` is
// reintroducing the readback and the nine findings it produced (#4247).
func TestServerConfigSaveWithholdsTheClaimWhenTheFileMoved(t *testing.T) {
	// compete is chosen per (case, unset) rather than stored on the case, so one
	// subtest cannot rewrite what a later one runs.
	sameKey := func(unset bool) func(*testing.T, string) {
		return func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			if unset {
				// The unset removed both spellings, so there is no line to
				// replace. The competing write reinstates the key through the
				// flat alias, PREPENDED so it lands in root scope rather than
				// inside whichever table the file happens to end in.
				require.NotContains(t, string(data), "require_token", "premise: the unset removed the key")
				require.NoError(t, os.WriteFile(path, append([]byte("require_token = true\n"), data...), 0600))
				return
			}
			replaced := strings.Replace(string(data), "require_token = true", "require_token = false", 1)
			require.NotEqual(t, string(data), replaced, "premise: the save's own line was replaced")
			require.NoError(t, os.WriteFile(path, []byte(replaced), 0600))
		}
	}
	// A comment is the purest form of the over-report: it changes the file's
	// BYTES while changing no value at all, so the save's own key is provably
	// still the one the apply loaded — and the digest still withholds the claim,
	// because comparing whole files is what makes it cheap enough to be correct.
	untouchedKey := func(bool) func(*testing.T, string) {
		return func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append(data, []byte("\n# a competing writer's comment\n")...), 0600))
		}
	}
	cases := []struct {
		name    string
		compete func(unset bool) func(*testing.T, string)
	}{
		{name: "a competing write to the same key", compete: sameKey},
		{name: "a competing write that changes no value of this save", compete: untouchedKey},
	}
	for _, tc := range cases {
		for _, unset := range []bool{false, true} {
			name := tc.name + "/set"
			if unset {
				name = tc.name + "/unset"
			}
			t.Run(name, func(t *testing.T) {
				setResp, unsetResp := configSaveRace(t, unset, tc.compete(unset))
				notice, outcome, warnings := setResp.RestartNotice, setResp.ApplyOutcome, setResp.Warnings
				if unset {
					notice, outcome, warnings = unsetResp.RestartNotice, unsetResp.ApplyOutcome, unsetResp.Warnings
				}
				require.Equal(t, config.ApplyStatusUnconfirmed, outcome,
					"a live key whose apply loaded other bytes must not claim applied")
				require.NotContains(t, notice, "using the new value now")
				require.Contains(t, strings.Join(warnings, "\n"), digestMismatchWarning)
			})
		}
	}
}

// A DEFERRED key keeps its class sentence through the same mismatch. The file
// was written either way, and a race at save time is indistinguishable from a
// hand-edit a minute later — which no save could have reported either.
func TestServerConfigSaveKeepsADeferredKeysPromiseThroughAMismatch(t *testing.T) {
	m := applyConfigTestManager(t)
	server := &controlServer{manager: m}
	dir, err := config.GetConfigDir()
	require.NoError(t, err)
	path := filepath.Join(dir, config.TomlConfigFileName)

	var resp SetConfigValueResponse
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
		done <- server.SetConfigValue(SetConfigValueRequest{Key: "branch_prefix", Value: "mine/"}, &resp)
	}()
	require.Eventually(t, func() bool {
		data, readErr := os.ReadFile(path)
		return readErr == nil && strings.Contains(string(data), "mine/")
	}, 5*time.Second, time.Millisecond, "the save must reach disk before the competing write")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, []byte("\n# a competing writer's comment\n")...), 0600))
	m.configApplyMu.Unlock()
	locked = false
	select {
	case err = <-done:
		done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("save handler did not finish")
	}
	require.NoError(t, err)
	require.Equal(t, config.ApplyStatusDeferred, resp.ApplyOutcome)
	require.Contains(t, resp.RestartNotice, "takes effect on the next daemon start")
}

// applyOnlyControl is a daemon too old to serve SetConfigValue: net/rpc answers
// "can't find method", which is what routes the client to its version-skewed
// fallback. Its ApplyConfig succeeds and, like every pre-#1960 daemon, reports
// nothing about which bytes it loaded.
type applyOnlyControl struct{}

func (*applyOnlyControl) ApplyConfig(_ ApplyConfigRequest, resp *ApplyConfigResponse) error {
	resp.Applied = []string{"network.require_token"}
	return nil
}

// The named skew rule (#4247): with no digest to compare, the fallback reports
// `unconfirmed` for a live key rather than keeping the pre-digest `applied`,
// which would be a claim it cannot support. A deferred key is untouched, by the
// same rule as every other cause of that bit.
func TestClientFallbackAppliesTheDigestSkewRule(t *testing.T) {
	t.Run("live key is unconfirmed", func(t *testing.T) {
		configClientHome(t)
		serveControlStub(t, &applyOnlyControl{})

		resp, err := SetGlobalConfigValue("network.require_token", "true")
		require.NoError(t, err)
		require.NotNil(t, resp.Result)
		require.Equal(t, config.ApplyStatusUnconfirmed, resp.ApplyOutcome)
		require.NotContains(t, resp.RestartNotice, "using the new value now")
		require.Contains(t, strings.Join(resp.Warnings, "\n"), skewedApplyDigestWarning)

		cfg, err := config.LoadConfig()
		require.NoError(t, err)
		require.True(t, cfg.RequireToken, "the write still reached disk")
	})

	t.Run("deferred key keeps its class", func(t *testing.T) {
		configClientHome(t)
		serveControlStub(t, &applyOnlyControl{})

		resp, err := SetGlobalConfigValue("branch_prefix", "mine/")
		require.NoError(t, err)
		require.Equal(t, config.ApplyStatusDeferred, resp.ApplyOutcome)
		require.Contains(t, resp.RestartNotice, "takes effect on the next daemon start")
	})

	t.Run("unset of a live key is unconfirmed", func(t *testing.T) {
		configClientHome(t)
		_, err := config.SetGlobalConfigValue("network.require_token", "true")
		require.NoError(t, err)
		serveControlStub(t, &applyOnlyControl{})

		resp, err := UnsetGlobalConfigValue("network.require_token")
		require.NoError(t, err)
		require.NotNil(t, resp.Result)
		require.True(t, resp.Result.Removed)
		require.Equal(t, config.ApplyStatusUnconfirmed, resp.ApplyOutcome)
		require.Contains(t, strings.Join(resp.Warnings, "\n"), skewedApplyDigestWarning)
	})
}
