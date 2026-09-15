package daemon

import "github.com/sachiniyer/agent-factory/config"

// A save's apply outcome may claim "applied" only when the daemon is actually
// serving the value THIS save wrote. The file lock inside the writer releases
// before the apply loads config.toml, so a competing write can land in that gap
// and be the value the apply carries (#4247). Comparing the post-apply value —
// live on the server, on disk for the version-skewed fallback whose daemon
// predates the GetConfig readback — keeps a raced save from reporting
// "applied" for a value the daemon is not serving.

// liveValueDiverged reports whether cfg's rendered value for key differs from
// the value the save wrote. CurrentValue renders the same form `config set`
// accepts and SetResult.Value records (the round trip is pinned by
// TestCurrentValueRoundTripsThroughConfigSet), so a plain string compare is
// exact. A key that does not render cannot be confirmed as ours, so it counts
// as diverged.
func liveValueDiverged(cfg *config.Config, key, expected string) bool {
	live, ok := config.CurrentValue(cfg, key)
	return !ok || live != expected
}

// unsetExpectedValue is the live value a global unset saves: the key falls back
// to its default, so a diverged live value means a competing write won.
func unsetExpectedValue(key string) string {
	expected, _ := config.CurrentValue(config.DefaultConfig(), key)
	return expected
}

// diskValueDiverged is the fallback counterpart of liveValueDiverged: a client
// whose daemon is too old to serve SetConfigValue is also too old to serve
// GetConfig (both arrived together in #1960), so it cannot read the daemon's
// live config back. It checks the post-apply disk instead — the file the apply
// just loaded. A file that no longer loads or no longer holds the saved value
// proves this save's write is not what reached the daemon.
func diskValueDiverged(key, expected string) bool {
	cfg, err := config.LoadConfig()
	if err != nil {
		return true
	}
	return liveValueDiverged(cfg, key, expected)
}
