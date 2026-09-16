package daemon

import "github.com/sachiniyer/agent-factory/config"

// A save's apply outcome may claim "applied" only when the daemon is actually
// serving the value THIS save wrote. The file lock inside the writer releases
// before the apply loads config.toml, so a competing write can land in that gap
// and be the value the apply carries (#4247).
//
// The readback that checks this has four possible answers, not two, and each
// means something different to the save that is reported. Collapsing any two is
// how this mechanism kept producing findings: "cannot tell" read as "a competing
// value won" made a successful program_overrides.claude save report a race that
// never happened, and "the file will not load" read as "cannot tell" let a
// deferred key promise an effect from a file nothing can load. The verdicts stay
// distinct here, and recordSavedValueReadback is the one place that decides
// what each means for the outcome.

// savedValueVerdict is what a post-apply readback can honestly say about whether
// the daemon ended up serving the value this save wrote.
type savedValueVerdict int

const (
	// savedValueConfirmed: the readback resolved the key and it holds this
	// save's value.
	savedValueConfirmed savedValueVerdict = iota
	// savedValueSuperseded: the readback resolved the key and a DIFFERENT value
	// holds it, so this save lost a race.
	savedValueSuperseded
	// savedValueUnresolved: the store loaded but the key did not render, so
	// neither of the answers above may be claimed. This says nothing about the
	// store itself.
	savedValueUnresolved
	// savedValueFileUnreadable: the config FILE did not load. For a key served
	// from that file this is not neutral: the next daemon start or af launch
	// reads the same file.
	savedValueFileUnreadable
)

// liveSavedValue compares cfg's rendered value for key against the value the
// save wrote. CurrentSavedValue renders the same form `config set` accepts and
// SetResult.Value records (the round trip is pinned by
// TestCurrentValueRoundTripsThroughConfigSet), so a plain string compare is
// exact. cfg is already loaded, so this never reports an unreadable file.
func liveSavedValue(cfg *config.Config, key, expected string) savedValueVerdict {
	match, ok := config.SavedValueMatches(cfg, key, expected)
	if !ok {
		return savedValueUnresolved
	}
	if !match {
		return savedValueSuperseded
	}
	return savedValueConfirmed
}

// unsetExpectedValue is the live value a global unset saves: the key falls back
// to its default, so a diverged live value means a competing write won.
func unsetExpectedValue(key string) string {
	expected, _ := config.CurrentSavedValue(config.DefaultConfig(), key)
	return expected
}

// diskSavedValue is the FILE counterpart of liveSavedValue, for the callers
// whose authoritative store is the file rather than the daemon's snapshot: a
// client whose daemon is too old to serve SetConfigValue is also too old to
// serve GetConfig (both arrived together in #1960), so it cannot read the
// daemon's live config back; and appliedSavedValue uses it for file-authoritative
// keys, whose next daemon start or af launch reads that same file.
//
// It reports what the FILE says and nothing more, returning the load error with
// savedValueFileUnreadable so the report can name what is wrong with the file.
// What any verdict proves depends on the key and on which store the caller could
// consult, and only recordSavedValueReadback decides that.
func diskSavedValue(key, expected string) (savedValueVerdict, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return savedValueFileUnreadable, err
	}
	return liveSavedValue(cfg, key, expected), nil
}

// appliedSavedValue is the in-daemon post-apply readback behind a successful
// ApplyConfig: it verifies the saved value against the store that will actually
// serve it (#4247).
//
//   - A key the running daemon consumes is checked against cfg — the live
//     snapshot the apply just swapped in. A divergence means the apply loaded
//     a competing write.
//   - A file-authoritative key (config.ApplyOutcome.FileAuthoritative: deferred
//     by class, or a listener whose rebind failed) is checked against the FILE,
//     because the file is what the next daemon start or af launch will read.
//     The live snapshot cannot see a competing write that lands after the
//     apply's own load, and it would still hold this save.
func appliedSavedValue(cfg *config.Config, outcome config.ApplyOutcome, key, expected string) (savedValueVerdict, error) {
	if outcome.FileAuthoritative(key) {
		return diskSavedValue(key, expected)
	}
	return liveSavedValue(cfg, key, expected), nil
}
