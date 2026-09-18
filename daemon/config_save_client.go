package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// The CLIENT half of a global config save (#4247). It was split out of
// control_client.go, which had grown past the 1000-line limit: these functions
// are one coherent unit — the daemon-routed save, its version-skewed local
// fallback, and the apply poke that fallback pokes with — and they are the only
// callers in this package that must reason about a daemon too old to serve
// SetConfigValue. The server half lives in control_server.go and
// config_unset.go; the outcome rules both halves share are in
// config_save_outcome.go.

// requestApplyConfigAttempt asks a RUNNING daemon to apply the on-disk global
// config to itself in place (#2480). It deliberately never STARTS a daemon: a
// config write with no daemon running has nothing to apply live and takes
// effect on the next start, so this uses the no-ensure path and reports the
// dial failure when none is reachable. Preserving whether the RPC started lets
// save callers distinguish an unreachable daemon from a failed apply.
func requestApplyConfigAttempt() (ApplyConfigResponse, daemonCallAttempt) {
	var resp ApplyConfigResponse
	attempt := callDaemonNoEnsureAttemptBefore("ApplyConfig", ApplyConfigRequest{}, &resp, time.Time{}, false)
	return resp, attempt
}

// SetGlobalConfigValue writes one global config key through a running daemon's
// SetConfigValue — the same admission-gated handler the web form posts to — so
// every first-class save surface consults the same lifecycle predicate
// (upgrade probation, quiescing) and a refusal lands BEFORE anything reaches
// disk (#3231). The daemon applies an accepted write to itself in place and
// reports the per-key effect notice, so callers echo its answer rather than
// computing their own.
//
// With no daemon answering the control socket it falls back to the direct
// local write (config.SetGlobalConfigValue) — `af config set` must keep
// working with the daemon stopped. The fallback is decided by the DIAL, never
// by the daemon's answer: once a daemon has answered, an error (validation or
// admission refusal) is final, because writing locally after a refusal would
// reopen exactly the split #3231 closes. It never STARTS a daemon, like the
// apply poke.
func SetGlobalConfigValue(key, value string) (SetConfigValueResponse, error) {
	if err := config.RetiredThemeKeyError(key); err != nil {
		return SetConfigValueResponse{}, err
	}

	var resp SetConfigValueResponse
	if socketPath, err := DaemonSocketPath(); err == nil {
		if conn, dialErr := net.DialTimeout("unix", socketPath, daemonDialTimeout); dialErr == nil {
			client := rpc.NewClient(conn)
			defer client.Close()
			// The flat alias is the version-skew wire spelling: an older daemon's
			// SetConfigValue allowlist predates the grouped TOML name, while a new
			// daemon canonicalizes the same alias before writing. Normalize its
			// echo so new callers always see the canonical public key.
			wireKey := config.LegacyConfigKey(key)
			callErr := client.Call(controlServiceName+".SetConfigValue",
				SetConfigValueRequest{Key: wireKey, Value: value}, &resp)
			if callErr == nil && resp.Result != nil {
				resp.Result.Key = config.CanonicalConfigKey(resp.Result.Key)
			}
			if callErr == nil || !isRPCMethodMissing(callErr) {
				return resp, callErr
			}
			// A daemon old enough to lack SetConfigValue (pre-#1960) expects its
			// clients to write locally and poke ApplyConfig; fall through to that
			// sequence. Skew-only — delete once the oldest supported daemon
			// serves SetConfigValue.
		}
	}
	// A failed dial does not prove "no upgrade in flight": the hand-off leaves a
	// window with NO daemon on the socket — the quiescing daemon has exited and
	// the committed candidate has not bound yet — and a local write landing there
	// bypasses the very admission this function exists to consult, mutating
	// config after the candidate validated (and loaded) it. The durable upgrade
	// journal covers that window; consult it exactly as EnsureDaemon does, and
	// refuse the fallback only while a forward upgrade or rollback restore is
	// provably live (fail-open otherwise — a bad journal must not wedge
	// `af config set`, same doctrine as the launch path).
	if homeDir, ok := configHomeDir(); ok {
		switch decision, gateErr := checkUpgradeGate(homeDir, false); decision {
		case upgradeGateInProgress:
			return SetConfigValueResponse{}, fmt.Errorf(
				"config change refused during daemon upgrade handoff: %w; retry after the upgrade finishes", gateErr)
		case upgradeGateRestoringPrevious:
			return SetConfigValueResponse{}, fmt.Errorf(
				"config change refused while rollback restores the previous daemon: %w; retry after rollback recovery finishes", gateErr)
		}
	}
	result, err := config.SetGlobalConfigValue(key, value)
	if err != nil {
		return SetConfigValueResponse{}, err
	}
	resp = SetConfigValueResponse{Result: result}
	// Keep dial failure distinct from an RPC error: only the former means
	// no daemon was reached. A started RPC may have failed or lost its reply.
	applyResp, applyAttempt := requestApplyConfigAttempt()
	var outcome config.ApplyOutcome
	if applyAttempt.err == nil {
		resp.Applied = applyResp.Applied
		resp.Pending = applyResp.Pending
		resp.Warnings = applyResp.Warnings
		outcome = config.ApplyOutcome{DaemonApplied: true, FailedListenerKeys: applyResp.FailedListenerKeys}
		// The named skew rule (#4247). This fallback only runs against a daemon
		// too old to serve SetConfigValue, and ApplyConfigResponse carries no
		// digest, so nothing here can be matched against the local write above —
		// which is also why that write uses the plain writer rather than the
		// digest-returning one.
		applyDigestSkewUnconfirmed(&outcome, &resp.Warnings)
	} else if applyAttempt.requestStarted {
		var warning string
		outcome, warning = failedConfigApplyOutcome(applyAttempt.err)
		resp.Warnings = append(resp.Warnings, warning)
	}
	resp.Warnings = completeConfigSaveWarnings(outcome, result.Warnings, resp.Warnings)
	resp.ApplyOutcome = outcome.StatusForKey(result.Key)
	// The notice logic is no longer mirrored from controlServer.SetConfigValue — it
	// is the same code, in config.EffectNotice (#3397). Mirroring is what let the
	// unset surfaces be written without the socket-key branch at all; passing the
	// whole apply outcome leaves this surface nothing to get wrong.
	resp.RestartNotice = config.EffectNotice(result.Key, outcome)
	return resp, nil
}

