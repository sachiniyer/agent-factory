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

// preRefusalControl models a post-#3231 pre-#5137 daemon: it answers Ping and
// serves the admission-gated config writes, but its PingResponse predates the
// RefusesUnauthenticatedNetworkListener field, so it decodes false — and its
// writer has no listenerWriteRefusal, so it WOULD accept an exposure-capable
// write and bind the listener this build declines. setCalls/unsetCalls record
// whether the write was sent at all.
type preRefusalControl struct {
	setCalls   int
	unsetCalls int
	// capable flips the Ping answer to the #5137 build's: the write must then
	// route rather than refuse.
	capable bool
}

func (c *preRefusalControl) Ping(_ PingRequest, resp *PingResponse) error {
	resp.OK = true
	resp.Version = "1.0.200"
	resp.RefusesUnauthenticatedNetworkListener = c.capable
	return nil
}

func (c *preRefusalControl) SetConfigValue(req SetConfigValueRequest, resp *SetConfigValueResponse) error {
	c.setCalls++
	resp.Result = &config.SetResult{Key: req.Key, Value: req.Value}
	return nil
}

func (c *preRefusalControl) UnsetConfigValue(req UnsetConfigValueRequest, resp *UnsetConfigValueResponse) error {
	c.unsetCalls++
	resp.Result = &config.UnsetResult{Key: req.Key, Removed: true}
	return nil
}

// TestListenerWriteGateRefusesAPreRefusalDaemon is the local-socket half of the
// #5137 version-skew protection: EVERY write sent to a daemon whose Ping does
// not advertise the refusal capability is unsafe — the old writer ends in a
// whole-file ApplyConfig whose write→apply gap is not atomic, so even a
// safe-forcing key cannot promise the apply binds what the write intended.
// The client refuses before the write is sent, names the restart, and writes
// nothing.
func TestListenerWriteGateRefusesAPreRefusalDaemon(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*preRefusalControl) error
	}{
		{name: "set non-loopback listen_addr", call: func(c *preRefusalControl) error {
			_, err := SetGlobalConfigValue("network.listen_addr", "0.0.0.0:8443")
			return err
		}},
		{name: "set require_token false", call: func(c *preRefusalControl) error {
			_, err := SetGlobalConfigValue("network.require_token", "false")
			return err
		}},
		{name: "unset require_token", call: func(c *preRefusalControl) error {
			_, err := UnsetGlobalConfigValue("network.require_token")
			return err
		}},
		// The widened rule (#5137 review): an old daemon answers EVERY accepted
		// write with a whole-file ApplyConfig — an unrelated key on a file
		// already in the refused posture binds it just the same, and the
		// write→apply gap means the safe-forcing keys cannot promise their own
		// outcome either. Nothing is ungated.
		{name: "set an unrelated key", call: func(c *preRefusalControl) error {
			_, err := SetGlobalConfigValue("default_program", "codex")
			return err
		}},
		{name: "unset an unrelated key", call: func(c *preRefusalControl) error {
			_, err := UnsetGlobalConfigValue("network.preview_listen_addr")
			return err
		}},
		{name: "set require_token true", call: func(c *preRefusalControl) error {
			_, err := SetGlobalConfigValue("network.require_token", "true")
			return err
		}},
		{name: "set loopback listen_addr", call: func(c *preRefusalControl) error {
			_, err := SetGlobalConfigValue("network.listen_addr", "127.0.0.1:8443")
			return err
		}},
		{name: "unset listen_addr", call: func(c *preRefusalControl) error {
			_, err := UnsetGlobalConfigValue("network.listen_addr")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := configClientHome(t)
			stub := &preRefusalControl{}
			serveControlStub(t, stub)

			err := tc.call(stub)
			require.Error(t, err)
			require.Contains(t, err.Error(), "predates af's unauthenticated-listener refusal",
				"the refusal must name the policy the daemon lacks, got: %v", err)
			require.Contains(t, err.Error(), "af daemon restart",
				"the refusal must name the restart that arms the gate, got: %v", err)
			require.Equal(t, 0, stub.setCalls+stub.unsetCalls,
				"the write must never be sent to a daemon that cannot refuse it")
			require.NoFileExists(t, filepath.Join(home, config.TomlConfigFileName),
				"a refused write must not fall back to a local config write")
		})
	}
}

// TestListenerWriteGateRoutesToACapableDaemon is the same write to a daemon
// that DOES advertise the capability — it routes, because the daemon's own
// writer carries the refusal (and would refuse an unsafe resulting posture
// itself). No write routes to a pre-refusal daemon: the write→apply gap is
// not atomic on the old writer, so even the token coming ON could be swapped
// out of the file before the apply reads it — remediation there is a daemon
// restart, not a write.
func TestListenerWriteGateRoutesToACapableDaemon(t *testing.T) {
	t.Run("exposure-capable write to a refusal-capable daemon", func(t *testing.T) {
		configClientHome(t)
		stub := &preRefusalControl{capable: true}
		serveControlStub(t, stub)

		_, err := SetGlobalConfigValue("network.listen_addr", "0.0.0.0:8443")
		require.NoError(t, err, "a capable daemon gates the write itself — the client routes it")
		require.Equal(t, 1, stub.setCalls)
	})
	t.Run("safe write to a pre-refusal daemon refuses", func(t *testing.T) {
		configClientHome(t)
		stub := &preRefusalControl{}
		serveControlStub(t, stub)

		_, err := SetGlobalConfigValue("network.require_token", "true")
		require.Error(t, err, "even the remediation write is refused — the old daemon's apply could land a swapped file")
		require.Contains(t, err.Error(), "predates af's unauthenticated-listener refusal")
		require.Equal(t, 0, stub.setCalls)
	})
	t.Run("safe unset to a pre-refusal daemon refuses", func(t *testing.T) {
		configClientHome(t)
		stub := &preRefusalControl{}
		serveControlStub(t, stub)

		// Unsetting listen_addr restores the loopback default, but the old
		// daemon's whole-file apply could bind a hand edit instead.
		_, err := UnsetGlobalConfigValue("network.listen_addr")
		require.Error(t, err)
		require.Contains(t, err.Error(), "predates af's unauthenticated-listener refusal")
		require.Equal(t, 0, stub.unsetCalls)
	})
	t.Run("ping failure is not a refusal — the local writer still gates", func(t *testing.T) {
		configClientHome(t)
		// applyOnlyControl has no Ping method at all (pre-#1960): the gate must
		// not refuse on a transport failure — the write falls back to this
		// build's own config writer, which refuses the refused posture on disk.
		serveControlStub(t, &applyOnlyControl{})

		_, err := SetGlobalConfigValue("network.listen_addr", "0.0.0.0:8443")
		require.Error(t, err)
		require.Contains(t, err.Error(), "refusing to write",
			"the LOCAL refusal still gates when the daemon cannot answer, got: %v", err)
		require.NotContains(t, err.Error(), "predates",
			"a daemon that cannot answer Ping is not called stale — the shared refusal is the message")
	})
}
