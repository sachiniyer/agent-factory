package daemon

import (
	"errors"
	"net/rpc"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
)

// How a save surface turns one apply attempt into the outcome it reports. The
// distinctions here are the whole point: a refusal, a lost reply, and an apply
// that loaded a file this save did not write are different states, and
// collapsing any two of them makes a save surface claim something it does not
// know (#4247).

// digestMismatchWarning is what a save says when the apply returned success but
// did not load the bytes this save committed. It states what is known — the
// daemon's reload and this write are not the same file — and stops there.
//
// It deliberately does NOT say the save lost a race for its own key. The digest
// covers the whole file, so an unrelated key changing in the same window reads
// identically, and asserting the stronger claim is exactly what the readback
// this replaced spent nine review rounds failing to earn.
const digestMismatchWarning = "saved config, but the daemon's live reload did not load the config this save wrote, " +
	"so the value the daemon is now serving could not be confirmed"

// skewedApplyDigestWarning is the version-skew rule's sentence. A daemon too old
// to serve SetConfigValue (pre-#1960) is served by the client fallback below,
// and reports nothing about which bytes its apply loaded, so no save on that
// path can ever be confirmed.
const skewedApplyDigestWarning = "saved config, but this daemon is too old to report which config its live apply loaded, " +
	"so the value the daemon is now serving could not be confirmed"

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
		return config.ApplyOutcome{DaemonApply: config.DaemonApplyFailed}, "saved config, but live apply failed: " + err.Error()
	}
	return config.ApplyOutcome{DaemonApply: config.DaemonApplyUnconfirmed}, "saved config, but live apply could not be confirmed: " + err.Error()
}

// confirmSavedConfigDigest is the ONE comparison that decides whether a
// successful apply may be reported as this save's value reaching the daemon.
//
// A save cannot claim `applied` merely because ApplyConfig returned nil: the
// writer's file lock is released before the apply loads config.toml, so a
// competing write — another client, or a hand-edit, which takes no lock at all —
// can land in that gap and be the value the apply carried. wrote is the digest
// of the bytes this save committed; loaded is the digest of the bytes that apply
// parsed. Equal means the daemon adopted exactly this file, and only then may
// the outcome's own answer stand.
//
// Anything else withholds the claim rather than making a different one, which is
// the failure direction this whole mechanism is chosen for. Two states reach it:
// a genuine mismatch, and an apply whose load never reached the canonical
// config.toml read (it materialized defaults or converted a legacy config.json,
// so it parsed no file this save could have written). Both mean "cannot
// confirm", which is what config.DaemonApplyUnconfirmed says — and which leaves
// a DEFERRED key's next-start promise standing, because the file was written
// either way. It replaces the applied answer outright: the apply ran, but which
// file it loaded is unproven, so no applied claim may survive beside it.
//
// The comparison is against a digest rather than a value on purpose: this
// function plus config.ConfigDigest is the entire mechanism, where the readback
// it replaced needed a store to re-read, a value normalisation, a generation
// identity, and a file-readability verdict.
func confirmSavedConfigDigest(outcome *config.ApplyOutcome, warnings *[]string, wrote, loaded config.ConfigDigest) {
	if loaded.Matches(wrote) {
		return
	}
	outcome.DaemonApply = config.DaemonApplyUnconfirmed
	*warnings = append(*warnings, digestMismatchWarning)
}

// applyDigestSkewUnconfirmed is the named skew rule for the version-skewed
// client fallback: a pre-#1960 daemon's ApplyConfig reports no digest, so a save
// on that path reports `unconfirmed` for a live key.
//
// It is a rule rather than an absent default, and the alternative was considered
// and rejected: keeping the pre-digest `applied` there would preserve a known
// misreport — the daemon may be serving a competing write — on the grounds that
// it only affects daemons older than #1960. Knowingly preserving a misreport
// because of who it reaches is how skew bugs become permanent. `unconfirmed` is
// the only answer this path can support.
//
// A DEFERRED key is unaffected, by the same rule as every other cause of this
// state: the file was written, so the next daemon start or af launch still reads
// this save's value (see config.DaemonApplyUnconfirmed).
func applyDigestSkewUnconfirmed(outcome *config.ApplyOutcome, warnings *[]string) {
	outcome.DaemonApply = config.DaemonApplyUnconfirmed
	*warnings = append(*warnings, skewedApplyDigestWarning)
}
