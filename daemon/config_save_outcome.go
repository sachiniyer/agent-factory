package daemon

import (
	"errors"
	"net/rpc"

	"github.com/sachiniyer/agent-factory/config"
)

// How a save surface turns one apply attempt into the outcome it reports. The
// distinctions here are the whole point: a refusal, a lost reply, and a readback
// that cannot resolve its key are three different states, and collapsing any two
// of them makes a save surface claim something it does not know (#4247).

// unconfirmedReadbackWarning explains the one apply a version-skewed client can
// neither confirm nor call failed: the daemon accepted the apply, but predates
// the live-config readback, and the post-apply file no longer matches this
// save. The file may have moved after the daemon loaded this save's value, so
// this states what is known rather than asserting a lost race.
const unconfirmedReadbackWarning = "saved config, but this daemon is too old to report its live config back and the file no longer holds this save's value, so the value the daemon is serving could not be confirmed"

// failedConfigApplyOutcome keeps a daemon's explicit refusal distinct from a
// lost RPC reply. The former proves that the saved config was not applied; the
// latter proves only that the client cannot tell whether it was applied.
func failedConfigApplyOutcome(err error) (config.ApplyOutcome, string) {
	var serverErr rpc.ServerError
	if errors.As(err, &serverErr) {
		return config.ApplyOutcome{DaemonApplyFailed: true}, "saved config, but live apply failed: " + err.Error()
	}
	return config.ApplyOutcome{DaemonApplyUnconfirmed: true}, "saved config, but live apply could not be confirmed: " + err.Error()
}

// applyFallbackDiskVerdict folds the version-skewed fallback's post-apply FILE
// read into outcome. What a divergence proves depends on when the key is
// consumed, which is why the verdict is interpreted here rather than inside the
// readback:
//
//   - A deferred key: the file IS what the next daemon start or af launch will
//     read, so a divergence means this save will not take effect. Definitive,
//     and reported as superseded.
//   - A live key: the read happens after the apply RPC returned and cannot
//     establish which generation the daemon loaded, so a divergence means only
//     that the client cannot confirm what the daemon is serving.
//
// An unresolvable readback claims neither and leaves the apply's own answer
// standing.
func applyFallbackDiskVerdict(outcome *config.ApplyOutcome, warnings *[]string, key, expected string) {
	if diskSavedValue(key, expected) != savedValueSuperseded {
		return
	}
	if deferredEffectKey(key) {
		outcome.SavedValueSuperseded = true
		return
	}
	outcome.DaemonApplied = false
	outcome.DaemonApplyUnconfirmed = true
	*warnings = append(*warnings, unconfirmedReadbackWarning)
}