// UnsetGlobalConfigValue routes a global alias removal through a running
// daemon's mutation-admission gate. With no daemon it performs the same locked
// local edit and best-effort live apply used by SetGlobalConfigValue.
func UnsetGlobalConfigValue(key string) (UnsetConfigValueResponse, error) {
	if err := config.RetiredThemeKeyError(key); err != nil {
		return UnsetConfigValueResponse{}, err
	}

	var resp UnsetConfigValueResponse
	if socketPath, err := DaemonSocketPath(); err == nil {
		if conn, dialErr := net.DialTimeout("unix", socketPath, daemonDialTimeout); dialErr == nil {
			client := rpc.NewClient(conn)
			defer client.Close()
			callErr := client.Call(controlServiceName+".UnsetConfigValue",
				UnsetConfigValueRequest{Key: key}, &resp)
			if callErr == nil || !isRPCMethodMissing(callErr) {
				return resp, callErr
			}
		}
	}
	if homeDir, ok := configHomeDir(); ok {
		switch decision, gateErr := checkUpgradeGate(homeDir, false); decision {
		case upgradeGateInProgress:
			return UnsetConfigValueResponse{}, fmt.Errorf(
				"config change refused during daemon upgrade handoff: %w; retry after the upgrade finishes", gateErr)
		case upgradeGateRestoringPrevious:
			return UnsetConfigValueResponse{}, fmt.Errorf(
				"config change refused while rollback restores the previous daemon: %w; retry after rollback recovery finishes", gateErr)
		}
	}
	result, err := config.UnsetGlobalConfigValue(key)
	if err != nil {
		return UnsetConfigValueResponse{}, err
	}
	resp.Result = result
	// The whole apply outcome, not just "the apply poke returned nil" (#3397): a
	// network.listen_addr / network.preview_listen_addr rebind that failed left the
	// OLD listener serving, and this surface used to report that as "Applied".
	// Keep dial failure distinct from an RPC error: only the former means
	// no daemon was reached. A started RPC may have failed or lost its reply.
	applyResp, applyAttempt := requestApplyConfigAttempt()
	var outcome config.ApplyOutcome
	if applyAttempt.err == nil {
		resp.Applied = applyResp.Applied
		resp.Pending = applyResp.Pending
		resp.Warnings = applyResp.Warnings
		outcome = config.ApplyOutcome{DaemonApplied: true, FailedListenerKeys: applyResp.FailedListenerKeys}
		// The same named skew rule as the set fallback (#4247).
		applyDigestSkewUnconfirmed(&outcome, &resp.Warnings)
	} else if applyAttempt.requestStarted {
		var warning string
		outcome, warning = failedConfigApplyOutcome(applyAttempt.err)
		resp.Warnings = append(resp.Warnings, warning)
	}
	resp.ApplyOutcome = outcome.StatusForKey(result.Key)
	resp.RestartNotice = config.EffectNotice(result.Key, outcome)
	return resp, nil
}

// isRPCMethodMissing reports whether a net/rpc call failed because the serving
// daemon does not register the method at all — the version-skew case, distinct
// from a handler that ran and refused. net/rpc flattens both into
// rpc.ServerError, so the stable "can't find" prefixes are the only signal.
func isRPCMethodMissing(err error) bool {
	var srv rpc.ServerError
	if !errors.As(err, &srv) {
		return false
	}
	return strings.HasPrefix(string(srv), "rpc: can't find method") ||
		strings.HasPrefix(string(srv), "rpc: can't find service")
}
