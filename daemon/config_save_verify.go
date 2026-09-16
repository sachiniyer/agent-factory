package daemon

import "github.com/sachiniyer/agent-factory/config"

// A save's apply outcome may claim "applied" only when the daemon is actually
// serving the value THIS save wrote. The file lock inside the writer releases
// before the apply loads config.toml, so a competing write can land in that gap
// and be the value the apply carries (#4247).
//
// The readback that checks this has three possible answers, not two. Collapsing
// "cannot tell" into "a competing value won" is what made a successful save of
// program_overrides.claude report a race that never happened, so the verdict is
// explicit here and each call site decides what an unverifiable readback means
// for its own surface.

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
	// savedValueUnverifiable: the readback could not resolve the key at all, so
	// neither of the other two may be claimed.
	savedValueUnverifiable
)

// liveSavedValue compares cfg's rendered value for key against the value the
// save wrote. CurrentSavedValue renders the same form `config set` accepts and
// SetResult.Value records (the round trip is pinned by
// TestCurrentValueRoundTripsThroughConfigSet), so a plain string compare is
// exact.
func liveSavedValue(cfg *config.Config, key, expected string) savedValueVerdict {
	live, ok := config.CurrentSavedValue(cfg, key)
	if !ok {
		return savedValueUnverifiable
	}
	if live != expected {
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

// diskSavedValue is the fallback counterpart of liveSavedValue: a client whose
// daemon is too old to serve SetConfigValue is also too old to serve GetConfig
// (both arrived together in #1960), so it cannot read the daemon's live config
// back and must read the file instead.
//
// That file read happens AFTER the apply RPC returned, which is why this can
// never report savedValueSuperseded. A competing write landing between the old
// daemon's load and this read leaves the daemon correctly serving THIS save
// while the file shows another value — the disk state at this instant is simply
// not evidence of which generation the daemon loaded. A divergence here is
// therefore reported as unverifiable, and the caller turns that into an
// explicitly unconfirmed apply rather than a definitive lost race.
func diskSavedValue(key, expected string) savedValueVerdict {
	cfg, err := config.LoadConfig()
	if err != nil {
		return savedValueUnverifiable
	}
	if liveSavedValue(cfg, key, expected) == savedValueConfirmed {
		return savedValueConfirmed
	}
	return savedValueUnverifiable
}
