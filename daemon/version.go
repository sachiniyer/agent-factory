package daemon

import "sync/atomic"

// buildVersion is the af version this process was built as, recorded by
// SetVersion at startup. It is the value a daemon reports back over Ping so
// clients can detect version skew (#1044): the daemon half of a skewed pair
// silently rejects fields a newer client sends (the HTTP handler decodes with
// DisallowUnknownFields), which surfaces to users as "unknown field <name>"
// and a hung UI rather than as an upgrade prompt.
//
// Stored atomically: SetVersion runs once on the main goroutine before the
// control server binds, while Ping reads it from per-connection RPC
// goroutines.
var buildVersion atomic.Value // string

// SetVersion records the af build version for this process. The version lives
// in package main, so the root command injects it here the same way it does
// for the TUI — the daemon package cannot import it.
func SetVersion(v string) { buildVersion.Store(v) }

// Version returns the version recorded by SetVersion, or "" when it was never
// set (a bare test binary, or a daemon built before version reporting existed).
func Version() string {
	v, _ := buildVersion.Load().(string)
	return v
}
