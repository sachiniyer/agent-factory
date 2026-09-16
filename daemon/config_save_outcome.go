package daemon

import (
	"errors"
	"net/rpc"
	"strings"

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

// failedConfigApplyOutcome keeps a genuine reload failure distinct from both a
// lost RPC reply and a refusal that never reached the reload. net/rpc flattens
// every handler error into rpc.ServerError, so the message is the only signal:
// Manager.ApplyConfig's sole error return wraps config.LoadConfig as
// "reload config: …", and only that string proves the saved FILE did not load —
// evidence that outranks even a deferred key's next-start effect. An admission
// refusal (upgrade probation, quiescing) is also a ServerError but says nothing
// about whether the file loads; like a lost reply, it proves only that the
// client cannot tell whether the apply ran, so it is unconfirmed rather than
// failed.
func failedConfigApplyOutcome(err error) (config.ApplyOutcome, string) {
	var serverErr rpc.ServerError
	if errors.As(err, &serverErr) && strings.HasPrefix(string(serverErr), "reload config:") {
		return config.ApplyOutcome{DaemonApplyFailed: true}, "saved config, but live apply failed: " + err.Error()
	}
	return config.ApplyOutcome{DaemonApplyUnconfirmed: true}, "saved config, but live apply could not be confirmed: " + err.Error()
}

// readbackStore is which store a post-apply readback could consult. It is what
// decides what a divergence on a LIVE key proves.
type readbackStore int

const (
	// readbackSnapshot: the in-daemon handler reads the snapshot its apply just
	// swapped in, so a live key's divergence is a definite lost race.
	readbackSnapshot readbackStore = iota
	// readbackFileAfterApply: a version-skewed client can read only the file, and
	// only after the apply returned, which cannot establish which generation the
	// daemon loaded for a live key.
	readbackFileAfterApply
)

// recordSavedValueReadback is the ONE place a post-apply readback verdict
// becomes an outcome fact. Each save path used to compare the verdict against
// savedValueSuperseded at its own call site, so a verdict a path did not name —
// an unloadable file — was silently dropped on all four of them (#4247). Every
// verdict is decided here, once, for every path.
func recordSavedValueReadback(outcome *config.ApplyOutcome, warnings *[]string, key string, store readbackStore, verdict savedValueVerdict, loadErr error) {
	switch verdict {
	case savedValueConfirmed, savedValueUnresolved:
		// Either this save is confirmed, or the readback could not look. Neither
		// contradicts the apply, so its own answer stands.
	case savedValueFileUnreadable:
		// Only a key served FROM the file is harmed: its next daemon start or af
		// launch reads this same file. A live key's value is whatever the running
		// daemon already loaded, which a file breaking afterwards does not change,
		// and the apply's own answer already states what is known about it.
		if outcome.FileAuthoritative(key) {
			outcome.SavedFileUnreadable = true
			*warnings = append(*warnings, fileUnreadableWarning(key, loadErr))
		}
	case savedValueSuperseded:
		// The file decides for a file-authoritative key, and the snapshot decides
		// for a live key read inside the daemon. Either way the divergence is
		// definitive.
		if outcome.FileAuthoritative(key) || store == readbackSnapshot {
			outcome.SavedValueSuperseded = true
			return
		}
		// A live key read from the file after the apply returned: the file may
		// have moved after the daemon loaded this save, so this proves only that
		// the client cannot confirm what the daemon is serving.
		outcome.DaemonApplied = false
		outcome.DaemonApplyUnconfirmed = true
		*warnings = append(*warnings, unconfirmedReadbackWarning)
	}
}

// fileUnreadableWarning names the key whose next-start effect is lost and the
// reason the file will not load, so the operator knows what to fix.
func fileUnreadableWarning(key string, loadErr error) string {
	reason := "the file could not be read"
	if loadErr != nil {
		reason = loadErr.Error()
	}
	return "saved config, but the config file no longer loads, so " + key +
		" cannot take effect at its next start until the file is fixed: " + reason
}

// applyFallbackDiskVerdict is the version-skewed fallback's readback: the file is
// the only store such a daemon lets a client consult.
func applyFallbackDiskVerdict(outcome *config.ApplyOutcome, warnings *[]string, key, expected string) {
	verdict, loadErr := diskSavedValue(key, expected)
	recordSavedValueReadback(outcome, warnings, key, readbackFileAfterApply, verdict, loadErr)
}
