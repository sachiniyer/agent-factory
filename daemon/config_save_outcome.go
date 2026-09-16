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
