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
	match, ok := config.SavedValueMatches(cfg, key, expected)
	if !ok {
		return savedValueUnverifiable
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

// diskSavedValue is the fallback counterpart of liveSavedValue: a client whose
// daemon is too old to serve SetConfigValue is also too old to serve GetConfig
// (both arrived together in #1960), so it cannot read the daemon's live config
// back and must read the file instead.
//
// It reports what the FILE says and nothing more. What a divergence proves
// depends on the key, and only the caller knows that:
//
//   - For a live key the file read happens after the apply RPC returned, so it
//     cannot establish which generation the daemon loaded — a write landing
//     between the daemon's load and this read leaves the daemon correctly
//     serving THIS save. There, a divergence means "cannot confirm".
//   - For a key that takes effect at the next daemon start or af launch, the
//     file IS the thing that will be read, so a divergence means this save
//     genuinely will not take effect: definitively superseded.
//
// Collapsing those two into one answer here is what made a deferred key's lost
// race report as merely unconfirmed (#4247).
func diskSavedValue(key, expected string) savedValueVerdict {
	cfg, err := config.LoadConfig()
	if err != nil {
		return savedValueUnverifiable
	}
	return liveSavedValue(cfg, key, expected)
}

// deferredEffectKey reports whether key's value is consumed at the next daemon
// start or af launch rather than by the running daemon — the condition that
// makes a post-apply FILE read definitive for it.
func deferredEffectKey(key string) bool {
	switch config.KeyEffectClass(key) {
	case config.EffectNextDaemonStart, config.EffectNextAfLaunch:
		return true
	}
	return false
}
